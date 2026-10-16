//go:build !linux

package divert

import (
	"errors"
	"net"
	"net/netip"
)

// SockOpt reads the original destination of a redirected connection. Only
// Linux has SO_ORIGINAL_DST, so here it always fails and the relay resets
// every connection.
type SockOpt struct{}

// OriginalDst returns an error.
func (SockOpt) OriginalDst(*net.TCPConn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("SO_ORIGINAL_DST is Linux only")
}
