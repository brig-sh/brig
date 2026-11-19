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

// readIndexAs reads the session index as brig does on goos for the rest of
// the case. See entryRuntime.
func readIndexAs(t *testing.T, goos string) {
	t.Helper()
	orig := hostOS
	hostOS = goos
	t.Cleanup(func() { hostOS = orig })
}

// On macOS an entry with no runtime, from a release before it was recorded,
// belongs to whichever runtime reads it, as every entry did before.
func TestASessionWithNoRecordedRuntimeIsAnyRuntimes(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "darwin")
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
// runtime cannot list it, and still prunes one of its own runtime, or of none
// on macOS, that it does not list.
func TestSandboxesOfOtherRuntimes(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "darwin")
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

// unrecordedSession boots the profile's default session on nerdctl, puts work in
// its guest home, and rewrites its entry as a release before 154f53e4 wrote
// it, with no runtime. It returns the session's ref and home.
func unrecordedSession(t *testing.T, p profile.Profile) (string, string) {
	t.Helper()
	first, _, err := bootOn(t, p, "nerdctl")
	if err != nil {
		t.Fatal(err)
	}
	ref := first.sessionRef()
	index := readSessionIndex()
	entry := index[ref]
	entry.Runtime = ""
	index[ref] = entry
	if err := writeSessionIndex(index); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Workspace, "work"), []byte("the agent's work"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ref, first.Workspace
}

// Negative: on Linux, hull refuses a session whose entry records no runtime,
// as it refuses one recorded on nerdctl. Every such entry was booted on
// nerdctl, the default before hull. Read as hull's, the run would boot a
// second VM on the same guest home, and stop and rm would act on a sandbox
// hull cannot see: they would revoke the tokens of the VM on nerdctl, and the
// next run would delete its home under it.
func TestOnLinuxHullRefusesASessionWithNoRecordedRuntime(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "linux")
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "")
	p := testProfile(t, "hypervisor: hvi\n")
	ref, home := unrecordedSession(t, p)

	c, rt, err := bootOn(t, p, "hull")
	var other *OtherRuntimeError
	if !errors.As(err, &other) || other.Recorded != "nerdctl" || other.Current != "hull" {
		t.Fatalf("the run on hull: %v, want it refused as nerdctl's", err)
	}
	if rt.boots != 0 {
		t.Error("hull booted a second VM of the session")
	}

	broker := &fakeBroker{}
	c.broker, c.Err = broker, io.Discard
	for verb, f := range map[string]func() error{"stop": c.Stop, "rm": c.Remove} {
		if err := f(); !errors.As(err, &other) || other.Recorded != "nerdctl" {
			t.Errorf("%s on hull: %v, want it refused as nerdctl's", verb, err)
		}
	}
	if rt.stops != 0 || rt.removes != 0 || len(broker.forgotten) != 0 {
		t.Errorf("acted on nerdctl's sandbox: %d stops, %d removes, tokens revoked for %v",
			rt.stops, rt.removes, broker.forgotten)
	}
	if b, err := os.ReadFile(filepath.Join(home, "work")); err != nil || string(b) != "the agent's work" {
		t.Errorf("the guest home lost its work: %q, %v", b, err)
	}
	if entry, ok := readSessionIndex()[ref]; !ok || entry.Runtime != "" {
		t.Errorf("the refused commands dropped or rewrote the entry: %+v, %v", entry, ok)
	}
}

// Negative: on Linux, hull's brig ls keeps a session whose entry records no
// runtime, which it cannot list, and shows it on nerdctl. Its prune would
// drop it, and the next run of the session would delete its home as one no
// session owns. nerdctl, which can list it, still prunes it when it is gone.
func TestOnLinuxListingUnderHullKeepsASessionWithNoRecordedRuntime(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "linux")
	if err := writeSessionIndex(map[string]sessionEntry{
		"probe@old": {Home: "/ws/old", Sandbox: "brig-probe-old"},
	}); err != nil {
		t.Fatal(err)
	}
	others := OtherRuntimeSessions("hull")
	if len(others) != 1 || others[0].Sandbox != "brig-probe-old" || others[0].Runtime != "nerdctl" ||
		others[0].Ref != "probe@old" {
		t.Fatalf("OtherRuntimeSessions(hull) = %+v, want the entry on nerdctl", others)
	}
	PruneSessions(SandboxesOfOtherRuntimes("hull"))
	if got := WorkspaceOfSandbox("brig-probe-old"); got != "/ws/old" {
		t.Fatalf("ls on hull pruned the session nerdctl booted: %q", got)
	}

	for _, kind := range []string{"nerdctl", "docker"} {
		if got := OtherRuntimeSessions(kind); len(got) != 0 {
			t.Errorf("OtherRuntimeSessions(%s) = %+v, want the entry read as %s's own", kind, got, kind)
		}
	}
	PruneSessions(SandboxesOfOtherRuntimes("nerdctl"))
	if got := WorkspaceOfSandbox("brig-probe-old"); got != "" {
		t.Errorf("ls on nerdctl kept a session whose sandbox it does not list: %q", got)
	}
}

