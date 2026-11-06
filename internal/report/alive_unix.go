//go:build unix

package report

import (
	"errors"
	"syscall"
)

// alive reports whether a process with pid exists. EPERM means it does, as
// someone else's.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
