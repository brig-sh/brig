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

// brig doctor asks hull where its boot assets live, and doctor is what gets run
// against a hull that has stopped answering. The question must come back, and
// the answer must be the fallback a hull with no `assets dir` gets.
func TestBootAssetsDirDoesNotHangWhenHullNeverAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
	fallback, err := defaultBootAssetsDir()
	if err != nil {
		t.Fatal(err)
	}
	h := &hull{bin: hangingHull(t)}

	type result struct {
		dir string
		err error
	}
	done := make(chan result, 1)
	go func() {
		dir, _, _, err := BootAssetsDir(h)
		done <- result{dir, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.dir != fallback {
			t.Errorf("a hull that never answered resolved %s, want the fallback %s", r.dir, fallback)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("BootAssetsDir never returned: brig doctor hangs at its boot row on a wedged hull")
	}
}

// The deadline on `hull assets dir` is doctor's alone. A run waits for hull's
// answer: falling back on a slow hull would boot an old bundle left at the
// default, or fetch a second copy, which is the drift #314 is about. Doctor
// is the one that must come back from a wedged hull.
func TestOnlyDoctorBoundsTheAssetsQuestion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	home := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
	fallback, err := defaultBootAssetsDir()
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "store", "assets")
	bin := filepath.Join(t.TempDir(), "hull")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\necho '"+store+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := doctorAssetDirTimeout
	doctorAssetDirTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorAssetDirTimeout = old })
	h := &hull{bin: bin}

	if dir, err := h.assetDir(); err != nil || dir != store {
		t.Errorf("the run path got %q, %v from a slow hull, want its answer %s", dir, err, store)
	}
	if dir, _, _, err := BootAssetsDir(h); err != nil || dir != fallback {
		t.Errorf("doctor got %q, %v from a slow hull, want the fallback %s", dir, err, fallback)
	}
}
