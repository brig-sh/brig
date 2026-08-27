package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// livenessRuntime answers the one question the boot decision turns on, and
// counts the boots so a case can tell "refused" from "started a second
// sandbox". Everything else panics through the embedded nil interface, which
// is louder than a stub quietly answering something it was never asked.
type livenessRuntime struct {
	runtime.Runtime
	running    bool
	runningErr error
	// workspace is the directory the guest is pretending to mount, so Output
	// can read back the marker brig wrote there.
	workspace string
	boots     int
	stops     int
	removes   int
}

func (r *livenessRuntime) Kind() string                 { return "hull" }
func (r *livenessRuntime) Running(string) (bool, error) { return r.running, r.runningErr }
func (r *livenessRuntime) Stop(string) error            { r.stops++; return nil }
func (r *livenessRuntime) Remove(string) error          { r.removes++; return nil }
func (r *livenessRuntime) Run(runtime.RunSpec) error    { r.boots++; return nil }
func (r *livenessRuntime) Probe(runtime.ExecSpec) bool  { return true }
func (r *livenessRuntime) LogsHint(name string) string  { return "hull logs " + name }

// Output stands in for the guest reading its own home: a guest that mounts the
// workspace reads back the marker brig put there.
func (r *livenessRuntime) Output(runtime.ExecSpec) (string, error) {
	b, err := os.ReadFile(filepath.Join(r.workspace, markerFile))
	return string(b), err
}

// livenessConfig is a run with nothing in its way: no secret files to deliver,
// no volumes to mount, and no image check, so what a case observes is the boot
// decision alone.
func livenessConfig(t *testing.T, rt *livenessRuntime) *Config {
	t.Helper()
	isolateState(t)
	ws := t.TempDir()
	c := testConfig(t, ws, ws, profile.Profile{
		Name:      "liveness",
		Image:     "img",
		GuestHome: "/home/liveness",
		Mem:       1024,
		CPUs:      1,
	})
	c.Verify = verify.Off
	rt.workspace = ws
	c.Runtime = rt
	return c
}

// The bug in #49. Both adapters folded an exec failure into "not running", so a
// runtime brig could not reach read as a workspace with nothing on it, and
// EnsureRunning booted a second sandbox over the first. Failing to answer is
// not answering no, and booting on it is the dangerous direction.
func TestEnsureRunningRefusesWhenTheRuntimeCannotSayIfItIsUp(t *testing.T) {
	rt := &livenessRuntime{runningErr: errors.New("cannot connect to the daemon")}
	c := livenessConfig(t, rt)

	err := c.EnsureRunning(creds.Set{})
	if err == nil {
		t.Fatal("a runtime that could not answer let the run boot a second sandbox")
	}
	if rt.boots != 0 {
		t.Errorf("booted %d sandboxes on an answer the runtime never gave", rt.boots)
	}
	// The refusal has to name the sandbox it is about and carry the runtime's
	// own explanation, which is the only account of what actually broke.
	if !strings.Contains(err.Error(), c.VMName) {
		t.Errorf("the refusal does not name the sandbox: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot connect to the daemon") {
		t.Errorf("the runtime's explanation was dropped: %v", err)
	}
}

// The ordinary first boot. A genuinely absent sandbox is a plain no, and the
// run must still start one -- refusing on every no would be the opposite bug.
func TestEnsureRunningBootsWhenNothingIsRunning(t *testing.T) {
	rt := &livenessRuntime{running: false}
	c := livenessConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a first boot was refused: %v", err)
	}
	if rt.boots != 1 {
		t.Errorf("booted %d times, want 1", rt.boots)
	}
}

// And a sandbox that is up is still reused rather than rebooted, which is what
// keeps a second `brig run` from throwing away a live session.
func TestEnsureRunningReusesASandboxThatIsUp(t *testing.T) {
	rt := &livenessRuntime{running: true}
	c := livenessConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a running sandbox was not reused: %v", err)
	}
	if rt.boots != 0 {
		t.Errorf("a running sandbox was rebooted %d times", rt.boots)
	}
	if rt.stops != 0 || rt.removes != 0 {
		t.Errorf("a running sandbox mounting this workspace was torn down (%d stops, %d removes)",
			rt.stops, rt.removes)
	}
}
