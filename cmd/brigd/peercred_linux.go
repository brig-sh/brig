//go:build linux

package main

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the uid of the process on the other end of a unix connection.
//
// SO_PEERCRED is the kernel's answer, taken from the socket rather than from
// anything the peer says about itself, so it cannot be spoofed by a client. It
// is read once, at accept, because that is when the connection is either the
// invoking user's to serve or nobody's to refuse; nothing later can change who
// opened it.
func peerUID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials are only available on a unix socket, not a %T", conn)
	}
	raw, err := uc.SyscallConn()
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
	return int(cred.Uid), nil
}
