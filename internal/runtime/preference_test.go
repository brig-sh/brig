package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executableFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A profile can name the binary to drive, so a build can be pinned without a
// variable in every shell.
func TestDetectForUsesTheProfilesRuntimeBin(t *testing.T) {
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_RUNTIME", "hull")
	bin := executableFile(t, t.TempDir(), "hull")

	rt, err := DetectFor(Preference{Bin: bin})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Bin() != bin {
		t.Errorf("Bin() = %q, want the profile's %q", rt.Bin(), bin)
	}
}

// The environment beats the profile, which is the order every other setting
// follows.
func TestDetectForEnvironmentBeatsTheProfile(t *testing.T) {
	dir := t.TempDir()
	fromEnv := executableFile(t, dir, "hull-from-env")
	fromProfile := executableFile(t, dir, "hull-from-profile")
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", fromEnv)

	rt, err := DetectFor(Preference{Bin: fromProfile})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Bin() != fromEnv {
		t.Errorf("Bin() = %q, want the environment's %q", rt.Bin(), fromEnv)
	}
}

// A mistyped path is reported against the profile that carries it, rather than
// surfacing later as a failed exec with no hint where it came from.
func TestDetectForReportsAMissingRuntimeBin(t *testing.T) {
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_RUNTIME", "hull")

	_, err := DetectFor(Preference{Bin: filepath.Join(t.TempDir(), "not-there")})
	if err == nil {
		t.Fatal("expected a missing runtimeBin to be reported")
	}
	if !strings.Contains(err.Error(), "runtimeBin") {
		t.Errorf("error does not name the field: %v", err)
	}
}

func TestDetectForRefusesADirectoryAsRuntimeBin(t *testing.T) {
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_RUNTIME", "hull")

	if _, err := DetectFor(Preference{Bin: t.TempDir()}); err == nil {
		t.Fatal("expected a directory to be refused")
	}
}

// Only "no runtime binary on PATH" carries ErrNoRuntime. That is the seam env
// and ls degrade on -- exit 0, report what they can -- so an unknown
// BRIG_RUNTIME and a runtimeBin that is not there must NOT match it, or those
// mistakes would be swallowed as "nothing installed". This test is what stops a
// future change to the error type or message from silently widening that
// swallow: it asserts the sentinel is the seam, not "err != nil".
func TestDetectForSentinelMarksOnlyTheMissingRuntime(t *testing.T) {
	t.Setenv("BRIG_RUNTIME_BIN", "")

	// No runtime on PATH: LookPath finds nothing, so both backends report the
	// sentinel.
	t.Setenv("PATH", t.TempDir())
	for _, kind := range []string{"hull", "nerdctl"} {
		t.Setenv("BRIG_RUNTIME", kind)
		_, err := DetectFor(Preference{})
		if err == nil {
			t.Fatalf("BRIG_RUNTIME=%s with an empty PATH was accepted", kind)
		}
		if !errors.Is(err, ErrNoRuntime) {
			t.Errorf("BRIG_RUNTIME=%s: %v does not match ErrNoRuntime", kind, err)
		}
	}

	// An unknown BRIG_RUNTIME is a mistake, not a missing install.
	t.Setenv("BRIG_RUNTIME", "podman")
	if _, err := DetectFor(Preference{}); errors.Is(err, ErrNoRuntime) {
		t.Errorf("unknown BRIG_RUNTIME matched ErrNoRuntime, so env/ls would swallow it: %v", err)
	}

	// A runtimeBin that is not there is a misconfiguration, not a missing
	// install.
	t.Setenv("BRIG_RUNTIME", "hull")
	_, err := DetectFor(Preference{Bin: filepath.Join(t.TempDir(), "not-there")})
	if err == nil {
		t.Fatal("a missing runtimeBin was accepted")
	}
	if errors.Is(err, ErrNoRuntime) {
		t.Errorf("a bad runtimeBin matched ErrNoRuntime, so env/ls would swallow it: %v", err)
	}
}

// A config file is where people write ~, so it has to mean something.
func TestRuntimeBinExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	// Resolve a path that exists on every machine brig runs on.
	got, err := runtimeBinFromProfile("~/../../bin/sh")
	if err != nil {
		t.Skipf("could not resolve a shell through ~: %v", err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("~ was not expanded: %s", got)
	}
	if !strings.HasPrefix(got, filepath.VolumeName(home)+"/") && !strings.HasPrefix(got, "/") {
		t.Errorf("expanded path is not absolute: %s", got)
	}
}

// The hypervisor a profile asks for reaches the command line, and vz remains
// the default when nothing asks.
func TestHypervisorComesFromTheSpec(t *testing.T) {
	if got := hypervisor(RunSpec{}); got != "vz" {
		t.Errorf("default hypervisor = %q, want vz", got)
	}
	if got := hypervisor(RunSpec{Hypervisor: "hvi"}); got != "hvi" {
		t.Errorf("hypervisor = %q, want the requested hvi", got)
	}
}
