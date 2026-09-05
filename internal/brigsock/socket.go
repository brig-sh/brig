// Package brigsock is the socket-path logic brigd and brig share.
//
// It was package main in cmd/brigd until brig doctor needed the same answers --
// where the daemon listens by default, so it can say whether one is up, and how
// long a path the kernel will bind, so a report can be honest about a path that
// is too long. Neither behaviour changed in the move: the two functions below
// are lifted verbatim, and cmd/brigd now calls them from here rather than
// keeping a second copy that could drift.
package brigsock

import (
	"os"
	"path/filepath"
	"syscall"
)

// MaxPath is the longest path the kernel will bind a unix socket to.
//
// bind copies the path into sun_path, a fixed array in the address struct --
// 104 bytes on macOS, 108 on Linux -- and the last byte has to be the
// terminator, so what fits is one less than the array. Derived from the
// platform's own struct rather than written out, because the two numbers
// differ and a hard-coded 104 would turn away paths Linux binds happily.
const MaxPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// Default is the socket brigd picks when it is not told one, and where that
// choice came from.
func Default() (path, source string) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "brigd.sock"), "XDG_RUNTIME_DIR"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".brig", "brigd.sock"), "your home directory"
}
