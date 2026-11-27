package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	"github.com/brig-sh/brig/internal/sessiontoken"
)

// echoService is a guest service of these tests: its data is a string, and
// it answers a request with it.
func echoService(t *testing.T, kind string) *guestService {
	t.Helper()
	key := make([]byte, sessiontoken.MinHostKey)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return &guestService{
		kind:      kind,
		label:     "brig-" + kind,
		tokenType: "brig-" + kind,
		hostKey:   key,
		same:      func(prev, next any) bool { return prev.(string) == next.(string) },
		serve: func(w http.ResponseWriter, _ *http.Request, _, _ string, data any) {
			_, _ = fmt.Fprint(w, data)
		},
	}
}

// serviceRig is a broker in this process that serves echo.
func serviceRig(t *testing.T) *internalRig {
	t.Helper()
	rig := newInternalRig(t)
	rig.srv.addService(echoService(t, "echo"))
	return rig
}

// register registers sandbox id for kind at internal, and fails the test on
// a refusal.
func register(t *testing.T, r *running, kind, id, internal string, data any) injector.AddResponse {
	t.Helper()
	var addr netip.Addr
	if internal != "" {
		addr = netip.MustParseAddr(internal)
	}
	resp, code, err := r.srv.registerService(kind, id, addr, data)
	if err != nil {
		t.Fatalf("register %s for %s = %d %v", id, kind, code, err)
	}
	return resp
}

// removeServices sends DELETE /services/sandboxes/{id} on the control socket.
func removeServices(t *testing.T, r *running, id string) injector.RemoveResponse {
	t.Helper()
	sock := filepath.Join(r.dir, injector.SocketName)
	c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequest(http.MethodDelete, "http://broker/services/sandboxes/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /services/sandboxes/%s = %d %q", id, resp.StatusCode, buf.String())
	}
	var out injector.RemoveResponse
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// verifyEcho checks token as the echo token of sandbox id.
func verifyEcho(r *running, id, token string) error {
	_, _, err := r.srv.verifyService("echo", id, token, time.Now())
	return err
}

// The same registration again keeps the nonce, so the tokens minted under
// it stay valid. A registration after a removal draws another, and refuses
// them.
func TestServiceReRegistrationKeepsTheToken(t *testing.T) {
	rig := serviceRig(t)
	first := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	if first.DivertSocket == "" || first.CA == "" {
		t.Fatalf("a registration at brig.internal answered %+v, want the socket and the CA", first)
	}
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	again := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	if again.DivertSocket != first.DivertSocket {
		t.Errorf("the socket moved from %s to %s", first.DivertSocket, again.DivertSocket)
	}
	if err := verifyEcho(rig.running, "brig-a", token); err != nil {
		t.Fatalf("a token minted before the same registration again = %v, want valid", err)
	}
	if !rig.srv.unregisterService("echo", "brig-a") {
		t.Fatal("unregister found no registration")
	}
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	if err := verifyEcho(rig.running, "brig-a", token); err == nil {
		t.Error("a token minted before a removal verifies under the next registration")
	}
}

// A sandbox that changes its data, or its address, under one registration
// is refused, and keeps what it had.
func TestServiceRegistrationWithOtherDataIsRefused(t *testing.T) {
	rig := serviceRig(t)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	_, code, err := rig.srv.registerService("echo", "brig-a", netip.MustParseAddr(internalA), "store-2")
	if code != http.StatusConflict || err == nil || !strings.Contains(err.Error(), "other settings") {
		t.Fatalf("other data = %d %v, want 409 naming other settings", code, err)
	}
	_, code, err = rig.srv.registerService("echo", "brig-a", netip.MustParseAddr(internalB), "store-1")
	want := fmt.Sprintf("sandbox brig-a is registered for echo at brig.internal %s, not %s", internalA, internalB)
	if code != http.StatusConflict || err == nil || err.Error() != want {
		t.Fatalf("another address = %d %v, want 409 %q", code, err, want)
	}
	rig.srv.mu.Lock()
	data := rig.srv.serviceRegs["echo"]["brig-a"].data
	rig.srv.mu.Unlock()
	if data != "store-1" {
		t.Errorf("after the refusals the data is %v, want store-1", data)
	}
	if _, code, err := rig.srv.registerService("nope", "brig-a", netip.Addr{}, "x"); code != http.StatusNotFound || err == nil {
		t.Errorf("an unknown kind = %d %v, want 404", code, err)
	}
}

