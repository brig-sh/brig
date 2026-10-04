package egress

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// freebind lets the resolver bind the bridge address before the bridge
// exists. CNI creates the bridge and its address when the first container
// joins, and the resolver has to be listening before the guest boots.
func freebind(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
