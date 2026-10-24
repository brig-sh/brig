package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/routemode"
)

// closedUnanswered reports whether c is closed without a byte written to it.
func closedUnanswered(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// The preamble is one strict line, and anything else closes the connection
// before a byte is answered. A connection to a destination the sandbox has
// nothing on is closed too.
func TestDivertClosesOnABadPreamble(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	lines := []string{
		"BRIG-DIVERT/2 198.18.1.1 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 0443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.01 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 [::1] 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 fe80::1%en0 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 40000 extra\n",
		"BRIG-DIVERT/1  198.18.1.1 443 10.100.0.2 40000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 70000\n",
		"BRIG-DIVERT/1 198.18.1.1 443 10.100.0.2 40000" + strings.Repeat(" ", 200) + "\n",
		// A destination that is not the sandbox's brig.internal address.
		"BRIG-DIVERT/1 192.0.2.10 443 10.100.0.2 40000\n",
	}
	for _, line := range lines {
		c, err := net.Dial("unix", added.DivertSocket)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(c, line); err != nil {
			t.Fatal(err)
		}
		if !closedUnanswered(t, c) {
			t.Errorf("%q: want the connection closed unanswered", line)
		}
		_ = c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pre, port := auditWith(t, rig.dir, "msg=divert", "check=preamble"), auditWith(t, rig.dir, "msg=divert", "check=port")
		if len(pre) == 9 && len(port) == 1 && strings.Contains(port[0], "dst=192.0.2.10:443") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit lines do not name the refusals: %q %q", pre, port)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// The divert socket is the sandbox's own: 0600 in a 0700 directory, named by
// the registration, and gone with the sandbox or the broker.
func TestADivertSocketIsPrivateAndGoesWithItsSandbox(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	info, err := os.Lstat(added.DivertSocket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the divert socket: %v, %v", info, err)
	}
	if info, err := os.Lstat(filepath.Dir(added.DivertSocket)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the divert socket directory: %v, %v", info, err)
	}
	if want, _ := injector.DivertSocket(rig.dir, "brig-a"); added.DivertSocket != want {
		t.Errorf("the divert socket is %s, want %s", added.DivertSocket, want)
	}
	if again := rig.add(t, "brig-a", internalA); again.DivertSocket != added.DivertSocket {
		t.Fatalf("registering again named %s", again.DivertSocket)
	}
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(added.DivertSocket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the divert socket outlived its sandbox: %v", err)
	}
	if c, err := net.Dial("unix", added.DivertSocket); err == nil {
		_ = c.Close()
		t.Error("the divert socket still answers")
	}
	other := rig.add(t, "brig-b", internalB)
	rig.cancel()
	<-rig.done
	rig.done <- nil
	if _, err := os.Lstat(other.DivertSocket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the divert socket outlived the broker: %v", err)
	}
}

// One broker serves the host, and caps the diverted connections of every
// sandbox together. A connection over the cap is closed, and named so.
func TestDivertCapsConnectionsAcrossSandboxes(t *testing.T) {
	saved := hostDivertSlots
	hostDivertSlots = make(chan struct{}, 2)
	t.Cleanup(func() { hostDivertSlots = saved })
	rig := newInternalRig(t)
	a := rig.add(t, "brig-a", internalA)
	b := rig.add(t, "brig-b", internalB)
	// Two connections of brig-a hold both slots while they wait for their
	// preamble.
	for range 2 {
		c, err := net.Dial("unix", a.DivertSocket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("unix", b.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !closedUnanswered(t, c) {
		t.Error("a connection over the host cap was not closed")
	}
	if got := auditWith(t, rig.dir, "sandbox=brig-b", "check=capacity.host"); len(got) == 0 {
		t.Error("no audit line says capacity.host")
	}
}

// relayHandler is the divert handler of the route mode relay, which a test
// registers. It claims port 443 of relayAddr, answers "relayed", and
// records what it was asked.
type relayHandler struct {
	mu     sync.Mutex
	served []netip.AddrPort
	closed bool
}

var relayAddr = netip.MustParseAddr("192.0.2.10")

func (h *relayHandler) claims(dst netip.AddrPort) bool {
	return dst.Addr() == relayAddr && dst.Port() == 443
}

func (h *relayHandler) serve(_ context.Context, client net.Conn, dst netip.AddrPort, a *divertAudit) {
	h.mu.Lock()
	h.served = append(h.served, dst)
	h.mu.Unlock()
	a.msg = "relay"
	a.attrs = append(a.attrs, slog.Bool("relayed", true))
	_, _ = io.WriteString(client, "relayed")
	_ = client.Close()
}

func (h *relayHandler) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
}

// relayMode registers the route mode relay on the claude-code preset, and
// its divert handler, for the length of a test. The handler refuses the
// settings "no".
func relayMode(t *testing.T) *relayHandler {
	t.Helper()
	t.Cleanup(routemode.Register(routemode.Mode{Name: "relay", Presets: []v1alpha1.Preset{v1alpha1.PresetClaudeCode}}))
	h := &relayHandler{}
	divertModes["relay"] = divertMode{
		check: func(id string, settings json.RawMessage) error {
			if string(settings) == `"no"` {
				return errors.New("relay settings refused")
			}
			return nil
		},
		open: func(*server, string, json.RawMessage) (divertHandler, error) { return h, nil },
	}
	t.Cleanup(func() { delete(divertModes, "relay") })
	return h
}

// A registration names the settings of each route mode besides endpoint, and
// the broker hands them to that mode's divert handler. A connection the
// handler claims is its own. Any other is closed, and a connection to
// brig.internal on the same socket is still brig.internal. The relay route
// opens no endpoint, and its handler goes with the sandbox.
func TestADivertHandlerServesWhatItClaims(t *testing.T) {
	rig := newInternalRig(t)
	h := relayMode(t)
	relay := claudeRoute()
	relay.Route.Mode = "relay"
	if err := rig.store.Update("gh-token", []byte("real-gh-token")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		modes map[v1alpha1.RouteMode]json.RawMessage
		want  string
	}{
		{"no settings", nil, `route anthropic is mode "relay", and the sandbox was registered with nothing to serve it`},
		{"settings the handler refuses", map[v1alpha1.RouteMode]json.RawMessage{"relay": json.RawMessage(`"no"`)},
			"relay settings refused"},
		{"a mode not in this build", map[v1alpha1.RouteMode]json.RawMessage{"other": json.RawMessage(`{}`)},
			"route mode other is not part of this build of brig"},
	} {
		_, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
			Routes: []injector.Route{relay}, Modes: c.modes})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{relay, githubRoute()}, BrigInternal: internalA,
		Modes: map[v1alpha1.RouteMode]json.RawMessage{"relay": json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dst, want string
	}{
		{relayAddr.String() + " 443", "relayed"},
		{relayAddr.String() + " 8443", ""},
	} {
		conn, err := net.Dial("unix", added.DivertSocket)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(conn, "BRIG-DIVERT/1 %s 10.100.0.2 40002\n", c.dst); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got, _ := io.ReadAll(conn)
		_ = conn.Close()
		if string(got) != c.want {
			t.Errorf("%s: read %q, want %q", c.dst, got, c.want)
		}
	}
	if lines := auditWith(t, rig.dir, "msg=relay", "sandbox=brig-a", "dst="+relayAddr.String()+":443", "relayed=true"); len(lines) == 0 {
		t.Error("no audit line names the relayed connection")
	}
	if lines := auditWith(t, rig.dir, "msg=divert", "check=port", "dst="+relayAddr.String()+":8443"); len(lines) == 0 {
		t.Error("no audit line names the unclaimed connection")
	}
	gh := rig.mint(t, "brig-a", "github")
	if got, err := post(guestClient(added.DivertSocket, internalA+":443", caPool(t, added)), "/brig-a/r/github/user", gh); err != nil || got != http.StatusOK {
		t.Errorf("the endpoint route over brig.internal: %d %v", got, err)
	}
	tok := rig.mint(t, "brig-a", "anthropic")
	if got, err := post(guestClient(added.DivertSocket, internalA+":443", caPool(t, added)),
		"/brig-a/r/anthropic/v1/messages", tok); err != nil || got != http.StatusNotFound {
		t.Errorf("the relay route over brig.internal: %d %v, want 404", got, err)
	}
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.served) != 1 || !h.closed {
		t.Errorf("the handler served %v and closed %v", h.served, h.closed)
	}
}
