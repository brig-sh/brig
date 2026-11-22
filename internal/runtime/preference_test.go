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

// A path in BRIG_RUNTIME_BIN is checked the way a profile's runtimeBin is.
// Taken on trust, a value that is not there reached the first exec instead,
// where it surfaced as a sandbox whose state brig could not read and named the
// variable nowhere (#139).
func TestDetectForReportsAMissingRuntimeBinFromTheEnvironment(t *testing.T) {
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", filepath.Join(t.TempDir(), "not-there"))

	_, err := DetectFor(Preference{})
	if err == nil {
		t.Fatal("expected a missing BRIG_RUNTIME_BIN to be reported")
	}
	if !errors.Is(err, ErrBadRuntime) {
		t.Errorf("%v does not match ErrBadRuntime, so it would not exit 4", err)
	}
	if errors.Is(err, ErrNoRuntime) {
		t.Errorf("a bad BRIG_RUNTIME_BIN matched ErrNoRuntime, so env/ls would swallow it: %v", err)
	}
	// The variable is what the reader has to fix, so the variable is what the
	// message has to name: the profile is not where this one came from.
	if !strings.Contains(err.Error(), "BRIG_RUNTIME_BIN") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

// A file that is there but cannot be executed fails the same way: it is a
// runtime that was named and is broken, not one that is absent.
func TestDetectForRefusesANonExecutableRuntimeBinFromTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hull")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", path)

	_, err := DetectFor(Preference{})
	if err == nil {
		t.Fatal("expected a non-executable BRIG_RUNTIME_BIN to be refused")
	}
	if !errors.Is(err, ErrBadRuntime) {
		t.Errorf("%v does not match ErrBadRuntime, so it would not exit 4", err)
	}
	if !strings.Contains(err.Error(), "BRIG_RUNTIME_BIN") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

// BRIG_RUNTIME_BIN=hull is a name to look up, not a path, and has always been
// read that way by the exec underneath. Checking it as a path would have
// refused every value that was working the day before the check landed.
func TestDetectForResolvesABareRuntimeBinOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := executableFile(t, dir, "hull")
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", "hull")
	t.Setenv("PATH", dir)

	rt, err := DetectFor(Preference{})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Bin() != bin {
		t.Errorf("Bin() = %q, want the %q found on PATH", rt.Bin(), bin)
	}
}

// A bare name nothing on PATH answers is still a runtime that was named and is
// not there, so it is ErrBadRuntime rather than the "nothing installed"
// sentinel env and ls degrade over.
func TestDetectForReportsABareRuntimeBinThatIsNotOnPath(t *testing.T) {
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", "hull")
	t.Setenv("PATH", t.TempDir())

	_, err := DetectFor(Preference{})
	if err == nil {
		t.Fatal("expected a bare BRIG_RUNTIME_BIN that is not on PATH to be reported")
	}
	if !errors.Is(err, ErrBadRuntime) {
		t.Errorf("%v does not match ErrBadRuntime, so it would not exit 4", err)
	}
	if errors.Is(err, ErrNoRuntime) {
		t.Errorf("a bad BRIG_RUNTIME_BIN matched ErrNoRuntime, so env/ls would swallow it: %v", err)
	}
	if !strings.Contains(err.Error(), "BRIG_RUNTIME_BIN") {
		t.Errorf("error does not name the variable: %v", err)
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

// The hypervisor a profile asks for reaches the command line. hvi is the
// default when nothing asks, and vz for a graphical profile.
func TestHypervisorComesFromTheSpec(t *testing.T) {
	if got := hypervisor(RunSpec{}); got != "hvi" {
		t.Errorf("default hypervisor = %q, want hvi", got)
	}
	if got := hypervisor(RunSpec{GUI: true}); got != "vz" {
		t.Errorf("default hypervisor of a graphical spec = %q, want vz", got)
	}
	if got := hypervisor(RunSpec{Hypervisor: "hvi"}); got != "hvi" {
		t.Errorf("hypervisor = %q, want the requested hvi", got)
	}
}

// printingFile is an executable that prints line, whatever it is asked.
func printingFile(t *testing.T, dir, name, line string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Negative: a binary named by a setting that is the other runtime is refused,
// with both settings named. The bundle's environment, sourced for nerdctl and
// followed by export BRIG_RUNTIME=hull, used to drive nerdctl as hull.
func TestDetectForRefusesTheOtherRuntimesBinary(t *testing.T) {
	for _, tt := range []struct{ name, kind, file, line, other string }{
		{"nerdctl as hull", "hull", "nerdctl", "nerdctl version 2.3.5", "nerdctl"},
		{"docker as hull", "hull", "docker", "Docker version 27.3.1, build ce12230", "docker"},
		{"a wrapper of nerdctl as hull", "hull", "rt", "nerdctl version 2.3.5", "nerdctl"},
		{"hull as nerdctl", "nerdctl", "hull", "hull v0.1.0-rc30 (abc1234, 2026-10-01, go1.26.5, linux/amd64)", "hull"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := printingFile(t, t.TempDir(), tt.file, tt.line)
			t.Setenv("BRIG_RUNTIME", tt.kind)
			for _, from := range []string{"env", "profile"} {
				var pref Preference
				if from == "env" {
					t.Setenv("BRIG_RUNTIME_BIN", bin)
				} else {
					t.Setenv("BRIG_RUNTIME_BIN", "")
					pref.Bin = bin
				}
				rt, err := DetectFor(pref)
				if !errors.Is(err, ErrBadRuntime) {
					t.Fatalf("%s: drove %s as %s: %v, %v", from, tt.other, tt.kind, rt, err)
				}
				for _, want := range []string{tt.kind, tt.other, bin} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: %q does not name %s", from, err, want)
					}
				}
			}
		})
	}
}

