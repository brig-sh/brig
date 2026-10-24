//go:build unix

package renew

import (
	"os"
	"sync"
	"syscall"

	"github.com/brig-sh/brig/internal/proc"
)

// processAlive reports whether pid names a running process of this user.
//
// A journal in this user's store comes from a broker of this user, so a
// process that refuses the signal with EPERM belongs to someone else and is
// not the journal's owner.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// processStart returns the start time the kernel records for pid, the form
// internal/injector records for the broker. No locale changes it, so two
// brokers started under different ones agree on it.
func processStart(pid int) (string, error) { return proc.StartTime(pid) }

// selfStart returns the start time of this process, or "" when the kernel
// cannot tell. A journal with no start time is matched on its pid alone.
var selfStart = sync.OnceValue(func() string {
	start, _ := processStart(os.Getpid())
	return start
})
