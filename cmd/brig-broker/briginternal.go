package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/injector"
)

// This file serves brig.internal to a microVM or a container. The runtime
// answers brig.internal with an address of the sandbox's own gateway, and
// diverts that address on port 443 to the sandbox's divert socket (see
// divert.go). The broker terminates TLS there with the brig.internal
// certificate its CA signs, and hands the connection to the route handler.
// The handler sees the sandbox the socket belongs to in the request's remote
// address, and its arrival check refuses a token of another sandbox.
// docs/broker.md ("Transports") describes it.

// tlsHandshakeTimeout bounds the TLS handshake of a brig.internal
// connection.
const tlsHandshakeTimeout = 10 * time.Second

// divertArrival starts the remote address of a request that arrived on a
// divert socket: "divert:<sandbox>/<guest address>". A request on the
// loopback port has an IP address there, which never starts with it.
const divertArrival = "divert:"

// arrivalAddr is the remote address of a brig.internal connection. It names
// the sandbox whose divert socket the connection arrived on, which the
// arrival check compares with the sandbox the request names.
type arrivalAddr struct {
	sandbox string
	src     netip.AddrPort
}

func (a arrivalAddr) Network() string { return "brig-divert" }

func (a arrivalAddr) String() string { return divertArrival + a.sandbox + "/" + a.src.String() }

// arrivedConn is a brig.internal connection as the route server sees it. It
// reports where it arrived, and closes done when the server is through with
// it, so the slot it holds goes back.
type arrivedConn struct {
	net.Conn
	remote arrivalAddr
	once   sync.Once
	done   chan struct{}
}

func (c *arrivedConn) RemoteAddr() net.Addr { return c.remote }

func (c *arrivedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// connListener is the listener of the brig.internal route server. Its
// connections come from the divert sockets, already through TLS.
type connListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newConnListener() *connListener {
	return &connListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// hand gives c to the server, and reports false when the listener is closed.
func (l *connListener) hand(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.closed:
		return false
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr { return arrivalAddr{sandbox: "brig.internal"} }

// arrival is the arrival check of the route handler, and names the sandbox
// whose divert socket the request arrived on. A request that arrived on a
// divert socket is served only for the sandbox the socket belongs to. A
// request on the loopback port is served only for a sandbox that reaches the
// broker there: one registered with a brig.internal address reaches it on its
// divert socket alone.
func (s *server) arrival(sandbox, remoteAddr string) (string, bool) {
	if rest, ok := strings.CutPrefix(remoteAddr, divertArrival); ok {
		id, _, _ := strings.Cut(rest, "/")
		return id, id == sandbox
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.diverts[sandbox]
	return "", l == nil || !l.internal.IsValid()
}

// serveInternal serves one connection the diverter handed over for the
// sandbox's brig.internal address. Only port 443 is served, with TLS. The
// connection's slot is held until the route server closes it.
func (l *divertSandbox) serveInternal(client net.Conn, pre preamble, a *divertAudit) {
	a.transport = injector.BrigInternal
	if pre.dst.Port() != injector.BrigInternalPort {
		a.check = "brig.internal.port"
		_ = client.Close()
		return
	}
	tc := tls.Server(client, l.srv.ca.serverConfig())
	ctx, cancel := context.WithTimeout(l.ctx, tlsHandshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		a.check = "tls"
		_ = client.Close()
		return
	}
	conn := &arrivedConn{Conn: tc, remote: arrivalAddr{sandbox: l.id, src: pre.src}, done: make(chan struct{})}
	if !l.srv.internal.hand(conn) {
		a.check = "stopping"
		_ = conn.Close()
		return
	}
	<-conn.done
}
