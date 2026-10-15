// Package proc reads when a process started, so a pid recorded in a file can
// be told apart from an unrelated process that reuses it.
//
// The start time is a number the kernel keeps, never the text ps(1) prints:
// that text follows the locale of whoever runs ps, and two processes of one
// user can run under different ones.
package proc

import "fmt"

// StartTime returns the start time of pid, in a form that is equal for the
// same process every time it is read and differs for a later process that
// reuses the pid. Callers compare it and never parse it.
func StartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("cannot read the start time of pid %d: not a process", pid)
	}
	start, err := startTime(pid)
	if err != nil {
		return "", fmt.Errorf("cannot read the start time of pid %d: %w", pid, err)
	}
	return start, nil
}
