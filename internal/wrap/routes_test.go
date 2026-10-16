package wrap

import (
	"bytes"
	"encoding/json"
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
	// stuck is a broker whose process is alive and does not answer.
	stuck bool
	// extra is what Status reports besides the pid and the port.
	extra injector.Status
	// ca is the CA it hands a brig.internal sandbox, or "" for testBrigCA.
	ca string
}

const fakeBrokerPort = 47821

func (f *fakeBroker) Register(s injector.Sandbox) (int, injector.AddResponse, error) {
	f.registered = append(f.registered, s)
	f.running = true
	var added injector.AddResponse
	if s.BrigInternal != "" || len(s.Modes) > 0 {
		dir, err := injector.Dir()
		if err != nil {
			return 0, added, err
		}
		if added.DivertSocket, err = injector.DivertSocket(dir, s.ID); err != nil {
			return 0, added, err
		}
	}
	if s.BrigInternal != "" {
		added.CA = f.ca
		if added.CA == "" {
			added.CA = testBrigCA()
		}
	}
	return fakeBrokerPort, added, nil
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
	if f.stuck {
		return injector.Status{PID: 4242}, false
	}
	if !f.running {
		return injector.Status{}, false
	}
	st := f.extra
	st.PID, st.Port = 4242, fakeBrokerPort
	return st, true
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
	// On a microVM whose runtime cannot route brig.internal, the row says
	// the run is refused, and where to read why.
	c.Runtime = fakeRuntime{}
	if got := c.brokerLine(); !strings.Contains(got, "transport brig.internal") ||
		!strings.Contains(got, "refused: this run cannot route brig.internal to the broker; brig info --json says why") {
		t.Errorf("the BROKER row on hull is %q", got)
	}
}

// The BROKER row and info --json name the route modes the run path serves.
// The host kind serves endpoint routes, and hull on vz serves none.
func TestTheBrokerRowNamesTheModes(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, &hostRunRecorder{})
	c.broker = &fakeBroker{running: true}
	if got := c.brokerLine(); !strings.Contains(got, "; modes endpoint") {
		t.Errorf("the BROKER row on the host kind is %q", got)
	}
	d := c.InfoData(creds.Set{})
	if d.Broker == nil || !slices.Equal(d.Broker.Modes, []string{"endpoint"}) {
		t.Errorf("info --json broker on the host kind = %+v", d.Broker)
	}

	t.Setenv("BRIG_HYPERVISOR", "vz")
	c.Runtime = fakeRuntime{}
	if got := c.brokerLine(); !strings.Contains(got, "; modes none") {
		t.Errorf("the BROKER row on vz is %q", got)
	}
	blob, err := json.Marshal(c.InfoData(creds.Set{}).Broker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"modes":[]`) {
		t.Errorf("info --json broker on vz = %s", blob)
	}
}

// The BROKER row and info --json name a secret whose renewal journal is not
// settled, and the placeholder refusals of this sandbox alone. Another
// sandbox's count is not this run's to report, and a sandbox with none
// shows nothing.
func TestTheBrokerRowNamesRecoveryAndThisSandboxsRefusals(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, &hostRunRecorder{})
	fb := &fakeBroker{running: true, extra: injector.Status{
		Recovering:  []string{"claude-credentials"},
		Placeholder: map[string]int64{c.VMName: 3, "brig-other": 9},
	}}
	c.broker = fb
	line := c.brokerLine()
	// The semicolon ends the count, so a sum of both sandboxes' counts
	// does not match.
	for _, want := range []string{"refresh recovering: claude-credentials;", "placeholder refusals: 3;"} {
		if !strings.Contains(line, want) {
			t.Errorf("the BROKER row %q does not carry %q", line, want)
		}
	}
	d := c.InfoData(creds.Set{})
	if d.Broker == nil || !slices.Equal(d.Broker.Recovering, []string{"claude-credentials"}) || d.Broker.Placeholder != 3 {
		t.Errorf("info --json broker = %+v", d.Broker)
	}

	fb.extra = injector.Status{Placeholder: map[string]int64{"brig-other": 9}}
	if line := c.brokerLine(); strings.Contains(line, "placeholder") || strings.Contains(line, "recovering") {
		t.Errorf("the BROKER row of a sandbox with no refusal and nothing recovering is %q", line)
	}
	blob, err := json.Marshal(c.InfoData(creds.Set{}).Broker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "placeholder") || strings.Contains(string(blob), "recovering") {
		t.Errorf("info --json broker of a sandbox with neither is %s", blob)
	}
}

// A broker whose process is alive and does not answer on its control socket
// is named by its pid. It is not reported as "not running", which would say
// that the next run starts a broker, and the next run fails to.
func TestABrokerThatDoesNotAnswerIsNamed(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, &hostRunRecorder{})
	c.broker = &fakeBroker{stuck: true}
	if line := c.brokerLine(); !strings.Contains(line, "brig-broker pid 4242 is not answering on its control socket") ||
		strings.Contains(line, "the run starts it") {
		t.Errorf("the BROKER row of a broker that does not answer is %q", line)
	}
	c.broker = &fakeBroker{}
	if line := c.brokerLine(); !strings.Contains(line, "brig-broker not running; the run starts it") {
		t.Errorf("the BROKER row with no broker is %q", line)
	}

	// brig-broker as a run finds it: a pid file that names this process,
	// with its start time, and no control socket.
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	dir, err := injector.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if st, ok := (&brokerProcess{}).Status(); ok || st.PID != 0 {
		t.Errorf("with no pid file, Status = %+v, %v", st, ok)
	}
	start, err := injector.ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, injector.PIDName), []byte(injector.PIDLine(os.Getpid(), start)), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, ok := (&brokerProcess{}).Status(); ok || st.PID != os.Getpid() {
		t.Errorf("with a live pid and no socket, Status = %+v, %v, want pid %d and false", st, ok, os.Getpid())
	}
}
