package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// expireWhenReady makes the deadline of every agent call in this test fall
// as soon as the file ready exists, which the stand-in for hull writes once
// it has set its trap.
//
// The tests used to shorten agentCallTimeout to half a second instead. On a
// loaded machine the shell had not reached its trap by then, the signal
// killed it before it could record anything, and the test failed for a
// reason that had nothing to do with which signal brig sends. Waiting for
// the stand-in rather than for a time removes the race: the call always
// expires, and never before the stand-in can tell one signal from another.
func expireWhenReady(t *testing.T, ready string) {
	t.Helper()
	old := agentCallDeadline
	agentCallDeadline = func(parent context.Context) (context.Context, context.CancelFunc) {
		d := &readyDeadline{Context: parent, done: make(chan struct{}), stop: make(chan struct{})}
		go func() {
			tick := time.NewTicker(2 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(ready); err == nil {
					close(d.done)
					return
				}
				select {
				case <-d.stop:
					return
				case <-tick.C:
				}
			}
		}()
		var once sync.Once
		return d, func() { once.Do(func() { close(d.stop) }) }
	}
	t.Cleanup(func() { agentCallDeadline = old })
}

// readyDeadline is a context whose deadline passes when done is closed. To
// the command it bounds it is a deadline like any other: Done closes and Err
// reports context.DeadlineExceeded.
type readyDeadline struct {
	context.Context
	done chan struct{}
	stop chan struct{}
}

func (d *readyDeadline) Done() <-chan struct{} { return d.done }

func (d *readyDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// signalHull writes a stand-in for hull that hangs in exec, the way a real
// one does while it waits for the guest's agent, and writes TERM to termed
// if it is sent SIGTERM. SIGKILL cannot be caught, so termed stays absent
// when hull is killed outright. The trap is set first and ready written
// after it, so a test can tell a hull killed outright from one signalled
// before it could catch anything. The sleep it waits on is killed at
// cleanup, since a hull killed outright leaves it behind.
func signalHull(t *testing.T) (bin, termed, ready string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin, termed, ready = filepath.Join(dir, "hull"), filepath.Join(dir, "termed"), filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "sleep.pid")
	script := "#!/bin/sh\n" +
		"trap 'echo TERM > \"" + termed + "\"; [ -n \"$pid\" ] && kill $pid; exit 143' TERM\n" +
		": > '" + ready + "'\n" +
		"sleep 300 &\n" +
		"pid=$!\n" +
		"echo $pid > '" + pidFile + "'\n" +
		"wait $pid\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return bin, termed, ready
}

// trapWasSet fails the test unless signalHull's stand-in had set its trap
// before the call ended it, so what termed says is about the signal sent.
func trapWasSet(t *testing.T, ready string) {
	t.Helper()
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("the stand-in was ended before it set its trap, so the signal cannot be told: %v", err)
	}
}

// A call that times out on Linux ends hull with SIGTERM (#575). There the
// shim's `urunc exec` holds the guest connection, and a hull killed outright
// leaves the command running in the guest, its exec process in the shim and
// its FIFOs on the host. hull ends all three on SIGTERM once the exec has
// started. The stand-in exits on TERM at once, so this checks the signal
// brig sends, not what hull does with it while it waits for the guest's
// agent. Probe, Output and Feed all build their command in agentCall.
func TestLinuxHullEndsATimedOutCallWithSIGTERM(t *testing.T) {
	bin, termed, ready := signalHull(t)
	expireWhenReady(t, ready)
	h := &hull{bin: bin, linux: true}

	if h.Probe(ExecSpec{Name: "vm", Cmd: []string{"/bin/true"}}) {
		t.Fatal("a guest that never answered was reported ready")
	}
	trapWasSet(t, ready)
	if b, err := os.ReadFile(termed); err != nil || strings.TrimSpace(string(b)) != "TERM" {
		t.Errorf("hull was not sent SIGTERM when the call timed out: %q, %v", b, err)
	}
}

// Output sets its own Cancel, to tell a guest that never answered from a
// command that failed. It must keep the SIGTERM agentCall chose for hull on
// Linux, and still report the timeout as one.
func TestLinuxHullOutputEndsATimedOutCallWithSIGTERM(t *testing.T) {
	bin, termed, ready := signalHull(t)
	expireWhenReady(t, ready)
	h := &hull{bin: bin, linux: true}

	_, err := h.Output(ExecSpec{Name: "vm", Cmd: []string{"/bin/true"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a guest that never answered was not reported as a timeout: %v", err)
	}
	trapWasSet(t, ready)
	if b, rerr := os.ReadFile(termed); rerr != nil || strings.TrimSpace(string(b)) != "TERM" {
		t.Errorf("hull was not sent SIGTERM when Output timed out: %q, %v", b, rerr)
	}
}

// The macOS hull is still killed outright. Nothing leaks there, since the
// guest ends a session whose connection drops, and SIGTERM would cost a
// second on every probe that times out: hull passes it to the guest command,
// and with no agent listening yet it stays up until the backstop kills it.
func TestMacOSHullKillsATimedOutCall(t *testing.T) {
	bin, termed, ready := signalHull(t)
	expireWhenReady(t, ready)
	h := &hull{bin: bin}

	if h.Probe(ExecSpec{Name: "vm", Cmd: []string{"/bin/true"}}) {
		t.Fatal("a guest that never answered was reported ready")
	}
	// Without the trap in place, a SIGTERM would kill it as surely as a
	// SIGKILL, and the check below would pass whatever was sent.
	trapWasSet(t, ready)
	if _, err := os.Stat(termed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the macOS hull was sent SIGTERM when the call timed out: %v", err)
	}
}

// SIGTERM asks, and a hull that does not answer it is still killed, so the
// call keeps its promise to return. Without the backstop a hull ignoring
// SIGTERM would hang the readiness loop, which is the bug agentCall exists
// to prevent.
func TestLinuxHullCallReturnsWhenHullIgnoresSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin, ready := filepath.Join(dir, "hull"), filepath.Join(dir, "ready")
	// The deadline waits for ready, written once SIGTERM is ignored. A
	// signal that came before that would end the shell, and the call would
	// return without the backstop this test is about. exec keeps the
	// ignored SIGTERM, and leaves nothing behind once killed.
	script := "#!/bin/sh\ntrap '' TERM\n: > '" + ready + "'\nexec sleep 300\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	expireWhenReady(t, ready)
	h := &hull{bin: bin, linux: true}

	done := make(chan bool, 1)
	go func() { done <- h.Probe(ExecSpec{Name: "vm", Cmd: []string{"/bin/true"}}) }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("a guest that never answered was reported ready")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Probe never returned from a hull that ignores SIGTERM")
	}
}
