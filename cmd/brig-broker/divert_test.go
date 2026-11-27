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
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
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
	h := &relayHandler{}
	registerRelay(t, h)
	return h
}

// registerRelay registers the route mode relay as relayMode does, served by
// h.
func registerRelay(t *testing.T, h divertHandler) {
	t.Helper()
	t.Cleanup(routemode.Register(routemode.Mode{Name: "relay", Presets: []v1alpha1.Preset{v1alpha1.PresetClaudeCode}}))
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

// openHeld opens the divert socket of sandbox id at internal, or at no
// brig.internal address for "", for a service of kind, as the service
// registry will, and records it on the server.
func openHeld(t *testing.T, r *running, id, internal, kind string) *divertSandbox {
	t.Helper()
	var addr netip.Addr
	if internal != "" {
		addr = netip.MustParseAddr(internal)
	}
	l, err := r.srv.openDivert(id, addr, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.hold(kind)
	r.srv.mu.Lock()
	r.srv.diverts[id] = l
	r.srv.mu.Unlock()
	return l
}

// A divert socket stays open while anything holds it. The routes leaving a
// socket a service holds leave the socket, and the service's connections,
// in place.
func TestDivertHeldByAServiceSurvivesRoutesLeaving(t *testing.T) {
	rig := newInternalRig(t)
	l := openHeld(t, rig.running, "brig-a", "10.0.0.1", "echo")
	l.setRoutes(false, nil)
	if _, err := os.Lstat(l.path); err != nil {
		t.Fatalf("the divert socket went with the routes: %v", err)
	}
	if !l.held() {
		t.Fatal("a socket echo holds reports no holder")
	}
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("holders = %q, want [echo]", got)
	}
	l.release("echo")
	if l.held() {
		t.Errorf("a socket nothing holds reports holders %q", l.holders())
	}
}

// One divert socket serves one brig.internal address. Routes that name
// another than the service holding the socket are refused, and the socket
// stays the service's.
func TestRoutesAtAnotherAddressThanAServiceAreRefused(t *testing.T) {
	rig := newInternalRig(t)
	l := openHeld(t, rig.running, "brig-a", "10.0.0.1", "echo")
	_, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: "10.0.0.2"})
	if ref := refusal(t, err); ref.Status != http.StatusConflict || !strings.Contains(ref.Message, "its echo at 10.0.0.1") {
		t.Fatalf("routes at another address = %d %q, want 409 naming echo at 10.0.0.1", ref.Status, ref.Message)
	}
	rig.srv.mu.Lock()
	got, registered := rig.srv.diverts["brig-a"], rig.srv.sandboxes["brig-a"] != ""
	rig.srv.mu.Unlock()
	if got != l || registered {
		t.Errorf("after the refusal the socket is replaced %v, the routes registered %v", got != l, registered)
	}
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("holders = %q, want [echo]", got)
	}
}

// A service that reaches no brig.internal address holds the socket at none,
// and the refusal of routes at an address says so.
func TestRoutesAtAnAddressAServiceDoesNotReachAreRefused(t *testing.T) {
	rig := newInternalRig(t)
	openHeld(t, rig.running, "brig-a", "", "echo")
	_, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: "10.0.0.2"})
	if ref := refusal(t, err); ref.Status != http.StatusConflict || !strings.Contains(ref.Message, "its echo at none") {
		t.Fatalf("routes at an address echo does not reach = %d %q, want 409 naming echo at none", ref.Status, ref.Message)
	}
}

// divertOf is the divert socket the server records for sandbox id.
func divertOf(r *running, id string) *divertSandbox {
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	return r.srv.diverts[id]
}

// Removing a sandbox's routes leaves the divert socket a service holds open,
// as the same socket.
func TestRemovingRoutesKeepsASocketAServiceHolds(t *testing.T) {
	rig := newInternalRig(t)
	l := openHeld(t, rig.running, "brig-a", internalA, "echo")
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if got := divertOf(rig.running, "brig-a"); got != l {
		t.Fatalf("after the removal the socket is %p, want %p", got, l)
	}
	if _, err := os.Lstat(l.path); err != nil {
		t.Fatalf("the divert socket went with the routes: %v", err)
	}
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("holders = %q, want [echo]", got)
	}
}

// Routes at the address of a socket a service holds join it, and leave it
// to the service again when they go.
func TestRoutesJoinASocketAServiceHolds(t *testing.T) {
	rig := newInternalRig(t)
	l := openHeld(t, rig.running, "brig-a", internalA, "echo")
	added := rig.add(t, "brig-a", internalA)
	if got := divertOf(rig.running, "brig-a"); got != l || added.DivertSocket != l.path {
		t.Fatalf("the routes opened another socket: %p %s, want %p %s", got, added.DivertSocket, l, l.path)
	}
	if got := l.holders(); !slices.Equal(got, []string{"routes", "echo"}) {
		t.Errorf("holders = %q, want [routes echo]", got)
	}
	tok := rig.mint(t, "brig-a", "anthropic")
	if got, err := post(guestClient(l.path, internalA+":443", caPool(t, added)),
		"/brig-a/r/anthropic/v1/messages", tok); err != nil || got != http.StatusOK {
		t.Errorf("the route on the shared socket: %d %v", got, err)
	}
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if got := divertOf(rig.running, "brig-a"); got != l {
		t.Fatalf("after the removal the socket is %p, want %p", got, l)
	}
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("holders = %q, want [echo]", got)
	}
}

