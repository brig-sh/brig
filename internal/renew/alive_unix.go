//go:build unix

package renew

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// processStart returns the start time of pid as ps(1) prints it, the form
// internal/injector records for the broker.
func processStart(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(out)), " "), nil
}

// selfStart returns the start time of this process, or "" when ps cannot
// tell. A journal with no start time is matched on its pid alone.
var selfStart = sync.OnceValue(func() string {
	start, _ := processStart(os.Getpid())
	return start
})
