package ttytest

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// open allocates a pty. Linux spells the same three steps differently: unlock
// with TIOCSPTLCK, ask for the slave's number rather than its name, and build
// the /dev/pts path from it.
func open() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = m.Close()
		}
	}()
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		return nil, nil, fmt.Errorf("unlock the pseudo-terminal: %w", errno)
	}
	var number int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN,
		uintptr(unsafe.Pointer(&number))); errno != 0 {
		return nil, nil, fmt.Errorf("number the pseudo-terminal: %w", errno)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return m, s, nil
}