// The link-local address serves the routes that vend. On a socket a service
// alone holds it serves nothing: a request there is closed unanswered and
// named check=port, and never reaches the link-local server.
func TestLinkLocalIsClosedOnASocketAServiceAloneHolds(t *testing.T) {
	rig := newLinkLocalRig(t)
	l := openHeld(t, rig.running, "brig-a", internalA, "echo")
	host, port, _ := net.SplitHostPort(injector.LinkLocalAddr)
	c, err := net.Dial("unix", l.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "BRIG-DIVERT/1 %s %s 10.100.0.2 40000\nGET /brig-a/r/aws HTTP/1.1\r\nHost: %s\r\n\r\n",
		host, port, host); err != nil {
		t.Fatal(err)
	}
	wantClosedUnanswered(t, c, "the link-local address on a socket echo alone holds")
	lines := auditWith(t, rig.dir, "msg=divert", "sandbox=brig-a", "dst="+injector.LinkLocalAddr)
	if len(lines) != 1 || !strings.Contains(lines[0], "check=port") ||
		strings.Contains(lines[0], "transport="+linkLocalTransport) {
		t.Errorf("the audit line of the connection is %q, want one with check=port and no link-local transport", lines)
	}
}

// holdHandler claims what relayHandler claims, and reads the guest until the
// connection closes, as a relay to an upstream that never answers does. It
// never watches the context it is handed, so only the broker closing the
// connection ends it.
type holdHandler struct{ serving chan struct{} }

func (h *holdHandler) claims(dst netip.AddrPort) bool {
	return dst.Addr() == relayAddr && dst.Port() == 443
}

func (h *holdHandler) serve(_ context.Context, client net.Conn, _ netip.AddrPort, _ *divertAudit) {
	h.serving <- struct{}{}
	_, _ = io.Copy(io.Discard, client)
	_ = client.Close()
}

func (h *holdHandler) close() {}

// wantOpen fails unless c is still open: a read waits out a short deadline.
func wantOpen(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s is not open: %d %v", what, n, err)
	}
}

// Removing a sandbox's routes revokes them on a socket a service holds as on
// one they hold alone: a route-mode connection in flight is closed, and the
// socket stays for the service, which still answers.
func TestRemovingRoutesClosesTheirConnectionsOnASocketAServiceHolds(t *testing.T) {
	rig := newInternalRig(t)
	h := &holdHandler{serving: make(chan struct{}, 1)}
	registerRelay(t, h)
	rig.srv.addService(echoService(t, "echo"))
	relay := claudeRoute()
	relay.Route.Mode = "relay"
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{relay}, BrigInternal: internalA,
		Modes: map[v1alpha1.RouteMode]json.RawMessage{"relay": json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	conn, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "BRIG-DIVERT/1 %s 443 10.100.0.2 40002\n", relayAddr); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay handler was not handed the connection")
	}
	wantOpen(t, conn, "the relayed connection before the removal")
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	wantClosedUnanswered(t, conn, "the relayed connection after the routes were removed")
	l := divertOf(rig.running, "brig-a")
	if l == nil || !slices.Equal(l.holders(), []string{"echo"}) {
		t.Fatal("the removal took the socket echo holds")
	}
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, onSocket(t, added, internalA), "", "/brig-a/echo", token); got.code != http.StatusOK || got.body != "store-1" {
		t.Errorf("echo after the routes were removed = %+v, want 200 store-1", got)
	}
}

// A link-local connection is the routes' too: removing them closes it on a
// socket a service holds.
func TestRemovingRoutesClosesALinkLocalConnectionOnASocketAServiceHolds(t *testing.T) {
	rig := newLinkLocalRig(t)
	rig.srv.addService(echoService(t, "echo"))
	added := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	host, port, _ := net.SplitHostPort(injector.LinkLocalAddr)
	conn, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The route server waits for a request on the connection.
	if _, err := fmt.Fprintf(conn, "BRIG-DIVERT/1 %s %s 10.100.0.2 40000\n", host, port); err != nil {
		t.Fatal(err)
	}
	wantOpen(t, conn, "the link-local connection before the removal")
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	wantClosedUnanswered(t, conn, "the link-local connection after the routes were removed")
	if l := divertOf(rig.running, "brig-a"); l == nil || !slices.Equal(l.holders(), []string{"echo"}) {
		t.Fatal("the removal took the socket echo holds")
	}
}

// A route request on brig.internal is the routes' too: removing them ends a
// request in flight on a socket a service holds, as closing the socket did
// before services. The upstream's request ends, the guest's response ends,
// and the service still answers.
func TestRemovingRoutesEndsABrigInternalRequestOnASocketAServiceHolds(t *testing.T) {
	streaming, ended, release := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		streaming <- struct{}{}
		select {
		case <-r.Context().Done():
			ended <- struct{}{}
		case <-release:
		}
	}))
	t.Cleanup(up.Close)
	// Runs before up.Close, which waits for the handler.
	t.Cleanup(func() { close(release) })
	rig := &internalRig{running: startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard,
		transport: transportTo(up)}, nil)}
	if err := rig.store.Update("claude-credentials", fakeDoc("real-access-token", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	rig.srv.addService(echoService(t, "echo"))
	added := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")

	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/r/anthropic/v1/messages",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+rig.mint(t, "brig-a", "anthropic"))
	c := onSocket(t, added, internalA)
	c.Timeout = 0
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-streaming:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream did not start streaming")
	}
	read := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); read <- err }()

	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request still runs after the routes were removed")
	}
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's response still streams after the routes were removed")
	}
	if l := divertOf(rig.running, "brig-a"); l == nil || !slices.Equal(l.holders(), []string{"echo"}) {
		t.Fatal("the removal took the socket echo holds")
	}
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, onSocket(t, added, internalA), "", "/brig-a/echo", token); got.code != http.StatusOK || got.body != "store-1" {
		t.Errorf("echo after the routes were removed = %+v, want 200 store-1", got)
	}
}
