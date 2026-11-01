package main

import "syscall"

// hasFileCaps reports whether a binary carries file capabilities, which some
// distributions give newuidmap in place of the setuid bit.
func hasFileCaps(path string) bool {
	n, err := syscall.Getxattr(path, "security.capability", nil)
	return err == nil && n > 0
}
