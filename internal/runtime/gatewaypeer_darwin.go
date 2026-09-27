package runtime

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID is the pid of the process on the other end of a unix connection,
// as the kernel recorded it. LOCAL_PEERPID is darwin's spelling.
func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var pidErr error
	if err := raw.Control(func(fd uintptr) {
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	return pid, pidErr
}