// One divert socket serves one brig.internal address. A service at another
// address than the routes that hold the socket is refused, and the socket
// stays the routes' alone.
func TestServiceAtAnotherAddressThanRoutesIsRefused(t *testing.T) {
	rig := serviceRig(t)
	rig.add(t, "brig-a", "10.0.0.1")
	_, code, err := rig.srv.registerService("echo", "brig-a", netip.MustParseAddr("10.0.0.2"), "store-1")
	want := "sandbox brig-a: the routes reach brig.internal at 10.0.0.1, and its echo at 10.0.0.2; " +
		"both must name the same address"
	if code != http.StatusConflict || err == nil || err.Error() != want {
		t.Fatalf("echo at another address = %d %v, want 409 %q", code, err, want)
	}
	rig.srv.mu.Lock()
	_, registered := rig.srv.serviceRegs["echo"]["brig-a"]
	l := rig.srv.diverts["brig-a"]
	rig.srv.mu.Unlock()
	if registered {
		t.Error("the refused service is registered")
	}
	if got := l.holders(); !slices.Equal(got, []string{"routes"}) {
		t.Errorf("holders = %q, want [routes]", got)
	}

	// Routes without brig.internal are served on the loopback port, which
	// a sandbox with a brig.internal socket is refused: a service there
	// would cut them off.
	if _, err := rig.client.Add(injector.Sandbox{ID: "brig-b", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	_, code, err = rig.srv.registerService("echo", "brig-b", netip.MustParseAddr("10.0.0.2"), "store-1")
	if code != http.StatusConflict || err == nil || !strings.Contains(err.Error(), "the routes reach brig.internal at none") {
		t.Fatalf("echo beside routes on loopback = %d %v, want 409 naming the routes at none", code, err)
	}
	rig.srv.mu.Lock()
	l = rig.srv.diverts["brig-b"]
	rig.srv.mu.Unlock()
	if l != nil {
		t.Error("the refused service opened a divert socket")
	}
}

// Revoking a sandbox's services drops every kind of it, and leaves its
// routes and the other sandboxes. The answer counts every sandbox still
// registered, for routes or for a service.
func TestRevokeAllServicesAnswersRemaining(t *testing.T) {
	rig := serviceRig(t)
	rig.srv.addService(echoService(t, "echo-b"))
	rig.add(t, "brig-a", internalA)
	rig.add(t, "brig-b", internalB)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo-b", "brig-a", internalA, "store-1")
	onlyEcho := register(t, rig.running, "echo", "brig-c", "198.18.1.9", "store-1")

	got := removeServices(t, rig.running, "brig-a")
	if !got.Removed || got.Remaining != 3 {
		t.Fatalf("remove = %+v, want removed, 3 remaining (brig-a's routes, brig-b, brig-c)", got)
	}
	st, err := rig.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	for kind, ids := range st.Services {
		if slices.Contains(ids, "brig-a") {
			t.Errorf("status lists brig-a for %s after the revoke", kind)
		}
	}
	if _, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute); err == nil {
		t.Error("a revoked sandbox is minted an echo token")
	}
	rig.srv.mu.Lock()
	l := rig.srv.diverts["brig-a"]
	rig.srv.mu.Unlock()
	if l == nil || !slices.Equal(l.holders(), []string{"routes"}) {
		t.Fatal("the revoke took the socket the routes hold")
	}

	// The socket of a sandbox only a service held goes with it.
	got = removeServices(t, rig.running, "brig-c")
	if !got.Removed || got.Remaining != 2 {
		t.Fatalf("remove brig-c = %+v, want removed, 2 remaining", got)
	}
	if _, err := os.Lstat(onlyEcho.DivertSocket); !os.IsNotExist(err) {
		t.Errorf("the socket only echo held is still there: %v", err)
	}
	if got := removeServices(t, rig.running, "brig-c"); got.Removed {
		t.Error("a second revoke reports a removal")
	}
}

// controlDelete sends DELETE path on the control socket, and returns the
// status and the body.
func controlDelete(t *testing.T, r *running, path string) (int, string) {
	t.Helper()
	sock := filepath.Join(r.dir, injector.SocketName)
	c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequest(http.MethodDelete, "http://broker"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// Revoking one kind of a sandbox drops that kind alone: another kind of the
// same sandbox stays registered and keeps its token. The answer counts every
// sandbox still registered. An unknown kind and a sandbox that is not
// registered for the kind are 404, each saying which, so the client can tell
// them from a path the broker does not have.
func TestRevokeOneServiceKind(t *testing.T) {
	rig := serviceRig(t)
	rig.srv.addService(echoService(t, "echo-b"))
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo-b", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo", "brig-b", internalB, "store-1")
	kept, err := rig.srv.mintService("echo-b", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	code, body := controlDelete(t, rig.running, "/services/echo/sandboxes/brig-a")
	var got injector.RemoveResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("DELETE /services/echo/sandboxes/brig-a = %d %q, want 200", code, body)
	}
	if !got.Removed || got.Remaining != 2 {
		t.Errorf("remove = %+v, want removed, 2 remaining (brig-a for echo-b, brig-b)", got)
	}
	if _, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute); err == nil {
		t.Error("brig-a is still minted an echo token")
	}
	if _, _, err := rig.srv.verifyService("echo-b", "brig-a", kept, time.Now()); err != nil {
		t.Errorf("the echo-b token of brig-a = %v after echo was revoked, want valid", err)
	}
	rig.srv.mu.Lock()
	l := rig.srv.diverts["brig-a"]
	rig.srv.mu.Unlock()
	if l == nil || !slices.Equal(l.holders(), []string{"echo-b"}) {
		t.Error("the socket echo-b holds is gone or still held by echo")
	}

	for _, c := range []struct{ path, want string }{
		{"/services/echo/sandboxes/brig-a", "sandbox brig-a is not registered for echo\n"},
		{"/services/nope/sandboxes/brig-a", "this broker serves no nope\n"},
	} {
		if code, body := controlDelete(t, rig.running, c.path); code != http.StatusNotFound || body != c.want {
			t.Errorf("DELETE %s = %d %q, want 404 %q", c.path, code, body, c.want)
		}
	}
	if code, _ := controlDelete(t, rig.running, "/services/echo/sandboxes/bad%21id"); code != http.StatusBadRequest {
		t.Errorf("an invalid id = %d, want 400", code)
	}
	// The removal of every kind is still its own path.
	if got := removeServices(t, rig.running, "brig-a"); !got.Removed || got.Remaining != 1 {
		t.Errorf("remove every kind = %+v, want removed, 1 remaining", got)
	}
	// The client's path is this one.
	if got, err := rig.client.RemoveService("echo", "brig-b"); err != nil || !got.Removed || got.Remaining != 0 {
		t.Errorf("RemoveService(echo, brig-b) = %+v %v, want removed, 0 remaining", got, err)
	}
}

// Status names the sandboxes of each service, sorted.
func TestStatusListsServices(t *testing.T) {
	rig := serviceRig(t)
	register(t, rig.running, "echo", "brig-b", internalB, "store-1")
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	st, err := rig.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Services["echo"]; !slices.Equal(got, []string{"brig-a", "brig-b"}) {
		t.Errorf("status services = %v, want echo: [brig-a brig-b]", st.Services)
	}
}

// A token is refused once its sandbox is unregistered, and the key cleared
// then is never the one a verification uses: a token signed with the zero
// key is refused too.
func TestAnUnregisteredServiceTokenIsRefused(t *testing.T) {
	rig := serviceRig(t)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := rig.srv.verifyService("echo", "brig-a", token, time.Now())
	if err != nil || data != "store-1" {
		t.Fatalf("verify before the removal = %v %v, want store-1", data, err)
	}
	rig.srv.unregisterService("echo", "brig-a")
	if _, _, err := rig.srv.verifyService("echo", "brig-a", token, time.Now()); err == nil {
		t.Error("the token of an unregistered sandbox verifies")
	}
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	zero, err := sessiontoken.Mint(make([]byte, 32), sessiontoken.Claims{Sandbox: "brig-a", Session: "s1",
		Exp: time.Now().Add(time.Minute).Unix(), Typ: "brig-echo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rig.srv.verifyService("echo", "brig-a", zero, time.Now()); err == nil {
		t.Error("a token signed with the zero key verifies")
	}
	if _, _, err := rig.srv.verifyService("nope", "brig-a", token, time.Now()); err == nil {
		t.Error("a token of an unknown service verifies")
	}
}

// A service holds the socket even without a brig.internal address. Routes
// at an address are then refused, naming the service and where it is.
func TestAServiceWithoutAnAddressHoldsTheSocket(t *testing.T) {
	rig := serviceRig(t)
	added := register(t, rig.running, "echo", "brig-a", "", "store-1")
	if added.DivertSocket == "" || added.CA != "" {
		t.Fatalf("a registration without an address answered %+v, want the socket and no CA", added)
	}
	if _, err := os.Lstat(added.DivertSocket); err != nil {
		t.Fatalf("the divert socket is missing: %v", err)
	}
	rig.srv.mu.Lock()
	l := rig.srv.diverts["brig-a"]
	rig.srv.mu.Unlock()
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Fatalf("holders = %q, want [echo]", got)
	}
	_, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	want := "sandbox brig-a: the routes reach brig.internal at " + internalA + ", and its echo at none"
	if ref := refusal(t, err); ref.Status != http.StatusConflict || !strings.Contains(ref.Message, want) {
		t.Fatalf("routes at an address = %d %q, want 409 %q", ref.Status, ref.Message, want)
	}
	if got := l.holders(); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("after the refusal holders = %q, want [echo]", got)
	}
}

