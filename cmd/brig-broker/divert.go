package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/divert"
	"github.com/brig-sh/brig/internal/injector"
)

// This file serves the divert socket of a sandbox. A runtime diverts each
// guest connection to a diverted destination to a unix socket of the
// sandbox, with a one-line preamble that names where the guest was going.
// The broker reads the preamble and serves the connection. A connection to
// the sandbox's brig.internal address is served in briginternal.go, and one
// to the link-local address of the routes that vend in linklocal.go. Any
// other connection goes to the divert handler of a route mode that claims it,
// and is closed when none does. The contract with the diverter is in
// docs/broker.md ("Transports").

const (
	// maxDivertConns bounds the diverted connections of one sandbox open at
	// once. A connection over it is closed, and the diverter resets the
	// guest's.
	maxDivertConns = 64
	// maxHostDivertConns bounds the diverted connections of every sandbox
	// together.
	maxHostDivertConns = 512
	// preambleTimeout bounds the wait for the preamble.
	preambleTimeout = 5 * time.Second
)

// hostDivertSlots holds a slot for each diverted connection open on the
// host, up to maxHostDivertConns. One broker serves the host. Tests replace
// it.
var hostDivertSlots = make(chan struct{}, maxHostDivertConns)

// preamble is the first line a diverter writes.
type preamble struct {
	dst, src netip.AddrPort
}

// readPreamble reads the preamble one byte at a time, so no byte of the
// guest's stream after it is consumed, and parses it as divert.ParsePreamble
// does. It reads at most divert.MaxPreamble bytes.
func readPreamble(r io.Reader) (preamble, error) {
	var buf [divert.MaxPreamble]byte
	for n := 0; n < len(buf); n++ {
		if _, err := io.ReadFull(r, buf[n:n+1]); err != nil {
			return preamble{}, err
		}
		if buf[n] == '\n' {
			dst, src, err := divert.ParsePreamble(buf[:n+1])
			return preamble{dst: dst, src: src}, err
		}
	}
	return preamble{}, fmt.Errorf("the divert preamble is longer than %d bytes", divert.MaxPreamble)
}

// divertMode is how the broker serves a route mode besides endpoint. A
// build that registers such a mode (see internal/routemode) adds one to
// divertModes from an init function.
type divertMode struct {
	// check refuses the settings a registration of sandbox id carries for
	// the mode, before anything is registered.
	check func(id string, settings json.RawMessage) error
	// open starts serving the mode for sandbox id, once the route handler
	// holds its routes.
	open func(s *server, id string, settings json.RawMessage) (divertHandler, error)
}

// divertModes are the route modes besides endpoint that this build's broker
// serves.
var divertModes = map[v1alpha1.RouteMode]divertMode{}

// divertHandler serves the diverted connections of one sandbox that are not
// brig.internal, for one route mode.
type divertHandler interface {
	// claims returns whether the handler serves a connection to dst.
	claims(dst netip.AddrPort) bool
	// serve serves one connection to dst after its preamble. It owns
	// client, and fills in a.
	serve(ctx context.Context, client net.Conn, dst netip.AddrPort, a *divertAudit)
	// close stops serving, and closes what open opened. It does not wait
	// for a serve in flight: the client of every serve is closed when the
	// routes that opened the handler are replaced or removed, or the socket
	// closes, and ctx is done then too.
	close()
}

