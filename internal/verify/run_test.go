package verify

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// grandchildPID reads the pid of the sleep a test script leaves behind, from
// the file the script writes it to. The script is the shape a hung cosign
// takes: cosign waiting on a credential helper it spawned.
func grandchildPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file holds %q", b)
			}
			// If the test fails, the sleep is not left for the next run.
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the script never wrote the grandchild's pid")
	return 0
}

// gone reports whether pid has exited. A process killed by a signal stays a
// zombie until its new parent reaps it, and in a container with no init
// nothing does, so a zombie counts as gone: it runs nothing.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// ps exits non-zero when the pid is not there.
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the process cosign spawned (pid %d) is still running", pid)
}

// A cosign cut off at the deadline takes whatever it spawned with it. A
// credential helper that cosign waits on is the ordinary cause of the hang, and
// killing cosign alone left the helper running after every failed boot.
func TestRunLeavesNoGrandchildBehindOnTimeout(t *testing.T) {
	orig := cosignTimeout
	t.Cleanup(func() { cosignTimeout = orig })
	cosignTimeout = 200 * time.Millisecond

	pidFile := filepath.Join(t.TempDir(), "pid")
	if _, err := run("/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait"); err == nil {
		t.Fatal("a hanging cosign returned no error")
	}
	waitGone(t, grandchildPID(t, pidFile))
}

// A timeout is told apart from any other failure by the deadline, not by what
// cosign printed, so the caller can say that cosign did not answer.
func TestRunReportsATimeoutAsTimedOut(t *testing.T) {
	orig := cosignTimeout
	t.Cleanup(func() { cosignTimeout = orig })
	cosignTimeout = 200 * time.Millisecond

	if _, err := run("false"); err == nil || errors.Is(err, errTimedOut) {
		t.Errorf("a cosign that exits non-zero at once reads as a timeout: %v", err)
	}
	if _, err := run("/bin/sh", "-c", "sleep 5 & wait"); !errors.Is(err, errTimedOut) {
		t.Errorf("a cosign cut off at the deadline does not read as a timeout: %v", err)
	}
}

// cosign runs in a process group of its own, so the timeout can kill all of it.
// That takes it out of brig's foreground group, and a Ctrl-C from the terminal
// reaches brig alone. So run catches it, kills cosign's group, and raises the
// signal again once the group is gone, so brig dies of it as it did before.
func TestRunKillsTheGroupOnInterruptAndReraises(t *testing.T) {
	origT := cosignTimeout
	origN, origS, origR := notifySignals, stopSignals, reraise
	t.Cleanup(func() {
		cosignTimeout = origT
		notifySignals, stopSignals, reraise = origN, origS, origR
	})
	cosignTimeout = 20 * time.Second

	registered := make(chan chan<- os.Signal, 1)
	notifySignals = func(c chan<- os.Signal, _ ...os.Signal) { registered <- c }
	stopSignals = func(chan<- os.Signal) {}
	var mu sync.Mutex
	var raised []os.Signal
	reraise = func(s os.Signal) {
		mu.Lock()
		defer mu.Unlock()
		raised = append(raised, s)
	}

	pidFile := filepath.Join(t.TempDir(), "pid")
	finished := make(chan struct{})
	var runErr error
	go func() {
		defer close(finished)
		_, runErr = run("/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	}()
	// The seams are put back only after run has returned.
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(25 * time.Second):
		}
	})

	var sigs chan<- os.Signal
	select {
	case sigs = <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("run registered for no signal")
	}
	pid := grandchildPID(t, pidFile)
	sigs <- syscall.SIGINT

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("run kept waiting on cosign after an interrupt")
	}
	if errors.Is(runErr, errTimedOut) {
		t.Errorf("an interrupt reads as a timeout: %v", runErr)
	}
	waitGone(t, pid)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(raised, []os.Signal{syscall.SIGINT}) {
		t.Errorf("raised %v again, want SIGINT once", raised)
	}
}

