//go:build darwin || linux

package report

import (
	"context"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// stackGrace is how long a stack tool has to detach after SIGTERM before it
// is killed.
const stackGrace = 3 * time.Second

// resumeVMM sends SIGCONT to the VMM. A test replaces it.
var resumeVMM = func(pid int) { _ = syscall.Kill(pid, syscall.SIGCONT) }

// stackTool runs a tool that stops pid while it reads its threads: eu-stack
// and gdb attach with ptrace, sample suspends the task. At the deadline the
// tool gets SIGTERM, so it can detach, and SIGKILL only after stackGrace. A
// tracer killed while its tracee is stopped can leave the VMM stopped, so
// when the deadline ended the tool the VMM is sent SIGCONT.
func stackTool(ctx context.Context, pid int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var ended atomic.Bool
	cmd.Cancel = func() error {
		ended.Store(true)
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = stackGrace
	out, err := cmd.CombinedOutput()
	if ended.Load() {
		resumeVMM(pid)
	}
	return out, err
}
