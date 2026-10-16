package divert

import (
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// SockOpt reads the original destination of a connection that an nftables
// DNAT redirected, from conntrack, with SO_ORIGINAL_DST.
//
// It reads IPv4 alone. The table diverts no IPv6, and the relay listens on
// IPv4.
type SockOpt struct{}

// OriginalDst returns where c was going before the DNAT.
func (SockOpt) OriginalDst(c *net.TCPConn) (netip.AddrPort, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var out netip.AddrPort
	var serr error
	err = raw.Control(func(fd uintptr) {
		// The kernel fills a sockaddr_in: the family, the port in network
		// order, then the address. IPv6Mreq is a 16-byte buffer of the same
		// size, which is how Go code reads this option without cgo.
		m, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			serr = err
			return
		}
		b := m.Multiaddr
		port := uint16(b[2])<<8 | uint16(b[3])
		out = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port)
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	return out, serr
}
