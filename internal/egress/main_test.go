package egress

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The resolver answers on the bridge address alone. On every address of the
// namespace, it would answer whatever else reaches that namespace.
func TestListenBindsTheBridgeAddressAlone(t *testing.T) {
	defer func(prev uint16) { dnsPort = prev }(dnsPort)
	dnsPort = 0
	addr := netip.MustParseAddr("127.0.0.1")
	pc, ln, err := listen(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close(); _ = ln.Close() }()
	for _, a := range []net.Addr{pc.LocalAddr(), ln.Addr()} {
		if got := addrOf(a); got != addr {
			t.Errorf("bound %s, want %s", a, addr)
		}
	}
}

// fakeNFT writes an nft that logs every script it is given. Its table is gone
// while the file it returns as gone exists, and its heartbeat fails while
// the one it returns as stuck does.
func fakeNFT(t *testing.T) (bin, scripts, gone, stuck string) {
	t.Helper()
	dir := t.TempDir()
	bin, scripts = filepath.Join(dir, "nft"), filepath.Join(dir, "scripts")
	gone, stuck = filepath.Join(dir, "gone"), filepath.Join(dir, "stuck")
	sh := "#!/bin/sh\n" +
		"s=$(cat)\n" +
		"printf '%s\\n' \"$s\" >> '" + scripts + "'\n" +
		"case \"$s\" in\n" +
		"'add table'*) rm -f '" + gone + "' ;;\n" +
		"'add element'*alive*) [ -e '" + gone + "' ] || [ -e '" + stuck + "' ] && exit 1 ;;\n" +
		"'list table'*) [ -e '" + gone + "' ] && exit 1 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(sh), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, scripts, gone, stuck
}

func TestTableKeeper(t *testing.T) {
	// keeper returns a keeper on a fake nft whose pin writes "PIN" into the
	// same log as the scripts, so the order of the two shows.
	keeper := func(t *testing.T, logTo *bytes.Buffer) (k *tableKeeper, scripts, gone, stuck string) {
		bin, scripts, gone, stuck := fakeNFT(t)
		k = &tableKeeper{nft: []string{bin}, table: "t", script: "add table inet t\n", bridge: "br-x",
			log: log.New(logTo, "", 0), pin: func(context.Context) {
				f, err := os.OpenFile(scripts, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = f.WriteString("PIN\n")
				_ = f.Close()
			}}
		return k, scripts, gone, stuck
	}
	run := func(t *testing.T, plant func(gone, stuck string)) (installs, pins int, logged string) {
		var buf bytes.Buffer
		k, scripts, gone, stuck := keeper(t, &buf)
		if err := k.install(context.Background()); err != nil {
			t.Fatal(err)
		}
		plant(gone, stuck)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { k.run(ctx, 10*time.Millisecond); close(done) }()
		time.Sleep(300 * time.Millisecond)
		cancel()
		<-done
		blob, _ := os.ReadFile(scripts)
		return strings.Count(string(blob), "add table"), strings.Count(string(blob), "PIN"), buf.String()
	}

	// The heartbeat starts only once the hosts are pinned. Until it does, the
	// table refuses every new connection, so none is admitted, and marked,
	// while a denied host has no address in the table.
	t.Run("it pins, then beats", func(t *testing.T) {
		var buf bytes.Buffer
		k, scripts, _, _ := keeper(t, &buf)
		if err := k.install(context.Background()); err != nil {
			t.Fatal(err)
		}
		blob, _ := os.ReadFile(scripts)
		s := string(blob)
		table, pin, beat := strings.Index(s, "add table"), strings.Index(s, "PIN"), strings.Index(s, HeartbeatScript("t", "br-x", HeartbeatLapse))
		if table < 0 || pin < table || beat < pin {
			t.Errorf("install ran table %d, pin %d, heartbeat %d:\n%s", table, pin, beat, s)
		}
	})
	t.Run("a table that is gone is installed again", func(t *testing.T) {
		installs, pins, logged := run(t, func(gone, _ string) {
			if err := os.WriteFile(gone, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if installs < 2 || pins < 2 || !strings.Contains(logged, "is gone, installing it again") {
			t.Errorf("installs %d, pins %d, log %q", installs, pins, logged)
		}
	})
	// Installing again would drop every pin. A heartbeat that failed on a
	// table still in place lets the bridge lapse instead.
	t.Run("a failed heartbeat on a table in place", func(t *testing.T) {
		installs, pins, logged := run(t, func(_, stuck string) {
			if err := os.WriteFile(stuck, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if installs != 1 || pins != 1 || !strings.Contains(logged, "could not renew the heartbeat") {
			t.Errorf("installs %d, pins %d, log %q", installs, pins, logged)
		}
	})
}

// syncBuffer is a buffer a logger in another goroutine can write while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// brig boots the guest once the resolver logs that it is up. By then a deny
// rule for a literal host must have its addresses in the table. The upstream
// answers late, so a refresh that ran after the line would show.
func TestServeIsUpOnlyOnceTheHostsArePinned(t *testing.T) {
	defer func(prev uint16) { dnsPort = prev }(dnsPort)
	dnsPort = 0
	bin, _, _, _ := fakeNFT(t)
	pins := &fakePins{}
	r := &Resolver{
		Policy:   mustParse(t, "allow", nil, []string{"host=blocked.test"}),
		Upstream: []string{fakeUpstreamAfter(t, "198.51.100.7", 300*time.Millisecond)},
		Pins:     pins,
	}
	logged := &syncBuffer{}
	logger := log.New(logged, "", 0)
	k := &tableKeeper{nft: []string{bin}, table: "t", script: "add table inet t\n", bridge: "br-x", log: logger,
		pin: func(ctx context.Context) { r.RefreshHosts(ctx, RefreshInterval) }}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, k, r, netip.MustParseAddr("127.0.0.1"), RefreshInterval, 0, 1, logger)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged.String(), "egress resolver on ") {
		if time.Now().After(deadline) {
			t.Fatalf("the resolver never came up:\n%s", logged.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	pins.mu.Lock()
	denied := len(pins.sets[SetDenyHost])
	pins.mu.Unlock()
	if denied == 0 {
		t.Error("the resolver was up before the denied host was pinned")
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve returned %v", err)
	}
}

func TestMainRefusesWhatItCannotEnforce(t *testing.T) {
	base := []string{"--table", "t", "--bridge", "br-x", "--listen", "10.4.1.1", "--subnet", "10.4.1.0/24",
		"--upstream", "10.0.2.3:53", "--egress-default", "deny"}
	for name, extra := range map[string][]string{
		"no mark":        nil,
		"a mark too big": {"--mark", "65536"},
		"bad default":    {"--mark", "7", "--egress-default", "maybe"},
		"bad subnet":     {"--mark", "7", "--subnet", "10.4.2.0/24"},
	} {
		var stderr bytes.Buffer
		if code := Main(append(append([]string(nil), base...), extra...), &stderr); code != 1 {
			t.Errorf("%s: exit %d, want 1:\n%s", name, code, stderr.String())
		}
	}
}
