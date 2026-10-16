package wrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/routemode"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// relayPolicy binds claude-code to the Claude route in mode relay, which a
// test may register.
const relayPolicy = "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: claude-relay}\nspec:\n" +
	"  routes:\n    - name: anthropic\n      preset: claude-code\n      mode: relay\n" +
	"      credential: {secretRef: {name: claude-credentials}}\n"

// listTree names every file and directory under dir, so a test can tell
// whether a refused run wrote anything.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

// A run bound to a route in a mode this build does not serve is refused
// before anything boots or is written, on every runtime kind. The reason
// says the mode is not part of this build, and brig exits 7.
func TestARouteInAModeNotInThisBuildIsRefused(t *testing.T) {
	for _, c := range []struct{ name, kind, hypervisor string }{
		{"hull on vz", "hull", "vz"},
		{"hull on hvi", "hull", "hvi"},
		{"nerdctl", "nerdctl", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "claude-relay.yaml"), []byte(relayPolicy), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BRIG_POLICY_DIR", dir)
			t.Setenv("BRIG_HYPERVISOR", c.hypervisor)
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("BRIG_STATE_DIR", state)

			cfg := loadWithPolicy(t, "claude-relay")
			rec := &kindRecorder{kind: c.kind}
			cfg.Runtime = rec
			broker := &fakeBroker{}
			cfg.broker = broker
			before := listTree(t, state)
			err := cfg.EnsureRunning(creds.Set{})
			for _, want := range []string{"claude-relay (route anthropic) declares a route in mode relay",
				"route mode relay is not part of this build of brig", "Use mode: endpoint"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			var ce *runtime.CapabilityError
			if !errors.As(err, &ce) {
				t.Errorf("the refusal is not a CapabilityError, so brig would not exit 7: %v", err)
			}
			if len(rec.asked) != 0 {
				t.Errorf("the runtime was asked %v before the refusal", rec.asked)
			}
			if len(broker.registered) != 0 {
				t.Errorf("the broker was asked to serve a refused run: %v", broker.registered)
			}
			if after := listTree(t, state); !slices.Equal(before, after) {
				t.Errorf("the refused run wrote state: %v, was %v", after, before)
			}
			if d := cfg.Plan(); d.Refused == nil || !strings.Contains(d.Refused.Reason, "not part of this build") {
				t.Errorf("the plan does not carry the refusal: %+v", d.Refused)
			}
		})
	}
}

// fakeMode is the run of the route mode relay, which a test registers.
type fakeMode struct {
	refuse    error
	checked   []runtime.RunPath
	forgotten []string
	setup     *fakeSetup
}

func (f *fakeMode) check(_ Env, _ runtime.Runtime, path runtime.RunPath, _ string) error {
	f.checked = append(f.checked, path)
	return f.refuse
}

func (f *fakeMode) unavailable(c *Config) string {
	if c.modeServed("relay") {
		return ""
	}
	return "relay unavailable"
}

func (f *fakeMode) register(c *Config, s *injector.Sandbox) ([]runtime.Setup, error) {
	if s.Modes == nil {
		s.Modes = map[v1alpha1.RouteMode]json.RawMessage{}
	}
	s.Modes["relay"] = json.RawMessage(`{"sandbox":"` + c.VMName + `"}`)
	return []runtime.Setup{f.setup}, nil
}

func (f *fakeMode) forget(vmName string) error {
	f.forgotten = append(f.forgotten, vmName)
	return nil
}

// fakeSetup is the sandbox setup fakeMode hands the boot.
type fakeSetup struct{}

func (*fakeSetup) Check(runtime.RunPath, runtime.RunSpec) error { return nil }
func (*fakeSetup) Args(runtime.RunPath) ([]string, error)       { return []string{"--relay"}, nil }

// registerRelay registers the route mode relay on the claude-code preset,
// served on hull on hvi, with the run f, for the length of a test.
func registerRelay(t *testing.T, f *fakeMode) {
	t.Helper()
	t.Cleanup(routemode.Register(routemode.Mode{Name: "relay", Presets: []v1alpha1.Preset{v1alpha1.PresetClaudeCode}}))
	t.Cleanup(runtime.RegisterModeRecords(
		runtime.ModeRecord{Mode: "relay", Path: runtime.RunPath{Runtime: "hull", Backend: "hvi"},
			State: runtime.Enforced, Why: "the gateway diverts it", Transport: "divert"},
		runtime.ModeRecord{Mode: "relay", Path: runtime.RunPath{Runtime: "hull", Backend: "vz"},
			State: runtime.CannotEnforce, Why: "vz has no gateway brig owns", Remedy: "Use mode: endpoint"},
	))
	t.Cleanup(registerRouteMode("relay", f))
}

