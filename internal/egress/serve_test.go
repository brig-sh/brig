package egress

import (
	"context"
	"net"
	"testing"
	"time"
)

// fakeUpstream serves A answers on loopback over UDP and TCP, on one port.
func fakeUpstream(t *testing.T, addr string) string {
	t.Helper()
	return fakeUpstreamAfter(t, addr, 0)
}

// fakeUpstreamAfter is fakeUpstream that answers each query after delay.
func fakeUpstreamAfter(t *testing.T, addr string, delay time.Duration) string {
	t.Helper()
	var ln net.Listener
	var pc net.PacketConn
	// The UDP port of the TCP listener's number can be taken by someone
	// else, so a few ports are tried.
	for i := 0; i < 20 && pc == nil; i++ {
		var err error
		if ln, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if pc, err = net.ListenPacket("udp4", ln.Addr().String()); err != nil {
			_ = ln.Close()
		}
	}
	if pc == nil {
		t.Fatal("no port free for both UDP and TCP")
	}
	t.Cleanup(func() { _ = ln.Close(); _ = pc.Close() })
	answer := func(q []byte) []byte {
		time.Sleep(delay)
		return buildResponse(q, []rr{aRecord(addr, 300)}, nil)
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(answer(buf[:n]), from)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			q, err := readTCP(c)
			if err == nil {
				_ = writeTCP(c, answer(q))
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestServeUDPAndTCP(t *testing.T) {
	pins := &fakePins{}
	r := &Resolver{
		Policy:   mustParse(t, "deny", []string{"host=*.example.com"}, nil),
		Upstream: []string{fakeUpstream(t, "93.184.216.34")},
		Pins:     pins,
	}
	ctx, cancel := context.WithCancel(context.Background())
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 2)
	go func() { served <- r.ServeUDP(ctx, pc) }()
	go func() { served <- r.ServeTCP(ctx, ln) }()
	defer func() {
		cancel()
		for i := 0; i < 2; i++ {
			if err := <-served; err != nil {
				t.Errorf("serve returned %v after the context ended", err)
			}
		}
	}()

	for _, network := range []string{"udp", "tcp"} {
		server := pc.LocalAddr().String()
		if network == "tcp" {
			server = ln.Addr().String()
		}
		resp, err := exchangeNet(ctx, network, server, buildQuery(9, "www.example.com", typeA))
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		if rcode(resp) != 0 || ancount(resp) != 1 {
			t.Errorf("%s allowed: rcode %d, %d answers", network, rcode(resp), ancount(resp))
		}
		resp, err = exchangeNet(ctx, network, server, buildQuery(10, "example.org", typeA))
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		if rcode(resp) != rcodeRefused {
			t.Errorf("%s refused: rcode %d", network, rcode(resp))
		}
	}
	pins.mu.Lock()
	defer pins.mu.Unlock()
	if len(pins.sets[SetAllowIP]) != 2 {
		t.Errorf("allow pins = %v", pins.sets[SetAllowIP])
	}
}

// waitPinned waits until set holds n addresses.
func waitPinned(t *testing.T, pins *fakePins, set string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		pins.mu.Lock()
		got := pins.sets[set]
		pins.mu.Unlock()
		if len(got) >= n {
			var out []string
			for _, a := range got {
				out = append(out, a.String())
			}
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never held %d addresses", set, n)
	return nil
}

// A literal rule is pinned in a set of its own, so a refresh never shortens
// the pin of an answer for the same address.
func TestWatchHostsPinsLiteralRules(t *testing.T) {
	pins := &fakePins{}
	r := &Resolver{
		Policy:   mustParse(t, "allow", []string{"host=*.glob.test"}, []string{"host=blocked.test"}),
		Upstream: []string{fakeUpstream(t, "198.51.100.7")},
		Pins:     pins,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r.RefreshHosts(ctx, time.Hour)
	if got := waitPinned(t, pins, SetDenyHost, 1); got[0] != "198.51.100.7" {
		t.Errorf("deny_host = %v", got)
	}
	pins.mu.Lock()
	if len(pins.sets[SetDenyIP])+len(pins.sets[SetAllowIP])+len(pins.sets[SetAllowHost]) != 0 {
		t.Errorf("pins = %v", pins.sets)
	}
	if got := pins.lifetimes[SetDenyHost]; got != refreshRetention*time.Hour {
		t.Errorf("refresh pinned for %s", got)
	}
	pins.mu.Unlock()

	done := make(chan struct{})
	go func() { r.WatchHosts(ctx, 20*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()
	waitPinned(t, pins, SetDenyHost, 3)
}