// lookupHost resolves a host name on the host.
func lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialUpstream dials an address from the host, for a divert handler.
func dialUpstream(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// divertSandbox is the divert socket of one sandbox and what serves it.
//
// One socket serves a sandbox's routes and its guest services. The routes
// hold it when they reach brig.internal or have a route mode besides
// endpoint; a service holds it while it serves the guest at brig.internal.
// It opens with the first holder and closes with the last, so a change of
// routes keeps the guest's open connections to a service.
type divertSandbox struct {
	srv  *server
	id   string
	path string
	ln   net.Listener
	// internal is the address the sandbox's guest resolves brig.internal
	// to, or the zero Addr for a sandbox that does not reach it.
	internal netip.Addr
	// mu guards routes, services, handlers and routesCtx. A registration of routes or
	// of a service changes them while the socket serves connections. When
	// s.mu is needed as well, it is taken first.
	mu sync.Mutex
	// routes reports that the sandbox's routes hold the socket. Only then is
	// the link-local address served.
	routes bool
	// services are the kinds of the guest services that hold the socket.
	services map[string]struct{}
	// handlers serve the sandbox's route modes besides endpoint, in mode
	// order.
	handlers []divertHandler
	// routesCtx is done when the routes it was made for are replaced or
	// removed, and closes the connections they serve: the link-local ones
	// and those of a route mode. It also ends the route requests on
	// brig.internal (see routesRequest), whose connection a service shares.
	// A service can keep the socket open past the routes, so l.ctx alone
	// would leave them served.
	routesCtx    context.Context
	routesCancel context.CancelFunc
	sem          chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
}

// openDivert listens on the divert socket of sandbox id, and opens the
// divert handler of each of modes with its settings. internal is the
// sandbox's brig.internal address, or the zero Addr. routes reports that the
// sandbox's routes hold the socket. The socket is 0600 in a 0700 directory.
func (s *server) openDivert(id string, internal netip.Addr, routes bool, modes []v1alpha1.RouteMode,
	settings map[v1alpha1.RouteMode]json.RawMessage) (*divertSandbox, error) {
	sock, err := injector.DivertSocket(s.cfg.dir, id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(sock)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("refusing the divert socket directory %s: it is a link, not a directory, "+
			"not yours, or not mode 0700", dir)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	handlers, err := s.openHandlers(id, modes, settings)
	if err != nil {
		return nil, err
	}
	closeHandlers := func() { closeAll(handlers) }
	ln, err := net.Listen("unix", sock)
	if err != nil {
		closeHandlers()
		return nil, fmt.Errorf("cannot open the divert socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		closeHandlers()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	routesCtx, routesCancel := context.WithCancel(ctx)
	l := &divertSandbox{srv: s, id: id, path: sock, ln: ln, internal: internal, routes: routes,
		services: map[string]struct{}{}, handlers: handlers, routesCtx: routesCtx, routesCancel: routesCancel,
		sem: make(chan struct{}, maxDivertConns), ctx: ctx, cancel: cancel}
	go l.serve()
	return l, nil
}

// openHandlers opens the divert handler of each of modes with its settings,
// and closes those it opened when one fails.
func (s *server) openHandlers(id string, modes []v1alpha1.RouteMode,
	settings map[v1alpha1.RouteMode]json.RawMessage) ([]divertHandler, error) {
	var handlers []divertHandler
	for _, m := range modes {
		h, err := divertModes[m].open(s, id, settings[m])
		if err != nil {
			closeAll(handlers)
			return nil, err
		}
		handlers = append(handlers, h)
	}
	return handlers, nil
}

func closeAll(handlers []divertHandler) {
	for _, h := range handlers {
		h.close()
	}
}

// setRoutes replaces what the sandbox's routes serve on the socket, closes
// the connections the old routes serve, and closes the handlers it replaces.
// The listener and the connections of the guest services stay.
func (l *divertSandbox) setRoutes(routes bool, handlers []divertHandler) {
	l.mu.Lock()
	old, oldCancel := l.handlers, l.routesCancel
	l.routes, l.handlers = routes, handlers
	l.routesCtx, l.routesCancel = context.WithCancel(l.ctx)
	l.mu.Unlock()
	oldCancel()
	closeAll(old)
}

// hold records that a guest service of kind holds the socket.
func (l *divertSandbox) hold(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.services[kind] = struct{}{}
}

// release records that the guest service of kind no longer holds the
// socket. Closing a socket nothing holds is the caller's.
func (l *divertSandbox) release(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.services, kind)
}

// held reports whether the routes or any guest service hold the socket.
func (l *divertSandbox) held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.routes || len(l.services) > 0
}

// serviceKinds names the kinds of the guest services that hold the socket,
// sorted.
func (l *divertSandbox) serviceKinds() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.services))
}

// holders names what holds the socket: "routes" first if the routes do,
// then the kinds of the guest services, sorted.
func (l *divertSandbox) holders() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var names []string
	if l.routes {
		names = append(names, "routes")
	}
	return append(names, slices.Sorted(maps.Keys(l.services))...)
}

