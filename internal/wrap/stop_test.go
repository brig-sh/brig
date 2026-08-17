package wrap

import (
	"errors"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// stubRuntime answers only what Stop asks. Everything else panics through the
// embedded nil interface, which is louder than a stub that quietly returns
// nothing.
type stubRuntime struct {
	runtime.Runtime
	stopErr error
	running bool
	stops   int
}

func (s *stubRuntime) Stop(string) error   { s.stops++; return s.stopErr }
func (s *stubRuntime) Running(string) bool { return s.running }
func (s *stubRuntime) LogsHint(name string) string {
	return "hull logs " + name
}

// `brig stop` discarded the runtime's error and returned nil, so a sandbox
// that would not die reported success -- and the user believed a VM holding a
// forwarded credential was gone while it was still running.
func TestStopReportsASandboxThatWouldNotStop(t *testing.T) {
	rt := &stubRuntime{stopErr: errors.New("hull: instance is busy"), running: true}
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Runtime = rt

	err := c.Stop()
	if err == nil {
		t.Fatal("a sandbox that would not stop was reported as stopped")
	}
	// The runtime's own explanation is the only account of what happened, so
	// it has to survive into the error.
	if !errors.Is(err, rt.stopErr) {
		t.Errorf("the runtime's error was replaced rather than wrapped: %v", err)
	}
	if !strings.Contains(err.Error(), c.VMName) {
		t.Errorf("the error does not name the sandbox: %v", err)
	}
}

// The end state is what decides. A stop that failed against an instance which
// is no longer running asked for the state it got, and reporting that as a
// failure would make `brig stop` on an already-stopped sandbox an error.
func TestStopIsSilentWhenTheSandboxIsGoneAnyway(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Runtime = &stubRuntime{stopErr: errors.New("no such instance"), running: false}
	if err := c.Stop(); err != nil {
		t.Errorf("stopping a sandbox that was not running failed: %v", err)
	}
}

func TestStopSucceedsWhenTheRuntimeStopsIt(t *testing.T) {
	rt := &stubRuntime{running: true}
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Runtime = rt
	if err := c.Stop(); err != nil {
		t.Errorf("a clean stop reported an error: %v", err)
	}
	if rt.stops != 1 {
		t.Errorf("the runtime was asked to stop %d times, want 1", rt.stops)
	}
}
