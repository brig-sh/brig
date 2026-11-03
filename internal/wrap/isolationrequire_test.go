package wrap

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// logStub is a runtime binary that records every command it is given and
// answers none of them. A test reads the log to see whether the run got as far
// as starting anything.
func logStub(t *testing.T, dir, name string) (bin, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), name+".log")
	bin = filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// startedSomething reports whether the stub was asked to boot a sandbox: a run,
// a create or a start. Anything else, such as asking whether a sandbox is up,
// is reading, which a refusal is entitled to do.
func startedSomething(t *testing.T, log string) bool {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch strings.Fields(line + " x")[0] {
		case "run", "create", "start":
			return true
		}
	}
	return false
}

// loadRequiring resolves a run of the test profile that requires a microVM,
// with the real nerdctl adapter over the stub binary at bin, or over whichever
// runtime Detect finds when bin is empty. Only the binary is fake, so the
// isolation the run resolves is the one the adapter reports on a host.
func loadRequiring(t *testing.T, bin, isolation string) *Config {
	t.Helper()
	isolateState(t)
	t.Setenv("BRIG_RUNTIME", "nerdctl")
	if bin != "" {
		t.Setenv("BRIG_RUNTIME_BIN", bin)
	}
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(testProfile(t, planProfile), Options{Workspace: t.TempDir(), Isolation: isolation}, rt)
	if err != nil {
		t.Fatal(err)
	}
	c.Out, c.Err, c.Progress = io.Discard, io.Discard, io.Discard
	c.Verify = verify.Off
	return c
}

// Each way a run can end up on less than a microVM is refused when the run
// requires one, with exit 7's error, the row that says what it would have been
// and the setting to change. This is the test per downgrade path #30 asks for:
// the row used to say what a run stood on and the run went on regardless.
func TestARunRequiringAMicroVMIsRefusedOnEachDowngradePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		// shim is BRIG_CONTAINERD_RUNTIME; driver is the name the binary goes by.
		shim, driver string
		// fallback leaves BRIG_RUNTIME_BIN unset and puts only docker on PATH,
		// which is how nerdctl's absence takes a run to docker without a word.
		fallback bool
		want     []string
	}{
		{name: "BRIG_CONTAINERD_RUNTIME names runc", shim: "runc", driver: "nerdctl",
			want: []string{"the guest shares the host kernel", "BRIG_CONTAINERD_RUNTIME"}},
		{name: "BRIG_CONTAINERD_RUNTIME names a path to runc", shim: "/usr/bin/runc", driver: "nerdctl",
			want: []string{"the guest shares the host kernel", "BRIG_CONTAINERD_RUNTIME"}},
		{name: "BRIG_CONTAINERD_RUNTIME names the runc shim binary", shim: "containerd-shim-runc-v2", driver: "nerdctl",
			want: []string{"the guest shares the host kernel", "BRIG_CONTAINERD_RUNTIME"}},
		{name: "BRIG_CONTAINERD_RUNTIME names a shim brig cannot place", shim: "io.containerd.kata.v2", driver: "nerdctl",
			want: []string{"cannot tell whether that shim boots a kernel of its own", "BRIG_CONTAINERD_RUNTIME"}},
		{name: "BRIG_RUNTIME_BIN points at docker", driver: "docker",
			want: []string{"cannot tell whether docker has that shim", "BRIG_RUNTIME_BIN"}},
		{name: "nerdctl is missing and docker is taken in its place", driver: "docker", fallback: true,
			want: []string{"cannot tell whether docker has that shim", "install nerdctl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIG_CONTAINERD_RUNTIME", tc.shim)
			dir := t.TempDir()
			bin, log := logStub(t, dir, tc.driver)
			if tc.fallback {
				// Only the stub is on PATH, so there is no nerdctl to find.
				t.Setenv("PATH", dir)
				bin = ""
			}
			c := loadRequiring(t, bin, "microvm")
			if tc.fallback {
				// The case is about the silent fallback, so it has to be one.
				if fr, ok := c.Runtime.(runtime.FallbackReporter); !ok || fr.Fallback() == "" {
					t.Fatalf("the run did not fall back to docker: %T", c.Runtime)
				}
			}

			err := c.checkBackend(c.hypervisor())
			var refusal *runtime.IsolationError
			if !errors.As(err, &refusal) {
				t.Fatalf("checkBackend = %v, want the isolation refusal", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%s", want, err)
				}
			}

			// The run stops before it prepares anything or boots, and the plan
			// says the same, with the reason the run prints.
			plan := c.Plan()
			if plan.Refused == nil || plan.Refused.Reason != err.Error() {
				t.Errorf("the plan does not carry the refusal: %+v", plan.Refused)
			}
			runErr := c.EnsureRunning(creds.Set{})
			if !errors.As(runErr, &refusal) {
				t.Fatalf("EnsureRunning = %v, want the isolation refusal", runErr)
			}
			if startedSomething(t, log) {
				b, _ := os.ReadFile(log)
				t.Errorf("a refused run started something:\n%s", b)
			}
		})
	}
}

