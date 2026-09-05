package runtime

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// execHandover and attachHandover are the two ways a built command is handed
// down, kept behind package vars so a test can capture the argv and env each
// receives without a syscall.Exec that never returns. Replace and Attach share
// one builder already -- replaceCmd in each adapter -- and stubbing these is how
// a test proves the two paths cannot drift.
var (
	execHandover   = syscall.Exec
	attachHandover = RunAttached
)

// RunAttached runs argv as a child of brig, inheriting brig's own stdin, stdout
// and stderr, waits for it, and returns its exit status. argv[0] is the binary;
// env is the full child environment. Both adapters build the pair from the very
// function their Replace uses, so the child and the process replacement cannot
// drift.
//
// It is the child-process counterpart of syscall.Exec. Replace hands the
// terminal, the signals and the exit status straight to the runtime and never
// returns, which is the right default: the agent's TUI wants a real tty with
// nothing between it and the keyboard. Attach keeps brig alive across the exec
// so brig can report the outcome on a machine-readable line afterwards, which
// Replace by definition cannot -- there is no brig left once the exec has
// happened.
//
// A child killed by a signal is reported as 128 plus the signal number, the
// convention a shell follows, so a caller mapping the status onto its own exit
// returns the same number a shell would for the same death.
func RunAttached(argv, env []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// Registered before Start so a signal that arrives in the gap between fork
	// and the forwarding goroutine is buffered rather than taking brig down
	// under the default disposition.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	// The child holds the terminal, so it sits in brig's own process group and a
	// Ctrl-C from the tty is delivered by the kernel to BOTH brig and the child
	// at the same instant. brig must therefore neither exit on SIGINT nor
	// forward it: the child already has it, and a second copy -- or brig dying
	// and orphaning the child mid-keystroke -- is exactly the bug this guards
	// against. So SIGINT is caught and dropped, and brig goes on waiting.
	//
	// SIGTERM and SIGHUP are different in kind: they reach brig alone (a `kill`,
	// a hung-up session), not through the tty, so the child never sees them
	// unless brig passes them on. Those are forwarded. SIGWINCH needs nothing --
	// the child owns the tty and the kernel resizes its pty directly. The
	// deferred signal.Stop restores the default handling, so nothing outlives
	// the one child this call waits on.
	//
	// This is the reasoning most likely to be "corrected" wrongly later: do not
	// add SIGINT to the forwarded set, and do not make SIGINT return early.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				switch s {
				case syscall.SIGTERM, syscall.SIGHUP:
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(done)
	return attachedStatus(err)
}

// attachedStatus reads the child's exit status out of what cmd.Wait returned:
// its own exit code when it exited, or 128 plus the signal number when a signal
// killed it. A non-exit error -- the child could not be started or waited on --
// is brig's own failure and is returned as the error, not folded into a status.
func attachedStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return 0, err
}
