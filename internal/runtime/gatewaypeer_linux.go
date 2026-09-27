package runtime

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID is the pid of the process on the other end of a unix connection,
// as the kernel recorded it. For a connection to a listening socket,
// SO_PEERCRED is the process that called listen.
func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}
