//go:build darwin

package main

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the uid of the process on the other end of a unix connection.
//
// LOCAL_PEERCRED is darwin's SO_PEERCRED: the kernel's own record of who opened
// the socket, taken from the connection rather than from anything the peer
// claims, so a client cannot lie about it. Read once, at accept, for the reason
// given on the Linux side.
func peerUID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials are only available on a unix socket, not a %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Uid), nil
}