// A run with a route in a registered mode goes through the mode's run: its
// check before boot, its registration with the broker, the divert of the
// route's host and port, the setup it hands the runtime, and its cleanup
// when the routes are revoked. The guest gets the token alone.
func TestARegisteredRouteModeRunsThroughItsHooks(t *testing.T) {
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	f := &fakeMode{setup: &fakeSetup{}}
	registerRelay(t, f)
	rec := &hullRecorder{}
	cfg := routedConfig(t, strings.Replace(relayPolicy, "claude-relay", "brokered", 1)+"  egress:\n    default: deny\n", rec)
	shortStateDir(t)
	cfg.Verify = verify.Off
	broker := &fakeBroker{}
	cfg.broker = broker
	set, err := cfg.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureRunning(set); err == nil || !strings.Contains(err.Error(), "not booting") {
		t.Fatalf("the run did not reach the runtime: %v", err)
	}
	if len(f.checked) == 0 || f.checked[0] != (runtime.RunPath{Runtime: "hull", Backend: "hvi"}) {
		t.Errorf("the mode was checked on %v", f.checked)
	}
	if len(broker.registered) != 1 || string(broker.registered[0].Modes["relay"]) != `{"sandbox":"`+cfg.VMName+`"}` ||
		broker.registered[0].BrigInternal != "" {
		t.Fatalf("registered %+v", broker.registered)
	}
	if rec.spec == nil || len(rec.spec.Setup) != 1 || rec.spec.Setup[0] != f.setup {
		t.Fatalf("the runtime was handed %+v", rec.spec)
	}
	d := rec.spec.Egress.Divert
	dir, _ := injector.Dir()
	sock, _ := injector.DivertSocket(dir, cfg.VMName)
	if d.BrigInternal || d.Socket != sock || len(d.Routes) != 1 ||
		d.Routes[0] != (runtime.DivertRoute{Host: "api.anthropic.com", Port: 443, Mode: "relay"}) {
		t.Errorf("Divert = %+v", d)
	}
	// Under a deny default the guest must still resolve the diverted host.
	if !slices.Contains(rec.spec.Egress.Allow, runtime.Rule{Host: "api.anthropic.com"}) {
		t.Errorf("the deny default does not allow the diverted host: %+v", rec.spec.Egress.Allow)
	}
	env := envOf(cfg.execSpec(set, []string{"claude"}, false).Env)
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("the route in mode relay got a route URL")
	}
	if v := env["CLAUDE_CODE_OAUTH_TOKEN"]; v.Value == "" || !v.Secret {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %+v", v)
	}
	if got := cfg.brokerLine(); !strings.HasPrefix(got, "transport divert, routes anthropic") ||
		!strings.Contains(got, "; modes endpoint, relay") {
		t.Errorf("BROKER row: %q", got)
	}
	// The stub does not boot, and a run that does not boot forgets its
	// sandbox, the mode's leftovers with it.
	if !onlySandbox(f.forgotten, cfg.VMName) {
		t.Errorf("forgotten %v after a failed boot", f.forgotten)
	}
	f.forgotten = nil
	if err := RevokeRoutes(cfg.VMName); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.forgotten, []string{cfg.VMName}) {
		t.Errorf("forgotten %v", f.forgotten)
	}

	// The mode's own check has the last word where the table answers
	// enforced, and the BROKER row carries what the mode adds.
	f.refuse = &runtime.CapabilityError{Property: runtime.RouteModeProperty("relay"),
		Path: runtime.RunPath{Runtime: "hull", Backend: "hvi"}, State: runtime.CannotEnforce, Why: "relay is off"}
	if err := cfg.routeRefusal(); err == nil || !strings.Contains(err.Error(), "relay is off") {
		t.Errorf("routeRefusal = %v", err)
	}
	if got := cfg.brokerLine(); !strings.Contains(got, "modes endpoint; relay unavailable") {
		t.Errorf("BROKER row of a refused relay: %q", got)
	}
	if u := cfg.unavailableModes(); !strings.Contains(u["relay"], "relay is off") {
		t.Errorf("unavailable = %v", u)
	}
	t.Setenv("BRIG_HYPERVISOR", "vz")
	if err := cfg.routeRefusal(); err == nil || !strings.Contains(err.Error(), "vz has no gateway brig owns") {
		t.Errorf("routeRefusal on vz = %v", err)
	}
}