// A signal brig was started with ignored stays ignored. Notify stops a signal
// being ignored, and Stop puts the ignore back. A nohup'd brig that caught
// SIGHUP then raises it again into nothing and carries on after its cosign is
// killed.
func TestRunLeavesAnIgnoredSignalAlone(t *testing.T) {
	origN, origS, origR, origI := notifySignals, stopSignals, reraise, signalIgnored
	t.Cleanup(func() {
		notifySignals, stopSignals, reraise, signalIgnored = origN, origS, origR, origI
	})
	signalIgnored = func(s os.Signal) bool { return s == syscall.SIGHUP }
	var asked []os.Signal
	notifySignals = func(_ chan<- os.Signal, sig ...os.Signal) { asked = append(asked, sig...) }
	stopSignals = func(chan<- os.Signal) {}
	reraise = func(s os.Signal) { t.Errorf("raised %v with no signal received", s) }

	if _, err := run("true"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(asked, os.Signal(syscall.SIGHUP)) {
		t.Errorf("run asked for SIGHUP, which the process ignores: %v", asked)
	}
	if !slices.Contains(asked, os.Signal(syscall.SIGINT)) {
		t.Errorf("run did not ask for SIGINT: %v", asked)
	}
}

// With every forwarded signal ignored there is nothing to ask for, and run
// asks for nothing. Notify called with no signals relays every signal, so a
// terminal resize would kill cosign and read as brig dying.
func TestRunAsksForNoSignalWhenAllAreIgnored(t *testing.T) {
	origN, origS, origR, origI := notifySignals, stopSignals, reraise, signalIgnored
	t.Cleanup(func() {
		notifySignals, stopSignals, reraise, signalIgnored = origN, origS, origR, origI
	})
	signalIgnored = func(os.Signal) bool { return true }
	calls := 0
	notifySignals = func(_ chan<- os.Signal, sig ...os.Signal) {
		calls++
		if len(sig) == 0 {
			t.Error("run called Notify with no signals, which relays all of them")
		}
	}
	stopSignals = func(chan<- os.Signal) {}
	reraise = func(s os.Signal) { t.Errorf("raised %v with no signal received", s) }

	if _, err := run("true"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("run registered for signals %d times with all of them ignored", calls)
	}
}

// Skipping the signal handling does not skip the deadline. With every forwarded
// signal ignored, a hung cosign still reads as a timeout and still takes what
// it spawned with it.
func TestRunTimesOutWhenAllSignalsAreIgnored(t *testing.T) {
	origT, origN, origS, origI := cosignTimeout, notifySignals, stopSignals, signalIgnored
	t.Cleanup(func() {
		cosignTimeout, notifySignals, stopSignals, signalIgnored = origT, origN, origS, origI
	})
	cosignTimeout = 200 * time.Millisecond
	signalIgnored = func(os.Signal) bool { return true }
	notifySignals = func(chan<- os.Signal, ...os.Signal) {}
	stopSignals = func(chan<- os.Signal) {}

	pidFile := filepath.Join(t.TempDir(), "pid")
	_, err := run("/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	if !errors.Is(err, errTimedOut) {
		t.Errorf("a cosign cut off at the deadline does not read as a timeout: %v", err)
	}
	waitGone(t, grandchildPID(t, pidFile))
}

// Ctrl-\ ends brig too, and it no longer reaches cosign from the terminal. The
// Go runtime dumps its goroutines and exits on SIGQUIT without killing cosign's
// group, so unless run catches it, cosign and its helper outlive brig.
func TestRunAsksForEverySignalThatEndsBrig(t *testing.T) {
	origN, origS, origR, origI := notifySignals, stopSignals, reraise, signalIgnored
	t.Cleanup(func() {
		notifySignals, stopSignals, reraise, signalIgnored = origN, origS, origR, origI
	})
	signalIgnored = func(os.Signal) bool { return false }
	var asked []os.Signal
	notifySignals = func(_ chan<- os.Signal, sig ...os.Signal) { asked = append(asked, sig...) }
	stopSignals = func(chan<- os.Signal) {}
	reraise = func(s os.Signal) { t.Errorf("raised %v with no signal received", s) }

	if _, err := run("true"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		if !slices.Contains(asked, s) {
			t.Errorf("run did not ask for %v: %v", s, asked)
		}
	}
}
