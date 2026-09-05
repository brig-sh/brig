//go:build darwin || linux

package wrap

import (
	"syscall"
	"unsafe"
)

// isatty asks the terminal driver for this descriptor's line settings. Only a
// terminal has any, so the ioctl succeeding is the answer -- which is all
// isatty(3) does too.
//
// The request number is the one difference between the two operating systems
// brig runs on: TIOCGETA on darwin, TCGETS on linux. Both are "read the
// termios struct", and syscall already declares the struct and the constant
// for each, so there is nothing to hand-roll and no dependency to add. A GOOS
// brig does not support falls back to terminal_other.go.
func isatty(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, ioctlReadTermios,
		uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}
