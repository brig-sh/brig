package ttytest

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// open allocates a pty the way posix_openpt(3), grantpt(3) and unlockpt(3) do:
// open the multiplexer, hand the slave to this user, unlock it, ask for its
// name, open it.
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
	for _, step := range []struct {
		name string
		req  uintptr
	}{{"grant", syscall.TIOCPTYGRANT}, {"unlock", syscall.TIOCPTYUNLK}} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), step.req, 0); errno != 0 {
			return nil, nil, fmt.Errorf("%s the pseudo-terminal: %w", step.name, errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCPTYGNAME,
		uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return nil, nil, fmt.Errorf("name the pseudo-terminal: %w", errno)
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	s, err := os.OpenFile(string(name[:end]), os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return m, s, nil
}