// answer is one response to a guest.
type answer struct {
	code int
	auth string // WWW-Authenticate
	body string
}

// call sends one POST to https://<host><path> on c, with token as a bearer
// token when it is set. An empty host is brig.internal itself.
func call(t *testing.T, c *http.Client, host, path, token string) answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return answer{code: resp.StatusCode, auth: resp.Header.Get("WWW-Authenticate"), body: buf.String()}
}

// onSocket is a guest client of the sandbox added with added at internal.
func onSocket(t *testing.T, added injector.AddResponse, internal string) *http.Client {
	t.Helper()
	return guestClient(added.DivertSocket, internal+":443", caPool(t, added))
}

// Each service signs with its own key and checks its own typ, so a token of
// one kind opens no other, though both name the same sandbox.
func TestServiceTokenIsRefusedByAnotherKind(t *testing.T) {
	rig := serviceRig(t)
	rig.srv.addService(echoService(t, "echo2"))
	added := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo2", "brig-a", internalA, "store-2")
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := onSocket(t, added, internalA)
	got := call(t, c, "", "/brig-a/echo2", token)
	want := answer{code: http.StatusUnauthorized, auth: "Bearer", body: "brig broker: the echo2 token was refused\n"}
	if got != want {
		t.Errorf("an echo token on echo2 = %+v, want %+v", got, want)
	}
	// The same token is served by its own kind.
	if got := call(t, c, "", "/brig-a/echo", token); got.code != http.StatusOK || got.body != "store-1" {
		t.Errorf("an echo token on echo = %+v, want 200 store-1", got)
	}
}

