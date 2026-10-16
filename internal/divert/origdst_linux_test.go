//go:build linux

package divert

import (
	"net"
	"testing"
)

// With no DNAT, conntrack reports the connection's own destination, or has no
// entry for it. Either way the relay sees the address it listens on, which is
// never diverted.
func TestSockOptOnAConnectionNothingRedirected(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, ok := <-accepted
	if !ok {
		t.Fatal("no connection accepted")
	}
	defer func() { _ = s.Close() }()
	got, err := SockOpt{}.OriginalDst(s.(*net.TCPConn))
	if err != nil {
		t.Logf("no conntrack entry: %v", err)
		return
	}
	if want := ln.Addr().(*net.TCPAddr).AddrPort(); got != want {
		t.Errorf("OriginalDst = %s, want %s", got, want)
	}
}