// On Linux the nerdctl runtime, under nerdctl or docker, reads an entry with
// no runtime as its own, since it booted every one of them. Its run reaches
// the sandbox, and records nerdctl on the entry from then on.
func TestOnLinuxNerdctlKeepsASessionWithNoRecordedRuntime(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "linux")
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "")
	p := testProfile(t, "hypervisor: hvi\n")
	ref, _ := unrecordedSession(t, p)

	for _, kind := range []string{"nerdctl", "docker"} {
		c := &Config{VMName: readSessionIndex()[ref].Sandbox, Runtime: &networkDefaultRuntime{kind: kind}}
		if err := c.runtimeRefusal(); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if _, _, err := bootOn(t, p, "nerdctl"); err != nil {
		t.Fatalf("the run on nerdctl: %v", err)
	}
	if got := readSessionIndex()[ref].Runtime; got != "nerdctl" {
		t.Errorf("the run on nerdctl recorded runtime %q, want nerdctl", got)
	}
}

// On macOS an entry with no runtime still belongs to hull: hull's run, stop
// and rm reach it, and its brig ls neither keeps it as another runtime's nor
// shows it as one.
func TestOnMacOSASessionWithNoRecordedRuntimeIsHulls(t *testing.T) {
	isolateState(t)
	readIndexAs(t, "darwin")
	if err := writeSessionIndex(map[string]sessionEntry{
		"probe@old": {Home: "/ws/old", Sandbox: "brig-probe-old"},
	}); err != nil {
		t.Fatal(err)
	}
	c := &Config{VMName: "brig-probe-old", Runtime: &networkDefaultRuntime{kind: "hull"}}
	if err := c.runtimeRefusal(); err != nil {
		t.Errorf("hull: %v", err)
	}
	if got := OtherRuntimeSessions("hull"); len(got) != 0 {
		t.Errorf("OtherRuntimeSessions(hull) = %+v, want none", got)
	}
	PruneSessions(SandboxesOfOtherRuntimes("hull"))
	if got := WorkspaceOfSandbox("brig-probe-old"); got != "" {
		t.Errorf("ls on hull kept a session of its own whose sandbox it does not list: %q", got)
	}
}

// On a Linux host with docker and no nerdctl, hull's refusal of an entry with
// no runtime does not call nerdctl gone or point at brig rm --forget: the
// nerdctl runtime booted it through docker, and the BRIG_RUNTIME=nerdctl it
// names reaches it. Negative: an entry that records nerdctl still reads as
// gone there, since docker does not see what nerdctl booted, and so does an
// entry with no runtime on a host with neither.
func TestTheRefusalOfAnUnrecordedSessionCountsDockerAsReachingIt(t *testing.T) {
	readIndexAs(t, "linux")
	for _, tt := range []struct {
		name, recorded, bin string
		onPath              bool
	}{
		{"no runtime, docker on PATH", "", "docker", true},
		{"no runtime, nerdctl on PATH", "", "nerdctl", true},
		{"no runtime, neither on PATH", "", "", false},
		{"nerdctl recorded, docker on PATH", "nerdctl", "docker", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateState(t)
			dir := t.TempDir()
			if tt.bin != "" {
				if err := os.WriteFile(filepath.Join(dir, tt.bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			if err := writeSessionIndex(map[string]sessionEntry{
				"probe@old": {Home: "/ws/old", Sandbox: "brig-probe-old", Runtime: tt.recorded},
			}); err != nil {
				t.Fatal(err)
			}
			c := &Config{VMName: "brig-probe-old", Runtime: &networkDefaultRuntime{kind: "hull"}}
			var other *OtherRuntimeError
			if err := c.runtimeRefusal(); !errors.As(err, &other) || other.Recorded != "nerdctl" {
				t.Fatalf("hull: %v, want it refused as nerdctl's", err)
			}
			if other.OnPath != tt.onPath {
				t.Errorf("OnPath = %v, want %v: %v", other.OnPath, tt.onPath, other)
			}
			if forget := strings.Contains(other.Error(), "--forget"); forget == tt.onPath {
				t.Errorf("the refusal names --forget: %v: %v", forget, other)
			}
		})
	}
}
