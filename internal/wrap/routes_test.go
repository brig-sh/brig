package wrap

import (
	"bytes"
	"errors"
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

// hostRunRecorder is the host kind as far as a boot goes: it records the spec
// it was handed and boots nothing.
type hostRunRecorder struct {
	runtime.Runtime
	spec *runtime.RunSpec
}

func (r *hostRunRecorder) Kind() string                          { return runtime.HostKind }
func (r *hostRunRecorder) Bin() string                           { return "sandbox-exec" }
func (r *hostRunRecorder) Running(string) (bool, error)          { return false, nil }
func (r *hostRunRecorder) Remove(string) error                   { return nil }
func (r *hostRunRecorder) Stop(string) error                     { return nil }
func (r *hostRunRecorder) SandboxNetwork(string) (string, error) { return "", nil }
func (r *hostRunRecorder) Isolation(string) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryProcess, Detail: "shared kernel"}
}
func (r *hostRunRecorder) Run(spec runtime.RunSpec) error {
	r.spec = &spec
	return errors.New("stub runtime: not booting")
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
	return c
}

func envOf(vars []runtime.Var) map[string]runtime.Var {
	out := map[string]runtime.Var{}
	for _, v := range vars {
		out[v.Name] = v
	}
	return out
}

// The host kind serves routes over loopback. The run registers its routes,
// mints a token per route for the exec, hands the guest the route URL and
// the token in place of the credential, and allows the broker's port and no
// other in the sandbox's profile. Claude Code needs no tmpfs there, so the
// host kind takes the profile it refused before.
func TestAHostRunBoundToARouteGetsATokenAndThePort(t *testing.T) {
	rec := &hostRunRecorder{}
	c := routedConfig(t, claudeAndGitHub, rec)
	broker := &fakeBroker{}
	c.broker = broker
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	if rec.spec == nil {
		t.Fatal("the runtime was never asked to run")
	}
	if len(broker.registered) != 1 || broker.registered[0].ID != c.VMName {
		t.Fatalf("registered %+v, want the sandbox once", broker.registered)
	}
	var names []string
	for _, r := range broker.registered[0].Routes {
		names = append(names, r.Policy+"/"+r.Route.Name)
	}
	if !slices.Equal(names, []string{"brokered/anthropic", "brokered/github"}) {
		t.Errorf("registered routes %v", names)
	}
	if !slices.Equal(rec.spec.LoopbackPorts, []int{fakeBrokerPort}) {
		t.Errorf("LoopbackPorts = %v, want the broker's port alone", rec.spec.LoopbackPorts)
	}
	if len(rec.spec.Tmpfs) != 0 {
		t.Errorf("the host kind was handed a tmpfs: %v", rec.spec.Tmpfs)
	}
	if rec.spec.Net != "isolated" {
		t.Errorf("a run with routes runs %q, want isolated", rec.spec.Net)
	}
	env := envOf(c.execSpec(set, []string{"claude"}, false).Env)
	base := "http://127.0.0.1:47821/" + c.VMName + "/r/"
	for name, want := range map[string]string{
		"ANTHROPIC_BASE_URL":      base + "anthropic",
		"CLAUDE_CODE_OAUTH_TOKEN": "session-token-" + c.VMName + "-anthropic",
		"BRIG_ROUTE_GITHUB_TOKEN": "session-token-" + c.VMName + "-github",
		"CLAUDE_CONFIG_DIR":       "/root/.claude",
		"IS_SANDBOX":              "1",
	} {
		if got := env[name].Value; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "BRIG_ROUTE_GITHUB_TOKEN"} {
		if !env[name].Secret {
			t.Errorf("%s is not marked Secret, so it could reach argv", name)
		}
	}
	// The GitHub token the profile binds is the brokered secret's, so the
	// whole binding is gone, the ambient value with it.
	for _, name := range []string{"GH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s reached the guest: %q", name, v.Value)
		}
	}
	for _, v := range rec.spec.Env {
		if strings.Contains(v.Value, "ambient-gh-token-value") {
			t.Errorf("the ambient GitHub token reached the guest as %s", v.Name)
		}
	}
	if len(broker.minted) != 2 || broker.minted[0].TTLSeconds != int64(injector.TokenTTL.Seconds()) {
		t.Errorf("minted %+v, want a 12h token per route", broker.minted)
	}
}

// A secret a route injects stays on the host: its files: binding and every
// env binding that names it are dropped, and brig does not read it. On the
// host kind the tmpfs that held the file goes too, and CLAUDE_CONFIG_DIR
// follows it into the home. A microVM keeps the tmpfs.
func TestABrokeredSecretIsNeverDelivered(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	routes, _, err := routesOf([]policy.Policy{mustPolicy(t, claudeAndGitHub)})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []bool{false, true} {
		got, bindings, skipped := withRoutes(p, p.Env, routes, host)
		if !slices.Equal(skipped, []string{"claude-credentials", "gh-token"}) {
			t.Errorf("host=%v: skipped %v", host, skipped)
		}
		if len(got.Files) != 0 {
			t.Errorf("host=%v: files still delivered: %+v", host, got.Files)
		}
		for _, d := range got.Secrets {
			if d.Name == "claude-credentials" || d.Name == "gh-token" {
				t.Errorf("host=%v: brig still reads %s", host, d.Name)
			}
		}
		for _, b := range append(slices.Clone(got.Env), bindings...) {
			if b.Name == "GH_TOKEN" {
				t.Errorf("host=%v: GH_TOKEN is still bound: %+v", host, b)
			}
		}
		dir := ""
		for _, b := range bindings {
			if b.Name == "CLAUDE_CONFIG_DIR" {
				dir = b.Value
			}
		}
		if host {
			if len(got.Volumes) != 0 {
				t.Errorf("the host kind keeps volumes %+v", got.Volumes)
			}
			if dir != "/root/.claude" {
				t.Errorf("CLAUDE_CONFIG_DIR = %q on the host kind, want the home's .claude", dir)
			}
		} else {
			if len(got.Tmpfs()) != 1 || dir != "/brig/claude" {
				t.Errorf("a microVM lost its tmpfs (%d) or its config dir %q", len(got.Tmpfs()), dir)
			}
		}
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
// that stops it still has the policy: a detached policy leaves the sandbox
// registered until then.
func TestStopAndRemoveRevokeTheSandboxTokens(t *testing.T) {
	rec := &hostRunRecorder{}
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
func TestEnvelopeHasABrokerRow(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, &hostRunRecorder{})
	c.broker = &fakeBroker{running: true}
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	got := block.String()
	for _, want := range []string{
		"BROKER       transport loopback, routes anthropic, github; brig-broker pid 4242 on 127.0.0.1:47821; " +
			"kept on the host: claude-credentials, gh-token",
		"CLAUDE_CODE_OAUTH_TOKEN(route: anthropic)",
		"BRIG_ROUTE_GITHUB_TOKEN(route: github)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not carry %q:\n%s", want, got)
		}
	}
	d := c.InfoData(creds.Set{})
	if d.Broker == nil || d.Broker.Transport != "loopback" || !d.Broker.Running || d.Broker.Port != fakeBrokerPort {
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
	// On a microVM the row says the run is refused, and why.
	c.Runtime = fakeRuntime{}
	if got := c.brokerLine(); !strings.Contains(got, "transport brig.internal") ||
		!strings.Contains(got, "brig.internal routing is not available on this runtime yet") {
		t.Errorf("the BROKER row on hull is %q", got)
	}
}