// close stops the listener, closes every connection of the sandbox, removes
// the socket and closes the handlers.
func (l *divertSandbox) close() {
	l.cancel()
	_ = l.ln.Close()
	_ = os.Remove(l.path)
	l.setRoutes(false, nil)
}

func (l *divertSandbox) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || l.ctx.Err() != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case l.sem <- struct{}{}:
		default:
			l.srv.writeDivertAudit(&divertAudit{sandbox: l.id, check: "capacity"}, 0)
			_ = c.Close()
			continue
		}
		slots := hostDivertSlots
		select {
		case slots <- struct{}{}:
		default:
			<-l.sem
			l.srv.writeDivertAudit(&divertAudit{sandbox: l.id, check: "capacity.host"}, 0)
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-slots; <-l.sem }()
			l.handle(c)
		}()
	}
}

// handle serves one diverted connection.
func (l *divertSandbox) handle(client net.Conn) {
	start := time.Now()
	a := &divertAudit{sandbox: l.id}
	defer func() { l.srv.writeDivertAudit(a, time.Since(start)) }()
	stop := context.AfterFunc(l.ctx, func() { _ = client.Close() })
	defer stop()

	_ = client.SetReadDeadline(time.Now().Add(preambleTimeout))
	pre, err := readPreamble(client)
	if err != nil {
		a.check = "preamble"
		_ = client.Close()
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	a.dst = pre.dst.String()
	// The brig.internal address is the sandbox's own gateway, never an
	// upstream. A connection to it is served here or closed, and never
	// dialed.
	if l.internal.IsValid() && pre.dst.Addr().Unmap() == l.internal {
		l.serveInternal(client, pre, a)
		return
	}
	l.mu.Lock()
	routes, handlers, routesCtx := l.routes, l.handlers, l.routesCtx
	l.mu.Unlock()
	// What follows is the routes', and ends with them.
	stopRoutes := context.AfterFunc(routesCtx, func() { _ = client.Close() })
	defer stopRoutes()
	// The link-local address is the broker's own for the routes that vend,
	// never an upstream, on every port: a divert handler never claims it. A
	// socket that guest services alone hold serves no route there, and
	// closes it.
	if pre.dst.Addr().Unmap() == linkLocal.Addr() {
		if !routes {
			a.check = "port"
			_ = client.Close()
			return
		}
		l.serveLinkLocal(client, pre, a)
		return
	}
	for _, h := range handlers {
		if h.claims(pre.dst) {
			h.serve(routesCtx, client, pre.dst, a)
			return
		}
	}
	a.check = "port"
	_ = client.Close()
}

// divertAudit is the audit line of one diverted connection. It names the
// sandbox and the destination, and never a header value, a token or a
// credential.
type divertAudit struct {
	sandbox string
	// dst is the address the guest dialed.
	dst string
	// transport is brig.internal for a connection to the sandbox's
	// brig.internal address, and link-local for one to the link-local
	// address.
	transport string
	// check names why the connection was not served: preamble, capacity,
	// capacity.host or port, on brig.internal brig.internal.port, tls or
	// stopping, and on link-local link-local.port or stopping. A divert
	// handler names its own.
	check string
	// msg is the message of the line, or "" for divert. A divert handler
	// sets it, with the attributes it adds in attrs.
	msg   string
	attrs []slog.Attr
}

// writeDivertAudit writes the line of one diverted connection. A
// brig.internal connection has a line of its own, beside the lines of the
// requests the route handler served on it.
func (s *server) writeDivertAudit(a *divertAudit, dur time.Duration) {
	attrs := []slog.Attr{slog.String("sandbox", a.sandbox)}
	if a.dst != "" {
		attrs = append(attrs, slog.String("dst", a.dst))
	}
	if a.transport != "" {
		attrs = append(attrs, slog.String("transport", a.transport))
	}
	if a.check != "" {
		attrs = append(attrs, slog.String("check", a.check))
	}
	attrs = append(attrs, a.attrs...)
	attrs = append(attrs, slog.Duration("dur", dur))
	msg := a.msg
	if msg == "" {
		msg = "divert"
	}
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, msg, attrs...)
}