// The same runs are not refused for isolation when nothing required it. A
// known shared-kernel shim keeps its own refusal, which is not this one, and
// the others boot as they always did. The requirement is opt-in.
func TestARunNotRequiringAMicroVMIsNotRefusedForIsolation(t *testing.T) {
	for _, tc := range []struct{ name, shim, driver string }{
		{"a shim brig cannot place", "io.containerd.kata.v2", "nerdctl"},
		{"docker", "", "docker"},
		{"runc, which has its own refusal", "runc", "nerdctl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIG_CONTAINERD_RUNTIME", tc.shim)
			bin, _ := logStub(t, t.TempDir(), tc.driver)
			c := loadRequiring(t, bin, "")

			var refusal *runtime.IsolationError
			if err := c.checkBackend(c.hypervisor()); errors.As(err, &refusal) {
				t.Errorf("a run that required nothing was refused for its isolation: %v", err)
			}
		})
	}
}

// The requirement is met by what the row establishes, and nothing else gets in
// its way: urunc on nerdctl is a microVM, and so is hull on every backend.
func TestARunRequiringAMicroVMThatGetsOneIsNotRefused(t *testing.T) {
	t.Run("nerdctl on the default shim", func(t *testing.T) {
		t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
		bin, _ := logStub(t, t.TempDir(), "nerdctl")
		c := loadRequiring(t, bin, "microvm")

		var refusal *runtime.IsolationError
		if err := c.checkBackend(c.hypervisor()); errors.As(err, &refusal) {
			t.Errorf("a microVM was refused: %v", err)
		}
	})
	t.Run("hull", func(t *testing.T) {
		c := envelopeConfig()
		c.Runtime = &hullRecorder{}
		c.needMicroVM = true

		if err := c.checkBackend("hvi"); err != nil {
			t.Errorf("a microVM was refused: %v", err)
		}
	})
}

// Nothing in the environment can weaken the requirement: it has one source, the
// command line, and a variable that looks like a way to say otherwise is not
// read. A shell profile that sets one cannot turn a refusal into a boot.
func TestNoVariableWeakensTheIsolationRequirement(t *testing.T) {
	for _, v := range []string{"BRIG_ISOLATION", "BRIG_REQUIRE_ISOLATION", "BRIG_CLAUDE_CODE_ISOLATION"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.kata.v2")
			t.Setenv(v, "container")
			bin, _ := logStub(t, t.TempDir(), "nerdctl")
			c := loadRequiring(t, bin, "microvm")

			var refusal *runtime.IsolationError
			if err := c.checkBackend(c.hypervisor()); !errors.As(err, &refusal) {
				t.Errorf("%s=container lifted the requirement: %v", v, err)
			}
		})
	}
}

// A runtime that cannot be asked is not one that establishes a microVM, so a
// run that requires one and has none in hand is refused rather than waved
// through the way a check on a missing runtime usually is.
func TestARunRequiringAMicroVMWithNoRuntimeIsRefused(t *testing.T) {
	c := envelopeConfig()
	c.Runtime = nil
	c.needMicroVM = true

	var refusal *runtime.IsolationError
	if err := c.checkBackend("hvi"); !errors.As(err, &refusal) {
		t.Fatalf("checkBackend = %v, want the isolation refusal", err)
	}
}

// A value Load does not know is refused, not read as no requirement: a typo in
// a caller that is not the command line, brigd's request, must not boot the
// sandbox the requirement was written to stop.
func TestLoadRefusesAnIsolationRequirementItDoesNotKnow(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_RUNTIME", "nerdctl")
	bin, _ := logStub(t, t.TempDir(), "nerdctl")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"container", "microVM", "micro-vm", " microvm", "auto", "true"} {
		_, err := Load(testProfile(t, planProfile), Options{Workspace: t.TempDir(), Isolation: v}, rt)
		if err == nil || !strings.Contains(err.Error(), "microvm") {
			t.Errorf("Isolation %q: Load = %v, want a refusal that names microvm", v, err)
		}
	}
}
