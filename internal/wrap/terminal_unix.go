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

// termWidth returns the width of the terminal on fd in columns, or 0 when fd is
// not a terminal or the driver reports no size. TIOCGWINSZ fills a struct
// winsize, four unsigned shorts on both operating systems.
func termWidth(fd uintptr) int {
	var ws struct{ row, col, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.col)
}

// foreground returns whether this process is in the foreground process group
// of the terminal on fd. A job sent to the background shares the terminal
// with the shell, and is not. The driver answers only for this process's
// controlling terminal, so any other descriptor reads as false.
func foreground(fd uintptr) bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPGRP,
		uintptr(unsafe.Pointer(&pgrp)))
	return errno == 0 && int(pgrp) == syscall.Getpgrp()
}
