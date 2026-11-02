package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/brig-sh/brig/internal/injector"
)

// This file serves the routes that vend on the link-local address. The AWS
// SDKs' container provider verifies HTTPS with its own CA list and ignores
// AWS_CA_BUNDLE, so it cannot reach brig.internal. It takes plain HTTP from
// loopback and a few link-local addresses alone. The runtime diverts the ECS
// one, injector.LinkLocalAddr, to the sandbox's divert socket, and the broker
// hands the connection to the route handler without TLS. A session token
// crosses the virtual link in clear text there, so the handler serves a
// route that vends and refuses any other before reading its token.

const (
	// linkLocalTransport names the transport on the audit line.
	linkLocalTransport = "link-local"
	// linkLocalArrival starts the remote address of a request that arrived
	// on the link-local address of a divert socket:
	// "link-local:<sandbox>/<guest address>". It tells the handler the
	// request came in clear text.
	linkLocalArrival = "link-local:"
)

// linkLocal is the diverted destination served as plain HTTP.
var linkLocal = netip.MustParseAddrPort(injector.LinkLocalAddr)

// arrivalKey keys the arrival marker that the route servers put on each
// connection's context. The marker is typed, so the clear-text check does
// not depend on how a remote address is spelled.
type arrivalKey struct{}

// arrival is how a connection reached the broker.
type arrival int

const (
	arrivalLoopback arrival = iota + 1
	arrivalBrigInternal
	arrivalLinkLocal
)

// loopbackContext marks a connection of the loopback route server.
func loopbackContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, arrivalKey{}, arrivalLoopback)
}

// divertContext marks a connection a divert socket handed over: in clear
// text on the link-local address, under TLS on brig.internal.
func divertContext(ctx context.Context, c net.Conn) context.Context {
	a := arrivalBrigInternal
	if ac, ok := c.(*arrivedConn); !ok || ac.remote.prefix != divertArrival {
		a = arrivalLinkLocal
	}
	return context.WithValue(ctx, arrivalKey{}, a)
}

// vendOnly is the handler's VendOnly check: a request that arrived in clear
// text is served for a route that vends alone. A request with no marker
// counts as clear text, so a server that forgets to mark its connections
// refuses rather than serves.
func vendOnly(r *http.Request) bool {
	a, _ := r.Context().Value(arrivalKey{}).(arrival)
	return a != arrivalLoopback && a != arrivalBrigInternal
}

// serveLinkLocal serves one connection the diverter handed over for the
// link-local address. Only its port 80 is served, as plain HTTP, by the same
// route server as brig.internal. The connection's slot is held until the
// route server closes it.
func (l *divertSandbox) serveLinkLocal(client net.Conn, pre preamble, a *divertAudit) {
	a.transport = linkLocalTransport
	if pre.dst.Port() != linkLocal.Port() {
		a.check = "link-local.port"
		_ = client.Close()
		return
	}
	conn := &arrivedConn{Conn: client, remote: arrivalAddr{prefix: linkLocalArrival, sandbox: l.id, src: pre.src},
		done: make(chan struct{})}
	if !l.srv.internal.hand(conn) {
		a.check = "stopping"
		_ = conn.Close()
		return
	}
	<-conn.done
}
