//go:build unix

package runtime

import "syscall"

// mkfifo makes a named pipe at path, for tests of readers that must not open
// one.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
