package wrap

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
)

// lateRuntime is a livenessRuntime whose guest agent is not up when the VMM
// is: the first answers-after-1 probes fail, the way a real guest fails them
// for the seconds between the VMM starting and the agent binding its listener.
type lateRuntime struct {
	livenessRuntime
	answersAfter int
	probes       int
}

func (r *lateRuntime) Probe(runtime.ExecSpec) bool {
	r.probes++
	return r.probes >= r.answersAfter
}

// The number the issue asks for: a boot is timed from the runtime being
// asked to start to the first probe the guest answered, so a guest that needs
// a second probe reports at least one probe interval, and the figure sits under
// the timeout that would have failed the boot.
func TestEnsureRunningMeasuresTheBoot(t *testing.T) {
	rt := &lateRuntime{answersAfter: 2}
	c := livenessConfig(t, &rt.livenessRuntime)
	c.Runtime = rt
	c.ReadyTimeout = 5 * time.Second

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a boot with a late guest was refused: %v", err)
	}
	if rt.probes != 2 {
		t.Errorf("probed %d times, want 2", rt.probes)
	}
	if c.BootTime < 300*time.Millisecond {
		t.Errorf("BootTime = %s, want at least one 300ms probe interval", c.BootTime)
	}
	if c.BootTime >= c.ReadyTimeout {
		t.Errorf("BootTime = %s, not under the %s timeout", c.BootTime, c.ReadyTimeout)
	}
}

// A sandbox found running was not booted here, so there is nothing to report:
// the field stays zero rather than carrying the time a probe of a live guest
// took, which is a different number.
func TestEnsureRunningDoesNotTimeAReusedSandbox(t *testing.T) {
	rt := &livenessRuntime{running: true}
	c := livenessConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if c.BootTime != 0 {
		t.Errorf("a reused sandbox reported a boot time of %s", c.BootTime)
	}
}

// The line is narration, not a warning: nobody acts on it, and the smoke test
// reads the default output line by line. It waits for --verbose.
func TestBootTimeLineWaitsForVerbose(t *testing.T) {
	for _, v := range []Verbosity{Quiet, Normal, Verbose} {
		rt := &livenessRuntime{}
		c := livenessConfig(t, rt)
		progress := &bytes.Buffer{}
		c.Progress = progress
		c.Verbosity = v
		if err := c.EnsureRunning(creds.Set{}); err != nil {
			t.Fatal(err)
		}
		said := strings.Contains(progress.String(), "sandbox ready in")
		if v < Verbose && said {
			t.Errorf("level %d printed the boot time: %q", v, progress.String())
		}
		if v >= Verbose && !said {
			t.Errorf("--verbose did not print the boot time: %q", progress.String())
		}
	}
}

// The timeout is unchanged: a guest that never answers is still refused with
// the same message, and no boot time is recorded for it.
func TestEnsureRunningStillTimesOut(t *testing.T) {
	rt := &lateRuntime{answersAfter: 1 << 30}
	c := livenessConfig(t, &rt.livenessRuntime)
	c.Runtime = rt
	c.ReadyTimeout = 0

	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v, want the readiness refusal", err)
	}
	if c.BootTime != 0 {
		t.Errorf("a boot that timed out recorded %s", c.BootTime)
	}
}
