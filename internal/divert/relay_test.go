package divert

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var bound = netip.MustParseAddrPort("160.79.104.10:443")

// fixedDst is a DstReader that reports one destination for every connection.
type fixedDst netip.AddrPort

func (f fixedDst) OriginalDst(*net.TCPConn) (netip.AddrPort, error) { return netip.AddrPort(f), nil }

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(_, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// socketPath returns a unix socket path short enough for macOS's 104 bytes.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "i.sock")
}

// injector listens on a unix socket and runs serve on each connection it
// accepts. It returns the socket and a channel that receives each
// connection's preamble.
func injector(t *testing.T, serve func(c *net.UnixConn, rd *bufio.Reader)) (string, chan string) {
	t.Helper()
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	lines := make(chan string, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				rd := bufio.NewReader(c)
				line, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				lines <- line
				serve(c.(*net.UnixConn), rd)
			}()
		}
	}()
	return path, lines
}

// relay starts r on loopback and returns its address.
func relay(t *testing.T, r *Relay) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v", err)
		}
	})
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c.(*net.TCPConn)
}

// wantReset fails unless the relay resets a connection to addr that sends
// payload. On loopback the reset can arrive before the dial returns.
func wantReset(t *testing.T, addr, payload string) {
	t.Helper()
	c, err := net.Dial("tcp4", addr)
	if errors.Is(err, syscall.ECONNRESET) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// A reset that lands before the write is reported to the write, once,
	// and the read after it sees only the end of the stream.
	if _, err := c.Write([]byte(payload)); errors.Is(err, syscall.ECONNRESET) {
		return
	}
	_, err = io.ReadAll(c)
	if err == nil {
		t.Fatal("the relay closed the connection; want a reset")
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read: %v; want a reset", err)
	}
}

func boundSet() *Set {
	s := NewSet([]Route{{"api.anthropic.com", 443}})
	_, _ = s.Add(0, []netip.Addr{bound.Addr()})
	return s
}

func TestRelayHandsTheConnectionToTheInjector(t *testing.T) {
	sock, lines := injector(t, func(c *net.UnixConn, rd *bufio.Reader) {
		// The injector reads to the guest's end of stream, then answers.
		got, err := io.ReadAll(rd)
		if err != nil {
			return
		}
		_, _ = c.Write([]byte(strings.ToUpper(string(got))))
		_ = c.CloseWrite()
	})
	addr := relay(t, &Relay{Socket: sock, Set: boundSet(), Dst: fixedDst(bound)})
	c := dial(t, addr)
	if _, err := c.Write([]byte("client hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "CLIENT HELLO" {
		t.Errorf("guest read %q", got)
	}
	src := c.LocalAddr().(*net.TCPAddr).AddrPort()
	want := fmt.Sprintf("BRIG-DIVERT/1 160.79.104.10 443 127.0.0.1 %d\n", src.Port())
	if line := <-lines; line != want {
		t.Errorf("preamble = %q, want %q", line, want)
	}
}

// The relay checks the original destination itself. A guest that reaches it
// on a destination nothing diverts, or over IPv6, is reset and the injector
// never sees the connection.
func TestRelayResetsWhatIsNotDiverted(t *testing.T) {
	for name, dst := range map[string]string{
		"another address": "160.79.104.11:443",
		"another port":    "160.79.104.10:80",
		"IPv6":            "[2001:db8::1]:443",
		"the relay":       "127.0.0.1:15443",
	} {
		t.Run(name, func(t *testing.T) {
			sock, lines := injector(t, func(*net.UnixConn, *bufio.Reader) {})
			logs := &logLines{}
			addr := relay(t, &Relay{Socket: sock, Set: boundSet(), Dst: fixedDst(netip.MustParseAddrPort(dst)), Log: logs.log})
			wantReset(t, addr, "secret-payload")
			select {
			case line := <-lines:
				t.Errorf("the injector got %q", line)
			case <-time.After(100 * time.Millisecond):
			}
			if strings.Contains(logs.String(), "secret-payload") {
				t.Errorf("the log carries the payload:\n%s", logs)
			}
		})
	}
}

func TestRelayResetsWithNoOriginalDestination(t *testing.T) {
	sock, _ := injector(t, func(*net.UnixConn, *bufio.Reader) {})
	// SockOpt fails off Linux, and on Linux for a connection no DNAT
	// redirected it returns the relay's own address, which is not diverted.
	addr := relay(t, &Relay{Socket: sock, Set: boundSet()})
	wantReset(t, addr, "")
}

func TestRelayResetsWhenTheInjectorIsUnavailable(t *testing.T) {
	logs := &logLines{}
	addr := relay(t, &Relay{Socket: socketPath(t), Set: boundSet(), Dst: fixedDst(bound), Log: logs.log})
	wantReset(t, addr, "secret-payload")
	if !strings.Contains(logs.String(), "the injector is unavailable") {
		t.Errorf("log:\n%s", logs)
	}
	if strings.Contains(logs.String(), "secret-payload") {
		t.Errorf("the log carries the payload:\n%s", logs)
	}
}

// The injector refuses by closing its socket before it sends anything. A
// close passed on as a close would read to the guest as the upstream ending
// the stream.
func TestRelayResetsWhenTheInjectorRefuses(t *testing.T) {
	sock, lines := injector(t, func(*net.UnixConn, *bufio.Reader) {})
	addr := relay(t, &Relay{Socket: sock, Set: boundSet(), Dst: fixedDst(bound)})
	// The relay hands the connection over on accept, without waiting for the
	// guest to write. On loopback the reset can then reach the dial or the
	// write before the read.
	c, err := net.Dial("tcp4", addr)
	if err == nil {
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = c.Write([]byte("client hello")); err == nil {
			_, err = io.ReadAll(c)
		}
	}
	<-lines
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("%v; want a reset", err)
	}
}

func TestRelayBoundsItsConnections(t *testing.T) {
	release := make(chan struct{})
	sock, lines := injector(t, func(*net.UnixConn, *bufio.Reader) { <-release })
	defer close(release)
	logs := &logLines{}
	addr := relay(t, &Relay{Socket: sock, Set: boundSet(), Dst: fixedDst(bound), MaxConns: 1, Log: logs.log})
	dial(t, addr)
	<-lines
	wantReset(t, addr, "")
	if !strings.Contains(logs.String(), "1 connections are open") {
		t.Errorf("log:\n%s", logs)
	}
}
