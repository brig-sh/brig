package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// hangingHull writes a stand-in for the hull binary that accepts the exec and
// then never answers, which is what a real one does when it reaches the guest
// agent socket before the guest has bound its listener: the VMM accepts the
// connection, the open request goes into nothing, and the frame loop blocks
// with no deadline.
func hangingHull(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	path := filepath.Join(t.TempDir(), "hull")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bug this pins down: waitReady looks bounded -- it holds a deadline and
// polls -- but the deadline is only read between probes. A probe that never
// returns therefore never lets it be read, and `brig run` hangs forever
// without printing anything. Probe must come back on its own.
func TestProbeDoesNotHangWhenTheGuestNeverAnswers(t *testing.T) {
	h := &hull{bin: hangingHull(t)}

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- h.Probe(ExecSpec{Name: "vm", Cmd: []string{"/bin/true"}}) }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("a guest that never answered was reported ready")
		}
		// Comfortably above agentCallTimeout, well under a hang.
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("probe took %s, which is not bounded", elapsed)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Probe never returned: the readiness loop above it can never " +
			"reach its own deadline, so brig hangs with no output")
	}
}

// Same socket, same silence, same requirement: guestMountsWorkspace asks the
// guest a question it is willing to get no answer to, so it must not be the
// thing that blocks the run.
func TestOutputDoesNotHangWhenTheGuestNeverAnswers(t *testing.T) {
	h := &hull{bin: hangingHull(t)}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := h.Output(ExecSpec{Name: "vm", Cmd: []string{"cat", "/marker"}})
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("a guest that never answered returned output %q and no error", r.out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Output never returned")
	}
}
