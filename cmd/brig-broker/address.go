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
		peer, ok := connPeer(c)
		if !ok {
			_ = c.Close()
			continue
		}
		return &addressConn{Conn: c, remote: addressAddr{peer: peer}}, nil
	}
}

// connPeer returns the peer of c, unmapped and with no zone, and false when
// its remote address does not parse.
func connPeer(c net.Conn) (netip.AddrPort, bool) {
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(peer.Addr().Unmap().WithZone(""), peer.Port()), true
}

// registeredListener closes each connection whose peer IP no sandbox
// registered, as soon as it is accepted. It sits under newLimitListener. Any
// pod of the cluster can reach the listener, and a connection that waited
// for its TLS handshake would hold one of the maxConns slots for up to
// readHeaderTimeout. A peer that opens maxConns of them would then keep
// every registered pod of the node out.
type registeredListener struct {
	net.Listener
	// registered reports whether a sandbox is registered with ip, in the
	// spelling canonicalAddr returns.
	registered func(ip string) bool
}

// Accept returns the next connection from a registered peer. The arrival
// check still runs on each request: a sandbox can be removed while its
// connection stays open.
func (l registeredListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if peer, ok := connPeer(c); ok && l.registered(peer.Addr().String()) {
			return c, nil
		}
		_ = c.Close()
	}
}

// listenAddress serves the routes on ln with TLS: ln drops unregistered
// peers in a registeredListener, bounded at maxConns by newLimitListener,
// under tls.NewListener with the brig.internal configuration of ca, wrapped
// in an addressListener. ln is the listener of --listen, or the one handed
// over with --listen-fd.
//
// The wrapper hides *tls.Conn from net/http, so the TLS handshake runs on the
// server's first read of the connection, under ReadHeaderTimeout. r.TLS is
// nil on these requests, and nothing on the route server reads it.
func listenAddress(ln net.Listener, ca *authority, registered func(ip string) bool) net.Listener {
	filtered := registeredListener{Listener: ln, registered: registered}
	return addressListener{tls.NewListener(newLimitListener(filtered, maxConns), ca.serverConfig())}
}

// addressContext marks a connection of the address listener.
func addressContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, arrivalKey{}, arrivalAddress)
}

// canonicalAddr is the spelling of a registered address that the listener
// compares peers with: "" for none, else one IP with no zone, neither
// unspecified nor multicast, an IPv4-mapped IPv6 address unmapped.
//
// A pod never has a loopback or a link-local address. A connection from one
// comes from the node's host network namespace, which every hostNetwork pod,
// the kubelet and the node's daemons share. A sandbox registered with one
// would be bound to all of them, so both are refused. loopback lets a test
// register a loopback address, the only kind a test can dial from.
func canonicalAddr(s string, loopback bool) (string, error) {
	if s == "" {
		return "", nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", fmt.Errorf("the sandbox address %q is not one IP address a pod can have", s)
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || (a.IsLoopback() && !loopback) {
		return "", fmt.Errorf("the sandbox address %q is not one IP address a pod can have", s)
	}
	return a.String(), nil
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
