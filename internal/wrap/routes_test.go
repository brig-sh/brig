package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// fakeBroker is a routeServer that records what a run asked of it.
type fakeBroker struct {
	registered []injector.Sandbox
	minted     []injector.MintRequest
	forgotten  []string
	running    bool
}

const fakeBrokerPort = 47821

func (f *fakeBroker) Register(s injector.Sandbox) (int, error) {
	f.registered = append(f.registered, s)
	f.running = true
	return fakeBrokerPort, nil
}

func (f *fakeBroker) Mint(r injector.MintRequest) (string, error) {
	f.minted = append(f.minted, r)
	return "session-token-" + r.Sandbox + "-" + r.Route, nil
}

func (f *fakeBroker) Forget(id string) error {
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeBroker) Status() (injector.Status, bool) {
	return injector.Status{PID: 4242, Port: fakeBrokerPort}, f.running
}

// claudeAndGitHub binds claude-code to the Claude route and the GitHub route.
const claudeAndGitHub = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: anthropic
      preset: claude-code
      credential: {secretRef: {name: claude-credentials}}
    - name: github
      preset: github-token
      credential: {secretRef: {name: gh-token}}
`

// routedConfig loads claude-code bound to doc, on rt, with every directory
// brig writes in a fresh temporary one.
func routedConfig(t *testing.T, doc string, rt runtime.Runtime) *Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("BRIG_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("BRIG_POLICY_DIR", filepath.Join(dir, "policies"))
	t.Setenv("GH_TOKEN", "ambient-gh-token-value")
	if err := os.MkdirAll(filepath.Join(dir, "policies"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policies", "brokered.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Policy = []string{"brokered"}
	c, err := Load(p, Options{}, rt)
	if err != nil {
		t.Fatal(err)
	}
	c.Err, c.Out = &bytes.Buffer{}, &bytes.Buffer{}
	// The store holds every routed secret, so the run goes on to the broker.
	// The host's own store is never opened.
	held := fakeSecretStore{}
	for _, name := range c.routeSkipped {
		held[name] = "{}"
	}
	c.OpenStore = func() (creds.SecretReader, error) { return held, nil }
	return c
}

// A secret a route injects stays on the host: its files: binding and every
// env binding that names it are dropped, and brig does not read it. The
// tmpfs that held the file stays, and CLAUDE_CONFIG_DIR with it.
func TestABrokeredSecretIsNeverDelivered(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	routes, _, err := routesOf([]policy.Policy{mustPolicy(t, claudeAndGitHub)})
	if err != nil {
		t.Fatal(err)
	}
	got, bindings, skipped := withRoutes(p, p.Env, routes)
	if !slices.Equal(skipped, []string{"claude-credentials", "gh-token"}) {
		t.Errorf("skipped %v", skipped)
	}
	if len(got.Files) != 0 {
		t.Errorf("files still delivered: %+v", got.Files)
	}
	for _, d := range got.Secrets {
		if d.Name == "claude-credentials" || d.Name == "gh-token" {
			t.Errorf("brig still reads %s", d.Name)
		}
	}
	for _, b := range append(slices.Clone(got.Env), bindings...) {
		if b.Name == "GH_TOKEN" {
			t.Errorf("GH_TOKEN is still bound: %+v", b)
		}
	}
	dir := ""
	for _, b := range bindings {
		if b.Name == "CLAUDE_CONFIG_DIR" {
			dir = b.Value
		}
	}
	if len(got.Tmpfs()) != 1 || dir != "/brig/claude" {
		t.Errorf("a microVM lost its tmpfs (%d) or its config dir %q", len(got.Tmpfs()), dir)
	}
	// The profile the caller holds is not changed.
	if len(p.Files) == 0 || len(p.Volumes) == 0 {
		t.Error("withRoutes changed the caller's profile")
	}
}

func mustPolicy(t *testing.T, doc string) policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Stopping or removing a sandbox revokes its tokens, whether or not the run
// that stops it still has the policy.
func TestStopAndRemoveRevokeTheSandboxTokens(t *testing.T) {
	rec := &livenessRuntime{}
	c := routedConfig(t, claudeAndGitHub, rec)
	broker := &fakeBroker{}
	c.broker = broker
	c.routes = nil
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = c.Remove()
	if !slices.Equal(broker.forgotten, []string{c.VMName, c.VMName}) {
		t.Errorf("forgotten %v, want the sandbox on stop and on rm", broker.forgotten)
	}
}

// The envelope names the broker, its transport and the routes it serves, and
// the credentials row names each session token by its route, never a value.
// No runtime routes brig.internal yet, so the row says the run is refused,
// and why.
func TestEnvelopeHasABrokerRow(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, fakeRuntime{})
	c.broker = &fakeBroker{running: true}
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	got := block.String()
	for _, want := range []string{
		"BROKER       transport brig.internal, routes anthropic, github; " +
			"refused: brig.internal routing is not available on this runtime yet; " +
			"kept on the host: claude-credentials, gh-token",
		"CLAUDE_CODE_OAUTH_TOKEN(route: anthropic)",
		"BRIG_ROUTE_GITHUB_TOKEN(route: github)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not carry %q:\n%s", want, got)
		}
	}
	d := c.InfoData(creds.Set{})
	if d.Broker == nil || d.Broker.Transport != "brig.internal" || d.Broker.Running ||
		!strings.Contains(d.Broker.Refused, "brig.internal routing is not available") {
		t.Errorf("info --json broker = %+v", d.Broker)
	}
	var broker []InfoCredential
	for _, cr := range d.Credentials {
		if cr.Source == "broker" {
			broker = append(broker, cr)
		}
	}
	if len(broker) != 2 || broker[0].Route != "anthropic" || broker[1].Route != "github" {
		t.Errorf("info --json credentials from the broker = %+v", broker)
	}
}

// The BROKER row names every secret a route injects, also one the profile
// never delivers, such as the key of an anthropic-api-key route.
func TestTheBrokerRowNamesEveryRoutedSecret(t *testing.T) {
	const apiKey = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: anthropic
      preset: anthropic-api-key
      credential: {secretRef: {name: brigtest-key}}
`
	c := routedConfig(t, apiKey, fakeRuntime{})
	c.broker = &fakeBroker{running: true}
	if got := c.brokerLine(); !strings.Contains(got, "; kept on the host: brigtest-key") {
		t.Errorf("the BROKER row is %q", got)
	}
	d := c.InfoData(creds.Set{})
	if d.Broker == nil || !slices.Equal(d.Broker.KeptOnHost, []string{"brigtest-key"}) {
		t.Errorf("info --json broker = %+v", d.Broker)
	}
}

