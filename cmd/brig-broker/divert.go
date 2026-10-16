package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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
// the sandbox's brig.internal address is served in briginternal.go. Any other
// connection goes to the divert handler of a route mode that claims it, and
// is closed when none does. The contract with the diverter is in
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
	// close stops serving, and closes what open opened.
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
type divertSandbox struct {
	srv  *server
	id   string
	path string
	ln   net.Listener
	// internal is the address the sandbox's guest resolves brig.internal
	// to, or the zero Addr for a sandbox that does not reach it.
	internal netip.Addr
	// handlers serve the sandbox's route modes besides endpoint, in mode
	// order.
	handlers []divertHandler
	sem      chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// openDivert listens on the divert socket of sandbox id, and opens the
// divert handler of each of modes with its settings. internal is the
// sandbox's brig.internal address, or the zero Addr. The socket is 0600 in a
// 0700 directory.
func (s *server) openDivert(id string, internal netip.Addr, modes []v1alpha1.RouteMode,
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
	var handlers []divertHandler
	closeHandlers := func() {
		for _, h := range handlers {
			h.close()
		}
	}
	for _, m := range modes {
		h, err := divertModes[m].open(s, id, settings[m])
		if err != nil {
			closeHandlers()
			return nil, err
		}
		handlers = append(handlers, h)
	}
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
	l := &divertSandbox{srv: s, id: id, path: sock, ln: ln, internal: internal, handlers: handlers,
		sem: make(chan struct{}, maxDivertConns), ctx: ctx, cancel: cancel}
	go l.serve()
	return l, nil
}

// close stops the listener, closes every connection of the sandbox, removes
// the socket and closes the handlers.
func (l *divertSandbox) close() {
	l.cancel()
	_ = l.ln.Close()
	_ = os.Remove(l.path)
	for _, h := range l.handlers {
		h.close()
	}
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
	for _, h := range l.handlers {
		if h.claims(pre.dst) {
			h.serve(l.ctx, client, pre.dst, a)
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
	// brig.internal address.
	transport string
	// check names why the connection was not served: preamble, capacity,
	// capacity.host or port, and on brig.internal brig.internal.port, tls
	// or stopping. A divert handler names its own.
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
