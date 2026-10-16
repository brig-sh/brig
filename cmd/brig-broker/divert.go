package main

import (
	"context"
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

	"github.com/brig-sh/brig/internal/divert"
	"github.com/brig-sh/brig/internal/injector"
)

// This file serves the divert socket of a sandbox. A runtime diverts each
// guest connection to a diverted destination to a unix socket of the
// sandbox, with a one-line preamble that names where the guest was going.
// The broker reads the preamble and serves the connection: a connection to
// the sandbox's brig.internal address is served in briginternal.go, and any
// other connection is closed. The contract with the diverter is in
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
	sem      chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// openDivert listens on the divert socket of sandbox id. internal is the
// sandbox's brig.internal address, or the zero Addr. The socket is 0600 in a
// 0700 directory.
func (s *server) openDivert(id string, internal netip.Addr) (*divertSandbox, error) {
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
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("cannot open the divert socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &divertSandbox{srv: s, id: id, path: sock, ln: ln, internal: internal,
		sem: make(chan struct{}, maxDivertConns), ctx: ctx, cancel: cancel}
	go l.serve()
	return l, nil
}

// close stops the listener, closes every connection of the sandbox and
// removes the socket.
func (l *divertSandbox) close() {
	l.cancel()
	_ = l.ln.Close()
	_ = os.Remove(l.path)
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
	// or stopping.
	check string
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
	attrs = append(attrs, slog.Duration("dur", dur))
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "divert", attrs...)
}