// A binary that names neither runtime passes, since a build or a wrapper may
// print anything, and docker is a binary of the nerdctl runtime.
func TestDetectForTakesABinaryThatNamesNoOtherRuntime(t *testing.T) {
	for _, tt := range []struct{ name, kind, file, line string }{
		{"hull", "hull", "hull", "hull v0.1.0-rc30 (abc1234, 2026-10-01, go1.26.5, linux/amd64)"},
		{"a wrapper that prints nothing known", "hull", "my-hull", "built from main"},
		{"nerdctl", "nerdctl", "nerdctl", "nerdctl version 2.3.5"},
		{"docker", "nerdctl", "docker", "Docker version 27.3.1, build ce12230"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := printingFile(t, t.TempDir(), tt.file, tt.line)
			t.Setenv("BRIG_RUNTIME", tt.kind)
			t.Setenv("BRIG_RUNTIME_BIN", bin)
			rt, err := DetectFor(Preference{})
			if err != nil || rt.Bin() != bin {
				t.Fatalf("DetectFor = %v, %v; want %s", rt, err, bin)
			}
		})
	}
}

// OnPath looks the binary of a runtime up on PATH, and nowhere else. A kind
// brig does not know reads as on PATH, so its refusal names no --forget.
func TestOnPathLooksTheRuntimeUp(t *testing.T) {
	dir := t.TempDir()
	executableFile(t, dir, "hull")
	t.Setenv("PATH", dir)
	for kind, want := range map[string]bool{"hull": true, "nerdctl": false, "docker": false, "podman": true} {
		if got := OnPath(kind); got != want {
			t.Errorf("OnPath(%q) = %v, want %v", kind, got, want)
		}
	}
}

// A binary named hull is taken by its name, and its --version is not run for
// the check: on macOS that was a process on every verb that names a binary,
// info and env included, where none ran before. A hull of another name is
// still asked, which is what catches a wrapper of nerdctl.
func TestOtherRuntimeBinAsksNoBinaryNamedHull(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "asked")
	script := "#!/bin/sh\necho asked >> '" + log + "'\necho 'nerdctl version 2.3.5'\n"
	for _, name := range []string{"hull", "my-hull"} {
		bin := filepath.Join(dir, name)
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if other := otherRuntimeBin("hull", &hull{bin: filepath.Join(dir, "hull")}); other != "" {
		t.Errorf("a binary named hull read as %s", other)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the check ran --version of a binary named hull")
	}
	if other := otherRuntimeBin("hull", &hull{bin: filepath.Join(dir, "my-hull")}); other != "nerdctl" {
		t.Errorf("a wrapper of nerdctl named my-hull read as %q", other)
	}
}