// A service token carries the service's typ and is signed with the
// service's key, so a route refuses it as it refuses any other bad token, and
// nothing reaches the upstream.
func TestServiceTokenIsRefusedByARoute(t *testing.T) {
	rig := serviceRig(t)
	added := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := call(t, onSocket(t, added, internalA), "", "/brig-a/r/anthropic/v1/messages", token)
	if got.code != http.StatusUnauthorized || got.body != "brig broker: the session token was refused\n" {
		t.Errorf("an echo token on a route = %+v, want the route's 401", got)
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// A route token is signed with the route key, so a service refuses it.
func TestRouteTokenIsRefusedByAService(t *testing.T) {
	rig := serviceRig(t)
	added := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	got := call(t, onSocket(t, added, internalA), "", "/brig-a/echo", rig.mint(t, "brig-a", "anthropic"))
	want := answer{code: http.StatusUnauthorized, auth: "Bearer", body: "brig broker: the echo token was refused\n"}
	if got != want {
		t.Errorf("a route token on echo = %+v, want %+v", got, want)
	}
	// No token at all is the same refusal.
	if got := call(t, onSocket(t, added, internalA), "", "/brig-a/echo", ""); got != want {
		t.Errorf("no token on echo = %+v, want %+v", got, want)
	}
}

// A host route is named by its Host, and its path is the upstream's: a path
// that looks like a service's is the route's, and reaches the upstream with
// the route's token. The service's token opens nothing there.
func TestHostRouteWithAServiceLookingPathReachesTheRoute(t *testing.T) {
	rig := serviceRig(t)
	gh := injector.Route{Policy: "gh", Route: v1alpha1.Route{Name: "gh", Mode: v1alpha1.ModeHost,
		Upstream:   "https://api.github.example",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gh-token"}},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}}}},
		Guest: &v1alpha1.Guest{Env: map[string]string{"GH_TOKEN": "{session.token}"}}}}
	added := rig.add(t, "brig-a", internalA, gh)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	echo, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := onSocket(t, added, internalA)
	for _, host := range []string{"gh.brig.internal", "GH.Brig.Internal.", "gh.brig.internal:443"} {
		if got := call(t, c, host, "/brig-a/echo/y", echo); got.code != http.StatusUnauthorized ||
			got.body != "brig broker: the session token was refused\n" {
			t.Errorf("an echo token at %s = %+v, want the route's 401", host, got)
		}
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Fatalf("the upstream saw %d requests for an echo token", n)
	}
	if got := call(t, c, "gh.brig.internal", "/x/echo/y", rig.mint(t, "brig-a", "gh")); got.code != http.StatusOK {
		t.Errorf("the route's token at gh.brig.internal/x/echo/y = %+v, want 200 from the upstream", got)
	}
	if n := len(rig.up.requests()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

// A request on sandbox A's socket that names sandbox B is refused, whatever
// the token, with the answer to a sandbox that is not registered: a guest
// cannot tell the two apart.
func TestServiceRequestOnAnotherSandboxSocketIsRefused(t *testing.T) {
	rig := serviceRig(t)
	a := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo", "brig-b", internalB, "store-2")
	tokB, err := rig.srv.mintService("echo", "brig-b", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	onA := onSocket(t, a, internalA)
	got := call(t, onA, "", "/brig-b/echo", tokB)
	if got.code != http.StatusNotFound || strings.Contains(got.body, "store-2") {
		t.Fatalf("B's path and token on A's socket = %+v, want 404", got)
	}
	// brig-c has routes and the socket, and no echo registration: its own
	// path on its own socket passes the arrival check and is refused next.
	c := rig.add(t, "brig-c", "198.18.1.9")
	if unreg := call(t, onSocket(t, c, "198.18.1.9"), "", "/brig-c/echo", tokB); unreg != got {
		t.Errorf("an unregistered sandbox = %+v, want the answer to another sandbox's path %+v", unreg, got)
	}
	if got := call(t, onA, "", "/brig-b/echo/x", tokB); got.code != http.StatusNotFound {
		t.Errorf("B's path below echo on A's socket = %+v, want 404", got)
	}
}

// A service is served on brig.internal alone. The loopback port names no
// sandbox, and the link-local address is clear text, so a valid token is
// refused on both.
func TestServiceIsNotServedOnLoopbackOrLinkLocal(t *testing.T) {
	rig := serviceRig(t)
	// The link-local address is served on a socket that routes hold.
	a := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo", "brig-c", "", "store-3")
	tokA, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tokC, err := rig.srv.mintService("echo", "brig-c", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code, body, err := linkLocalGet(linkLocalClient(a.DivertSocket), "/brig-a/echo", "Bearer "+tokA); err != nil ||
		code != http.StatusNotFound || strings.Contains(body, "store-1") {
		t.Errorf("echo on the link-local address = %d %q %v, want 404", code, body, err)
	}
	for _, id := range []struct{ id, token string }{{"brig-c", tokC}, {"brig-a", tokA}} {
		url := fmt.Sprintf("http://127.0.0.1:%d/%s/echo", rig.srv.port, id.id)
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+id.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || strings.Contains(buf.String(), "store-") {
			t.Errorf("echo of %s on the loopback port = %d %q, want 404", id.id, resp.StatusCode, buf.String())
		}
	}
}

// A kind is one path segment that no route path and no control path takes:
// the routes live at /<sandbox>/r/<route>, and the control socket removes a
// sandbox's services at /services/sandboxes/<id>. Adding such a kind is a
// programming error.
func TestAServiceOfAReservedKindPanics(t *testing.T) {
	for _, kind := range []string{"r", "services", "", "a/b", "/echo", "echo/"} {
		t.Run(kind, func(t *testing.T) {
			rig := newInternalRig(t)
			defer func() {
				if recover() == nil {
					t.Errorf("addService(%q) did not panic", kind)
				}
				rig.srv.mu.Lock()
				_, added := rig.srv.services[kind]
				rig.srv.mu.Unlock()
				if added {
					t.Errorf("the refused kind %q was added", kind)
				}
			}()
			rig.srv.addService(echoService(t, kind))
		})
	}
}

// A kind no service has is a path of the routes, and the route handler
// answers it.
func TestAnUnregisteredKindReachesTheRouteHandler(t *testing.T) {
	rig := serviceRig(t)
	added := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	got := call(t, onSocket(t, added, internalA), "", "/brig-a/nope", rig.mint(t, "brig-a", "anthropic"))
	if got.code != http.StatusNotFound || got.body != "brig broker: no such route\n" {
		t.Errorf("/brig-a/nope = %+v, want the route handler's 404", got)
	}
}

// rawCall sends head, a request line and its headers without the blank line
// that ends them, to brig.internal on the socket of the sandbox added with
// added at internal, and returns the answer. It writes the request by hand,
// so a CONNECT can carry a path.
func rawCall(t *testing.T, added injector.AddResponse, internal, head string) answer {
	t.Helper()
	c, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "BRIG-DIVERT/1 %s 443 10.100.0.2 40000\n", internal); err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, &tls.Config{RootCAs: caPool(t, added), ServerName: injector.BrigInternal})
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(tc, head+"Host: "+injector.BrigInternal+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return answer{code: resp.StatusCode, auth: resp.Header.Get("WWW-Authenticate"), body: string(body)}
}

// A service takes no protocol upgrade and no CONNECT, as a route takes
// none: either would turn the request into a raw pipe. Both are refused
// before the token is read, so a valid token opens nothing.
func TestServiceRefusesAnUpgradeAndConnect(t *testing.T) {
	rig := serviceRig(t)
	added := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Authorization: Bearer " + token + "\r\n"
	want := answer{code: http.StatusBadRequest, body: "echo does not take a protocol upgrade or CONNECT\n"}
	for _, c := range []struct{ name, head string }{
		{"websocket", "GET /brig-a/echo HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" + auth},
		{"h2c", "GET /brig-a/echo HTTP/1.1\r\nUpgrade: h2c\r\n" + auth},
		{"connection upgrade", "GET /brig-a/echo HTTP/1.1\r\nConnection: keep-alive, upgrade\r\n" + auth},
		{"connect", "CONNECT /brig-a/echo HTTP/1.1\r\n" + auth},
		{"connect without a token", "CONNECT /brig-a/echo HTTP/1.1\r\n"},
	} {
		if got := rawCall(t, added, internalA, c.head); got != want {
			t.Errorf("%s = %+v, want %+v", c.name, got, want)
		}
	}
	// The same request without the upgrade is served.
	if got := rawCall(t, added, internalA, "GET /brig-a/echo HTTP/1.1\r\nConnection: close\r\n"+auth); got.code != http.StatusOK ||
		got.body != "store-1" {
		t.Errorf("a plain GET = %+v, want 200 store-1", got)
	}
}

// echoExp is the expiry of an echo token of sandbox id.
func echoExp(t *testing.T, r *running, id, token string) time.Time {
	t.Helper()
	r.srv.mu.Lock()
	key := slices.Clone(r.srv.serviceRegs["echo"][id].key)
	r.srv.mu.Unlock()
	c, err := sessiontoken.Verify(key, token, id, "", "brig-echo", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return time.Unix(c.Exp, 0)
}

// A service token is minted as a route token is: for a valid sandbox id
// alone, and for at most injector.TokenTTL, which a ttl that is not positive
// or is longer gets.
func TestMintServiceChecksTheIDAndCapsTheTTL(t *testing.T) {
	rig := serviceRig(t)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	for _, id := range []string{"", "..", "brig/a", strings.Repeat("a", 256)} {
		if _, err := rig.srv.mintService("echo", id, "s1", time.Minute); err == nil ||
			!strings.Contains(err.Error(), "is not 1 to") {
			t.Errorf("mint for sandbox id %q = %v, want the invalid id refusal", id, err)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Minute, injector.TokenTTL + time.Hour, 100 * time.Hour} {
		before := time.Now()
		token, err := rig.srv.mintService("echo", "brig-a", "s1", ttl)
		if err != nil {
			t.Fatal(err)
		}
		exp := echoExp(t, rig.running, "brig-a", token)
		if lo, hi := before.Add(injector.TokenTTL-2*time.Second), time.Now().Add(injector.TokenTTL+time.Second); exp.Before(lo) || exp.After(hi) {
			t.Errorf("ttl %v: the token expires at %v, want about %v from now", ttl, exp, injector.TokenTTL)
		}
	}
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if exp := echoExp(t, rig.running, "brig-a", token); exp.After(time.Now().Add(time.Minute + time.Second)) {
		t.Errorf("a one-minute token expires at %v", exp)
	}
}

// A broker that serves a guest service alone is not idle: stop ifIdle
// leaves it serving. Once the service is removed, it stops.
func TestStopIfIdleLeavesABrokerThatServesAService(t *testing.T) {
	rig := serviceRig(t)
	added := register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	if stopping, err := rig.client.Stop(true); err != nil || stopping {
		t.Fatalf("stop ifIdle with echo registered = %v, %v", stopping, err)
	}
	token, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, onSocket(t, added, internalA), "", "/brig-a/echo", token); got.code != http.StatusOK || got.body != "store-1" {
		t.Fatalf("echo after a refused stop = %+v, want 200 store-1", got)
	}
	if got := removeServices(t, rig.running, "brig-a"); !got.Removed || got.Remaining != 0 {
		t.Fatalf("remove = %+v, want removed, none remaining", got)
	}
	if stopping, err := rig.client.Stop(true); err != nil || !stopping {
		t.Fatalf("stop ifIdle with nothing registered = %v, %v", stopping, err)
	}
	select {
	case err := <-rig.done:
		rig.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("the broker did not stop")
	}
}

// hookCall is one call of a service's refused hook.
type hookCall struct {
	arrived, id, rest, check string
	status                   int
}

// refusals records the calls of svc's refused hook.
func refusals(svc *guestService) func() []hookCall {
	var mu sync.Mutex
	var got []hookCall
	svc.refused = func(arrived, id, rest, check string, status int) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, hookCall{arrived, id, rest, check, status})
	}
	return func() []hookCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// Each shared refusal reaches the service's refused hook with its check,
// the sandbox the request arrived on and the one its path names, so a
// service can audit a probe or a replayed token. A request served is not
// reported.
func TestServiceRefusalsReachTheRefusedHook(t *testing.T) {
	rig := newInternalRig(t)
	svc := echoService(t, "echo")
	got := refusals(svc)
	rig.srv.addService(svc)
	a := rig.add(t, "brig-a", internalA)
	register(t, rig.running, "echo", "brig-a", internalA, "store-1")
	register(t, rig.running, "echo", "brig-b", internalB, "store-2")
	c := rig.add(t, "brig-c", "198.18.1.9")
	tokA, err := rig.srv.mintService("echo", "brig-a", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := rig.srv.mintService("echo", "brig-b", "s1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	onA := onSocket(t, a, internalA)
	steps := []struct {
		name string
		send func() int
		want hookCall
	}{
		{"upgrade", func() int {
			return rawCall(t, a, internalA, "CONNECT /brig-a/echo/x HTTP/1.1\r\n").code
		}, hookCall{"brig-a", "brig-a", "x", "upgrade", http.StatusBadRequest}},
		{"another sandbox's path", func() int {
			return call(t, onA, "", "/brig-b/echo/y", tokB).code
		}, hookCall{"brig-a", "brig-b", "y", "arrival", http.StatusNotFound}},
		{"link-local", func() int {
			code, _, err := linkLocalGet(linkLocalClient(a.DivertSocket), "/brig-a/echo", "Bearer "+tokA)
			if err != nil {
				t.Fatal(err)
			}
			return code
		}, hookCall{"brig-a", "brig-a", "", "arrival", http.StatusNotFound}},
		{"unregistered sandbox", func() int {
			return call(t, onSocket(t, c, "198.18.1.9"), "", "/brig-c/echo", tokA).code
		}, hookCall{"brig-c", "brig-c", "", "sandbox", http.StatusNotFound}},
		{"bad token", func() int {
			return call(t, onA, "", "/brig-a/echo", tokB).code
		}, hookCall{"brig-a", "brig-a", "", "token", http.StatusUnauthorized}},
		{"no token", func() int {
			return call(t, onA, "", "/brig-a/echo", "").code
		}, hookCall{"brig-a", "brig-a", "", "token", http.StatusUnauthorized}},
	}
	for i, s := range steps {
		if code := s.send(); code != s.want.status {
			t.Errorf("%s answered %d, want %d", s.name, code, s.want.status)
		}
		if r := got(); len(r) != i+1 || r[i] != s.want {
			t.Fatalf("%s: the hook got %+v, want %+v last", s.name, r, s.want)
		}
	}
	if ans := call(t, onA, "", "/brig-a/echo", tokA); ans.code != http.StatusOK {
		t.Fatalf("a valid request = %+v, want 200", ans)
	}
	if r := got(); len(r) != len(steps) {
		t.Errorf("a served request reached the hook: %+v", r[len(steps):])
	}
}

// holdService is a guest service of these tests that streams one event and
// then holds the request until its context ends, telling started when it
// holds and ended when it ends.
func holdService(t *testing.T, kind string) (svc *guestService, started, ended chan struct{}) {
	t.Helper()
	started, ended = make(chan struct{}, 1), make(chan struct{}, 1)
	svc = echoService(t, kind)
	svc.serve = func(w http.ResponseWriter, r *http.Request, _, _ string, _ any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			ended <- struct{}{}
		case <-time.After(30 * time.Second):
		}
	}
	return svc, started, ended
}

// Revoking a service ends its requests in flight, as removing the routes
// ends theirs: otherwise a request past the token check keeps the revoked
// registration's data while the routes hold the socket open. The routes'
// requests on the same socket still work.
func TestRevokingAServiceEndsItsRequestInFlight(t *testing.T) {
	for _, revoke := range []string{"/services/hold/sandboxes/brig-a", "/services/sandboxes/brig-a"} {
		t.Run(revoke, func(t *testing.T) {
			rig := newInternalRig(t)
			svc, started, ended := holdService(t, "hold")
			rig.srv.addService(svc)
			added := rig.add(t, "brig-a", internalA)
			register(t, rig.running, "hold", "brig-a", internalA, "store-1")
			token, err := rig.srv.mintService("hold", "brig-a", "s1", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/hold",
				strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			c := onSocket(t, added, internalA)
			c.Timeout = 0
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the service did not start streaming")
			}
			read := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, resp.Body); read <- err }()

			if code, body := controlDelete(t, rig.running, revoke); code != http.StatusOK {
				t.Fatalf("DELETE %s = %d %q, want 200", revoke, code, body)
			}
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the service request still runs after the service was revoked")
			}
			select {
			case <-read:
			case <-time.After(5 * time.Second):
				t.Fatal("the guest's response still streams after the service was revoked")
			}
			if l := divertOf(rig.running, "brig-a"); l == nil || !slices.Equal(l.holders(), []string{"routes"}) {
				t.Fatal("the revocation took the socket the routes hold")
			}
			got := call(t, onSocket(t, added, internalA), "", "/brig-a/r/anthropic/v1/messages", rig.mint(t, "brig-a", "anthropic"))
			if got.code != http.StatusOK {
				t.Errorf("a route request after the service was revoked = %+v, want 200", got)
			}
		})
	}
}
