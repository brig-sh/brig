//go:build !unix

package renew

import "errors"

// processAlive cannot tell here, so every journal of another process holds
// off a renewal until it is journalLife old.
func processAlive(int) bool { return true }

// processStart cannot tell here.
func processStart(int) (string, error) {
	return "", errors.New("no process start time on this platform")
}

// selfStart returns "", so journals are matched on their pid alone.
func selfStart() string { return "" }
