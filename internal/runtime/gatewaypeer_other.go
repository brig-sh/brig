//go:build !darwin && !linux

package runtime

import (
	"errors"
	"net"
)

// peerPID has no implementation here, so no shared gateway is ever
// signalled on this platform: nobody can say which process listens.
func peerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("the peer of a unix socket cannot be read on this platform")
}