// A guest booted after another one is a new guest. The one before it was
// stopped through the runtime, and the broker still holds its registration,
// so the run removes it before the boot: the same registration again would
// keep the old guest's tokens valid, and a run whose policy was detached
// registers nothing that would replace it. A first boot clears a
// registration a guest that died without a stop left behind.
func TestABootForgetsTheGuestBeforeIt(t *testing.T) {
	for name, running := range map[string]bool{"restart": true, "first boot": false} {
		t.Run(name, func(t *testing.T) {
			rt := &livenessRuntime{running: running}
			c := livenessConfig(t, rt)
			// The running guest no longer mounts this workspace, so brig
			// restarts it.
			rt.workspace = t.TempDir()
			broker := &fakeBroker{}
			c.broker = broker
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if rt.boots != 1 || (running && rt.stops != 1) {
				t.Fatalf("booted %d times and stopped %d, want one boot after one stop", rt.boots, rt.stops)
			}
			if !slices.Contains(broker.forgotten, c.VMName) {
				t.Errorf("forgotten %v, want %s removed from the broker before the boot", broker.forgotten, c.VMName)
			}
			if len(broker.registered) != 0 {
				t.Errorf("a run with no route registered %+v", broker.registered)
			}
		})
	}
}

// A policy detached from a sandbox that keeps running leaves no route. The
// next exec removes the registration, so the agent's tokens are refused from
// then on rather than until a stop.
func TestAnExecWithNoRouteLeftForgetsTheSandbox(t *testing.T) {
	rt := &livenessRuntime{running: true}
	c := livenessConfig(t, rt)
	broker := &fakeBroker{}
	c.broker = broker
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.boots != 0 {
		t.Fatalf("a running sandbox was rebooted %d times", rt.boots)
	}
	if !slices.Equal(broker.forgotten, []string{c.VMName}) {
		t.Errorf("forgotten %v, want [%s]", broker.forgotten, c.VMName)
	}
}
