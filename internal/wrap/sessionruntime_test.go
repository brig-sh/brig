package wrap

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/verify"
)

// bootOn runs the profile's default session on a fake runtime of kind, with
// the guest home brig creates, and returns the run and its runtime.
func bootOn(t *testing.T, p profile.Profile, kind string) (*Config, *networkDefaultRuntime, error) {
	t.Helper()
	rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{}, kind: kind}
	c, err := Load(p, Options{}, rt)
	if err != nil {
		t.Fatal(err)
	}
	rt.workspace = c.Workspace
	c.Verify, c.MacOSVersion = verify.Off, func() string { return "" }
	c.Out, c.Err, c.Progress = io.Discard, io.Discard, io.Discard
	return c, rt, c.EnsureRunning(creds.Set{})
}

// Negative: a session booted on one runtime is refused on another, which
// boots nothing and leaves the guest home as it was. The runtimes do not see
// each other's sandboxes, so the second one used to boot a VM of the same name
// on the same home, and its next rm deleted that home under the first.
func TestASessionOnOneRuntimeIsRefusedOnAnother(t *testing.T) {
	for _, tt := range []struct{ first, second string }{
		{"nerdctl", "hull"},
		{"hull", "nerdctl"},
	} {
		t.Run(tt.first+" then "+tt.second, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_POLICY_DIR", t.TempDir())
			t.Setenv("BRIG_HYPERVISOR", "")
			p := testProfile(t, "hypervisor: hvi\n")
			first, _, err := bootOn(t, p, tt.first)
			if err != nil {
				t.Fatal(err)
			}
			if got := readSessionIndex()[first.sessionRef()].Runtime; got != tt.first {
				t.Fatalf("the session records runtime %q, want %q", got, tt.first)
			}
			work := filepath.Join(first.Workspace, "work")
			if err := os.WriteFile(work, []byte("the agent's work"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, second, err := bootOn(t, p, tt.second)
			var other *OtherRuntimeError
			if !errors.As(err, &other) || other.Recorded != tt.first || other.Current != tt.second {
				t.Fatalf("the run on %s: %v, want it refused as %s's", tt.second, err, tt.first)
			}
			if second.boots != 0 {
				t.Errorf("%s booted a second VM of the session", tt.second)
			}
			if b, err := os.ReadFile(work); err != nil || string(b) != "the agent's work" {
				t.Errorf("the guest home lost its work: %q, %v", b, err)
			}
			if got := readSessionIndex()[first.sessionRef()].Runtime; got != tt.first {
				t.Errorf("the refused run rewrote the session's runtime to %q", got)
			}
		})
	}
}

// An entry with no runtime, from a release before it was recorded, belongs to
// whichever runtime reads it, as every entry did before.
func TestASessionWithNoRecordedRuntimeIsAnyRuntimes(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"probe@x1": {Home: "/ws", Sandbox: "brig-probe-x1"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"hull", "nerdctl"} {
		c := &Config{VMName: "brig-probe-x1", Runtime: &networkDefaultRuntime{kind: kind}}
		if err := c.runtimeRefusal(); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

// ls counts a sandbox recorded on another runtime as there, since its own
// runtime cannot list it, and still prunes one of its own runtime, or of none,
// that it does not list.
func TestSandboxesOfOtherRuntimes(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"probe@x1":  {Home: "/a", Sandbox: "brig-probe-x1", Runtime: "nerdctl"},
		"probe@x2":  {Home: "/b", Sandbox: "brig-probe-x2", Runtime: "hull"},
		"probe@old": {Home: "/c", Sandbox: "brig-probe-old"},
	}); err != nil {
		t.Fatal(err)
	}
	got := SandboxesOfOtherRuntimes("hull")
	if len(got) != 1 || got[0] != "brig-probe-x1" {
		t.Errorf("SandboxesOfOtherRuntimes(hull) = %v, want only nerdctl's", got)
	}
}

// The refusal names a setting that selects the session's runtime. docker is
// not a value of BRIG_RUNTIME, so a session on docker names BRIG_RUNTIME_BIN.
func TestTheRefusalNamesASettingThatWorks(t *testing.T) {
	for kind, want := range map[string]string{
		"hull":    "set BRIG_RUNTIME=hull ",
		"nerdctl": "set BRIG_RUNTIME=nerdctl ",
		"docker":  "set BRIG_RUNTIME_BIN=docker ",
	} {
		msg := (&OtherRuntimeError{Sandbox: "brig-probe-x1", Recorded: kind, Current: "other", OnPath: true}).Error()
		if !strings.Contains(msg, want) {
			t.Errorf("%s: %q does not say %q", kind, msg, want)
		}
	}
}

// Negative: stop and remove of a session recorded on another runtime are
// refused before the broker forgets its tokens or the runtime is asked. This
// runtime cannot stop that sandbox and would report it stopped, with the
// tokens of the VM still running on the other runtime revoked.
func TestStopAndRemoveOnAnotherRuntimeAreRefused(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "")
	p := testProfile(t, "hypervisor: hvi\n")
	first, _, err := bootOn(t, p, "nerdctl")
	if err != nil {
		t.Fatal(err)
	}
	rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{}, kind: "hull"}
	broker := &fakeBroker{}
	c := &Config{VMName: first.VMName, Runtime: rt, broker: broker, Err: io.Discard}
	for verb, f := range map[string]func() error{"stop": c.Stop, "rm": c.Remove} {
		var other *OtherRuntimeError
		if err := f(); !errors.As(err, &other) {
			t.Errorf("%s on hull: %v, want it refused as nerdctl's", verb, err)
		}
	}
	if rt.stops != 0 || rt.removes != 0 || len(broker.forgotten) != 0 {
		t.Errorf("acted on another runtime's sandbox: %d stops, %d removes, tokens revoked for %v",
			rt.stops, rt.removes, broker.forgotten)
	}
	if _, err := os.Stat(first.Workspace); err != nil {
		t.Errorf("the guest home went: %v", err)
	}
}

// The refusal always names the setting that reaches the recorded runtime.
// When brig finds that runtime nowhere on PATH it adds brig rm --forget, the
// one way past for a session whose runtime is gone, without saying it is
// gone: BRIG_RUNTIME_BIN or a profile's runtimeBin may still reach it.
func TestTheRefusalNamesForgetWhenTheRuntimeIsOffPath(t *testing.T) {
	off := (&OtherRuntimeError{Sandbox: "brig-probe-x1", Recorded: "nerdctl", Current: "hull"}).Error()
	for _, want := range []string{"set BRIG_RUNTIME=nerdctl ", "no nerdctl on PATH", "brig rm --forget"} {
		if !strings.Contains(off, want) {
			t.Errorf("off PATH: %q does not say %q", off, want)
		}
	}
	if strings.Contains(off, "not installed") {
		t.Errorf("off PATH: %q says the runtime is not installed", off)
	}
	on := (&OtherRuntimeError{Sandbox: "brig-probe-x1", Recorded: "nerdctl", Current: "hull", OnPath: true}).Error()
	if !strings.Contains(on, "set BRIG_RUNTIME=nerdctl ") || strings.Contains(on, "--forget") {
		t.Errorf("on PATH: %q", on)
	}
}
