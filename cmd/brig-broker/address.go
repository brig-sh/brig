package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// This file serves the routes on a TCP address, for a broker on a Kubernetes
// node. There is no divert socket there: a pod reaches brig.internal over the
// network, so the listener speaks TLS with the brig.internal certificate the
// CA signs, and the pod trusts the CA from the registration answer. The peer
// IP of each connection binds the request to the sandbox registered with
// that address, the way a divert socket binds it to its own sandbox.

const (
	// addressArrival starts the remote address of a request that arrived on
	// the address listener: "address:<ip>:<port>". The rest is the peer the
	// TCP connection came from.
	addressArrival = "address:"
	// arrivalAddress marks a connection of the address listener. It is TLS,
	// so vendOnly does not treat it as clear text.
	arrivalAddress arrival = arrivalLinkLocal + 1
)

// addressAddr is the remote address of a connection on the address listener.
// It carries the peer in a spelling server.arrival recognizes.
type addressAddr struct {
	peer netip.AddrPort
}

// Network names the listener, so the address is not taken for a plain TCP
// one.
func (a addressAddr) Network() string { return "brig-address" }

// String is "address:<ip>:<port>", with the IP unmapped.
func (a addressAddr) String() string { return addressArrival + a.peer.String() }

// addressConn is a connection of the address listener, whose RemoteAddr
// names the peer under addressArrival.
type addressConn struct {
	net.Conn
	remote addressAddr
}

// RemoteAddr returns the peer under addressArrival.
func (c *addressConn) RemoteAddr() net.Addr { return c.remote }

// addressListener wraps each connection it accepts in an addressConn.
type addressListener struct {
	net.Listener
}

// Accept returns the next connection with its peer parsed and unmapped. A
// connection whose peer cannot be parsed is closed, and the next one is
// accepted: a request no sandbox can be bound to is not served.
func (l addressListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err != nil {
			_ = c.Close()
			continue
		}
		peer = netip.AddrPortFrom(peer.Addr().Unmap().WithZone(""), peer.Port())
		return &addressConn{Conn: c, remote: addressAddr{peer: peer}}, nil
	}
}

// listenAddress serves the routes on listen with TLS: a TCP listener bounded
// at maxConns by newLimitListener, under tls.NewListener with the
// brig.internal configuration of ca, wrapped in an addressListener.
//
// The wrapper hides *tls.Conn from net/http, so the TLS handshake runs on the
// server's first read of the connection, under ReadHeaderTimeout. r.TLS is
// nil on these requests, and nothing on the route server reads it.
func listenAddress(listen string, ca *authority) (net.Listener, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("cannot serve routes on %s: %w", listen, err)
	}
	return addressListener{tls.NewListener(newLimitListener(ln, maxConns), ca.serverConfig())}, nil
}

// addressContext marks a connection of the address listener.
func addressContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, arrivalKey{}, arrivalAddress)
}

// canonicalAddr is the spelling of a registered address that the listener
// compares peers with: "" for none, else one IP with no zone, neither
// unspecified nor multicast, an IPv4-mapped IPv6 address unmapped. Loopback
// is allowed.
func canonicalAddr(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
		return "", fmt.Errorf("the sandbox address %q is not one IP address a pod can have", s)
	}
	return a.Unmap().String(), nil
}

// peerAddr returns the canonical spelling of the peer IP in the remote
// address of a request that arrived on the address listener, and false for a
// remote address that is not one.
func peerAddr(remoteAddr string) (string, bool) {
	rest, ok := strings.CutPrefix(remoteAddr, addressArrival)
	if !ok {
		return "", false
	}
	ap, err := netip.ParseAddrPort(rest)
	if err != nil {
		return "", true
	}
	return ap.Addr().Unmap().WithZone("").String(), true
}
