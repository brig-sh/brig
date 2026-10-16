package divert

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// DefaultMaxConns bounds the guest connections one relay carries at once.
const DefaultMaxConns = 256

// dialTimeout bounds the connect to the injector's socket.
const dialTimeout = 3 * time.Second

// DstReader reads where a redirected connection was going.
type DstReader interface {
	OriginalDst(c *net.TCPConn) (netip.AddrPort, error)
}

// Relay accepts the guest connections the table redirects to it and hands
// each one to the injector.
type Relay struct {
	// Socket is the injector's unix socket, on the host's file system.
	Socket string
	// Set is what the relay diverts. A connection to anything else is reset.
	Set *Set
	// Dst reads the original destination. Nil reads SO_ORIGINAL_DST.
	Dst DstReader
	// MaxConns bounds the connections carried at once. Zero means
	// DefaultMaxConns.
	MaxConns int
	// Log receives a line for each refused connection, keyed so the caller
	// can limit how often one kind is logged. A line names addresses and
	// never carries the payload. Nil logs nothing.
	Log func(key, format string, args ...any)
}

func (r *Relay) logf(key, format string, args ...any) {
	if r.Log != nil {
		r.Log(key, format, args...)
	}
}

// Serve hands the connections accepted from ln to the injector until ctx is
// done.
func (r *Relay) Serve(ctx context.Context, ln net.Listener) error {
	max := r.MaxConns
	if max <= 0 {
		max = DefaultMaxConns
	}
	sem := make(chan struct{}, max)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		tc, ok := c.(*net.TCPConn)
		if !ok {
			_ = c.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			r.logf("busy", "divert: %d connections are open, so a new one was reset", max)
			reset(tc)
			continue
		}
		go func() {
			defer func() { <-sem }()
			r.handle(tc)
		}()
	}
}

// handle carries one guest connection. Every failure resets the guest. A
// close would read to the guest as an upstream that ended the stream.
func (r *Relay) handle(guest *net.TCPConn) {
	reader := r.Dst
	if reader == nil {
		reader = SockOpt{}
	}
	dst, err := reader.OriginalDst(guest)
	if err != nil {
		r.logf("no-dst", "divert: reset a connection with no original destination: %v", err)
		reset(guest)
		return
	}
	dst = netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())
	if !dst.Addr().Is4() {
		r.logf("v6", "divert: reset a connection to %s; IPv6 is never diverted", dst)
		reset(guest)
		return
	}
	if !r.Set.Match(dst) {
		r.logf(dst.String(), "divert: reset a connection to %s, which is not diverted", dst)
		reset(guest)
		return
	}
	src := addrPortOf(guest.RemoteAddr())
	line, err := Preamble(dst, src)
	if err != nil {
		r.logf("preamble", "divert: reset a connection to %s: %v", dst, err)
		reset(guest)
		return
	}
	c, err := net.DialTimeout("unix", r.Socket, dialTimeout)
	if err != nil {
		r.logf("socket", "divert: reset a connection to %s; the injector is unavailable: %v", dst, err)
		reset(guest)
		return
	}
	inj := c.(*net.UnixConn)
	if _, err := inj.Write(line); err != nil {
		r.logf("socket", "divert: reset a connection to %s; the injector is unavailable: %v", dst, err)
		_ = inj.Close()
		reset(guest)
		return
	}
	proxy(guest, inj)
}

// proxy copies both ways and passes each end-of-stream on with CloseWrite.
//
// The injector refuses a connection by closing its socket before it sends
// anything. The guest then gets a reset. An error in either direction also
// resets the guest and closes the socket, which ends the other direction.
func proxy(guest *net.TCPConn, inj *net.UnixConn) {
	var once sync.Once
	abort := func() {
		once.Do(func() {
			reset(guest)
			_ = inj.Close()
		})
	}
	up := make(chan struct{})
	go func() {
		defer close(up)
		if _, err := io.Copy(inj, guest); err != nil {
			abort()
			return
		}
		_ = inj.CloseWrite()
	}()
	n, err := io.Copy(guest, inj)
	switch {
	case err != nil, n == 0:
		abort()
	default:
		_ = guest.CloseWrite()
	}
	<-up
	once.Do(func() {
		_ = guest.Close()
		_ = inj.Close()
	})
}

// reset closes c with a TCP reset.
func reset(c *net.TCPConn) {
	_ = c.SetLinger(0)
	_ = c.Close()
}

func addrPortOf(a net.Addr) netip.AddrPort {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.AddrPort()
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}
