package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/routemode"
)

// inventoryRoute is the route of example 4 of the broker spec
// (api/policy/v1alpha1/testdata/example4-inventory-api.yaml): a custom API
// with no preset and a request allow list.
func inventoryRoute() injector.Route {
	r := v1alpha1.Route{
		Name:     "inventory",
		Upstream: "https://inventory.internal.example.com",
		Credential: &v1alpha1.CredentialRef{
			SecretRef: v1alpha1.SecretRef{Name: "inventory-token"},
			Field:     "token",
		},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "X-Api-Key", Value: "{value}"}},
		}},
		Guest: &v1alpha1.Guest{Env: map[string]string{
			"INVENTORY_URL":   "{route.url}",
			"INVENTORY_TOKEN": "{session.token}",
		}},
		Request: &v1alpha1.RequestPolicy{Allow: &v1alpha1.RequestAllow{
			Methods: []string{http.MethodGet},
			Paths:   []string{"/v2/items/*", "/v2/search"},
		}},
	}
	return injector.Route{Policy: "inventory-api", Route: r}
}

// nodeClient is an HTTPS client of a pod that resolves brig.internal to the
// broker's address listener at addr. It dials from local when local is set,
// so a test can pick the peer address. A nil pool trusts the system roots
// alone, as a pod without the brig CA.
func nodeClient(addr string, pool *x509.CertPool, local net.Addr) *http.Client {
	d := &net.Dialer{LocalAddr: local}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: injector.BrigInternal},
	}}
}

// sendTLS sends one request to https://brig.internal/<sandbox>/r/<route><path>,
// with token in Authorization when it is set, and returns the status and the
// body.
func sendTLS(t *testing.T, c *http.Client, method, sandbox, route, token, path string) (int, string) {
	t.Helper()
	url := "https://" + injector.BrigInternal + "/" + sandbox + "/r/" + route + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// The credential the node tests keep in a file, as a mounted Secret holds it.
const inventoryValue = "inv-secret-value-for-the-node-tests"

// nodeConfig is a broker on a node: TLS on a loopback port the kernel picks,
// and the credentials in a directory of files. The test pods dial from
// loopback, so the config lets them register a loopback address.
func nodeConfig(t *testing.T, logTo io.Writer) serverConfig {
	t.Helper()
	files := filesDir(t, map[string][]byte{
		"inventory-token":    []byte(`{"token":"` + inventoryValue + `"}` + "\n"),
		"claude-credentials": fakeDoc(fakeAccess, time.Now().Add(time.Hour)),
	})
	return serverConfig{dir: shortDir(t), logTo: logTo, listen: "127.0.0.1:0", secretDir: files,
		openStore: failingStore(t), loopbackAddresses: true}
}

// listenAddr is where the broker serves its routes.
func listenAddr(r *running) string { return r.srv.routes.Addr().String() }

func nodeSandbox(id, address string, routes ...injector.Route) injector.Sandbox {
	return injector.Sandbox{ID: id, Profile: "inventory", Address: address, Routes: routes}
}

// The whole node path: --listen with TLS, --secret-dir with files, a pod that
// trusts the returned CA and is bound by its address.
func TestNodeBrokerServesABoundSandboxOverTLS(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, nodeConfig(t, io.Discard), u)
	if st, err := r.client.Status(); err != nil || st.Port != 0 {
		t.Fatalf("status = %+v, %v; want port 0", st, err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, injector.PortName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the port file exists or cannot be read: %v", err)
	}
	added, err := r.client.Add(nodeSandbox("brig-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket != "" {
		t.Errorf("a sandbox with an address got the divert socket %s", added.DivertSocket)
	}
	// brig-b has an address no connection of this test comes from.
	if _, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	tokenA := r.mint(t, "brig-a", "inventory")
	tokenB := r.mint(t, "brig-b", "inventory")
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)

	// The pod without the CA cannot talk to the broker.
	bare := nodeClient(listenAddr(r), nil, nil)
	if resp, err := bare.Get("https://" + injector.BrigInternal + "/"); err == nil {
		_ = resp.Body.Close()
		t.Error("a client that does not trust the brig CA got an answer")
	}

	if code, body := sendTLS(t, pod, http.MethodGet, "brig-a", "inventory", tokenA, "/v2/items/7"); code != http.StatusOK {
		t.Fatalf("the sandbox's own token got %d: %s", code, body)
	}
	seen := u.requests()
	if len(seen) != 1 || seen[0].Get("X-Api-Key") != inventoryValue {
		t.Fatalf("upstream saw %v, want X-Api-Key from the file", seen)
	}
	for name, values := range seen[0] {
		for _, v := range values {
			if strings.Contains(v, tokenA) {
				t.Errorf("the session token reached upstream in %s", name)
			}
		}
	}

	if code, _ := sendTLS(t, pod, http.MethodGet, "brig-a", "inventory", "not-a-token", "/v2/items/7"); code != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401", code)
	}
	if code, _ := sendTLS(t, pod, http.MethodGet, "brig-b", "inventory", tokenB, "/v2/items/7"); code != http.StatusNotFound {
		t.Errorf("the token of another sandbox got %d, want 404", code)
	}
	if lines := auditWith(t, r.dir, "check=arrival", "arrived=brig-a"); len(lines) == 0 {
		t.Error("the audit log has no check=arrival line with arrived=brig-a")
	}
	if code, _ := sendTLS(t, pod, http.MethodGet, "brig-a", "inventory", tokenA, "/v2/admin"); code != http.StatusForbidden {
		t.Errorf("a path outside request.allow got %d, want 403", code)
	}
	if lines := auditWith(t, r.dir, "check=request.allow.paths"); len(lines) == 0 {
		t.Error("the audit log has no check=request.allow.paths line")
	}
	if code, _ := sendTLS(t, pod, http.MethodPost, "brig-a", "inventory", tokenA, "/v2/search"); code != http.StatusForbidden {
		t.Errorf("a method outside request.allow got %d, want 403", code)
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want the one allowed request", n)
	}
}

// A second loopback address stands for a second pod when the host binds it.
func TestNodeBrokerBindsEachPodByItsOwnAddress(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}
	probe, err := net.ListenTCP("tcp", local)
	if err != nil {
		t.Skipf("127.0.0.2 cannot be bound on this host: %v", err)
	}
	_ = probe.Close()
	u := newUpstream(t)
	r := startBrokerWith(t, nodeConfig(t, io.Discard), u)
	addedA, err := r.client.Add(nodeSandbox("brig-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	addedB, err := r.client.Add(nodeSandbox("brig-b", "127.0.0.2", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	tokenA, tokenB := r.mint(t, "brig-a", "inventory"), r.mint(t, "brig-b", "inventory")
	podA := nodeClient(listenAddr(r), caPool(t, addedA), nil)
	podB := nodeClient(listenAddr(r), caPool(t, addedB), local)
	if code, _ := sendTLS(t, podA, http.MethodGet, "brig-a", "inventory", tokenA, "/v2/items/1"); code != http.StatusOK {
		t.Errorf("pod a with its own token got %d", code)
	}
	if code, _ := sendTLS(t, podB, http.MethodGet, "brig-b", "inventory", tokenB, "/v2/items/1"); code != http.StatusOK {
		t.Errorf("pod b with its own token got %d", code)
	}
	if code, _ := sendTLS(t, podB, http.MethodGet, "brig-a", "inventory", tokenA, "/v2/items/1"); code != http.StatusNotFound {
		t.Errorf("pod b with the token of a got %d, want 404", code)
	}
	if code, _ := sendTLS(t, podA, http.MethodGet, "brig-b", "inventory", tokenB, "/v2/items/1"); code != http.StatusNotFound {
		t.Errorf("pod a with the token of b got %d, want 404", code)
	}
}

// A peer no sandbox registered is refused before a token is read: without a
// token the answer is 404 and not 401. The listener drops such a peer at
// accept, so the connection here is let in while its own sandbox is
// registered, and that sandbox is then removed with the connection still open.
func TestAnUnregisteredPeerIsRefusedBeforeTheToken(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, nodeConfig(t, io.Discard), u)
	added, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(nodeSandbox("brig-p", "127.0.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	token, tokenP := r.mint(t, "brig-a", "inventory"), r.mint(t, "brig-p", "inventory")
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)
	tr := pod.Transport.(*http.Transport)
	dial, dials := tr.DialContext, 0
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials++
		return dial(ctx, network, addr)
	}
	if code, body := sendTLS(t, pod, http.MethodGet, "brig-p", "inventory", tokenP, "/v2/items/1"); code != http.StatusOK {
		t.Fatalf("the peer's own sandbox got %d: %s", code, body)
	}
	if _, err := r.client.RemoveSandbox("brig-p"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", token} {
		if code, _ := sendTLS(t, pod, http.MethodGet, "brig-a", "inventory", tok, "/v2/items/1"); code != http.StatusNotFound {
			t.Errorf("a peer that registered nothing got %d with token %q, want 404", code, tok)
		}
	}
	if dials != 1 {
		t.Fatalf("the client dialed %d times, want one connection for every request", dials)
	}
	if lines := auditWith(t, r.dir, "check=arrival"); len(lines) < 2 {
		t.Errorf("the audit log has %d check=arrival lines, want 2", len(lines))
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want the one of brig-p", n)
	}
}

// A connection from an IP no sandbox registered is closed at accept, before
// its TLS handshake, and takes none of the maxConns slots. Without that, a
// peer that holds maxConns idle connections keeps every registered pod of the
// node out until readHeaderTimeout, and again after each reconnect.
func TestAnUnregisteredPeerTakesNoSlot(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, nodeConfig(t, io.Discard), u)
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	// The connections stay open on this side for the rest of the test.
	var idle []net.Conn
	t.Cleanup(func() {
		for _, c := range idle {
			_ = c.Close()
		}
	})
	for i := 0; i <= maxConns; i++ {
		c, err := net.DialTimeout("tcp", listenAddr(r), 5*time.Second)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		idle = append(idle, c)
		if err := c.SetReadDeadline(time.Now().Add(readHeaderTimeout / 2)); err != nil {
			t.Fatal(err)
		}
		var ne net.Error
		if _, err := c.Read(make([]byte, 1)); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
			t.Fatalf("connection %d from an unregistered peer was not closed: %v", i, err)
		}
	}
	// The peer registers now, and gets in while those connections are open.
	added, err := r.client.Add(nodeSandbox("brig-p", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)
	pod.Timeout = readHeaderTimeout / 2
	if code, body := sendTLS(t, pod, http.MethodGet, "brig-p", "inventory", r.mint(t, "brig-p", "inventory"),
		"/v2/items/1"); code != http.StatusOK {
		t.Fatalf("a registered peer got %d: %s", code, body)
	}
}

// Two sandboxes do not share an address. The address goes back when its
// sandbox registers another one or is removed.
func TestAnAddressBelongsToOneSandbox(t *testing.T) {
	r := startBrokerWith(t, nodeConfig(t, io.Discard), nil)
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	_, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.1", inventoryRoute()))
	if ref := refusal(t, err); ref.Status != http.StatusConflict {
		t.Fatalf("a shared address got %d, want 409", ref.Status)
	}
	if st, _ := r.client.Status(); !slices.Equal(st.Sandboxes, []string{"brig-a"}) {
		t.Fatalf("sandboxes = %v after the refusal", st.Sandboxes)
	}
	// The same sandbox again, and the same address spelled another way.
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Errorf("the same registration again: %v", err)
	}
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.2", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.1", inventoryRoute())); err != nil {
		t.Errorf("the address a sandbox gave up: %v", err)
	}
	if _, err := r.client.RemoveSandbox("brig-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(nodeSandbox("brig-c", "10.255.0.1", inventoryRoute())); err != nil {
		t.Errorf("the address of a removed sandbox: %v", err)
	}
	_, err = r.client.Add(nodeSandbox("brig-d", "::ffff:10.255.0.1", inventoryRoute()))
	if ref := refusal(t, err); ref.Status != http.StatusConflict {
		t.Errorf("an IPv4-mapped spelling of a taken address got %d, want 409", ref.Status)
	}
}

// An address that cannot be a pod's, one with no --listen to reach it by, and
// one combined with a divert socket's settings are refused at registration.
func TestABadAddressIsRefused(t *testing.T) {
	node := startBrokerWith(t, nodeConfig(t, io.Discard), nil)
	for _, bad := range []string{"not-an-ip", "0.0.0.0", "224.0.0.1", "fe80::1%eth0", "10.0.0.1:80"} {
		_, err := node.client.Add(nodeSandbox("brig-a", bad, inventoryRoute()))
		if ref := refusal(t, err); ref.Status != http.StatusBadRequest {
			t.Errorf("address %q got %d, want 400", bad, ref.Status)
		}
	}
	both := nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())
	both.BrigInternal = internalA
	if _, err := node.client.Add(both); refusal(t, err).Status != http.StatusBadRequest {
		t.Error("an address with a brig.internal address was not refused with 400")
	}

	loopback := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard}, nil)
	_, err := loopback.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code", Address: "10.255.0.1",
		Routes: []injector.Route{claudeRoute()}})
	ref := refusal(t, err)
	if ref.Status != http.StatusBadRequest || !strings.Contains(ref.Message, "--listen") {
		t.Errorf("an address without --listen = %d %q, want 400 naming --listen", ref.Status, ref.Message)
	}
}

// --listen replaces the loopback listener: a port file there is neither read
// nor rewritten, even one that holds no port.
func TestListenLeavesThePortFileAlone(t *testing.T) {
	cfg := serverConfig{dir: shortDir(t), logTo: io.Discard, listen: "127.0.0.1:0"}
	path := filepath.Join(cfg.dir, injector.PortName)
	if err := os.WriteFile(path, []byte("not a port\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startBrokerWith(t, cfg, nil)
	if got, err := os.ReadFile(path); err != nil || string(got) != "not a port\n" {
		t.Fatalf("the port file = %q, %v; want it as it was", got, err)
	}
	if r.srv.port != 0 {
		t.Errorf("the server has the loopback port %d", r.srv.port)
	}
}

// The divert sockets work beside the address listener: a sandbox with a
// brig.internal address still gets one and is served over it.
func TestListenKeepsTheDivertSockets(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, serverConfig{dir: shortDir(t), logTo: io.Discard, listen: "127.0.0.1:0"}, u)
	added, err := r.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code", BrigInternal: internalA,
		Routes: []injector.Route{claudeRoute()}})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket == "" {
		t.Fatal("a brig.internal sandbox got no divert socket under --listen")
	}
	token := r.mint(t, "brig-a", "anthropic")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if code, err := post(c, "/brig-a/r/anthropic/v1/messages", token); err != nil || code != http.StatusOK {
		t.Fatalf("a request over the divert socket = %d, %v", code, err)
	}
}

func TestCanonicalAddr(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "10.0.0.1": "10.0.0.1", "::ffff:10.0.0.1": "10.0.0.1", "2001:DB8::1": "2001:db8::1",
	} {
		if got, err := canonicalAddr(in, false); err != nil || got != want {
			t.Errorf("canonicalAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "fe80::1%en0", "10.0.0.1:80", " 10.0.0.1",
		"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1", "169.254.1.1", "::ffff:169.254.1.1", "fe80::1"} {
		if got, err := canonicalAddr(in, false); err == nil {
			t.Errorf("canonicalAddr(%q) = %q, want an error", in, got)
		}
	}
	// The test seam admits loopback and nothing else.
	for in, want := range map[string]string{"127.0.0.1": "127.0.0.1", "::1": "::1", "::ffff:127.0.0.1": "127.0.0.1"} {
		if got, err := canonicalAddr(in, true); err != nil || got != want {
			t.Errorf("canonicalAddr(%q, true) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"169.254.1.1", "fe80::1", "0.0.0.0"} {
		if got, err := canonicalAddr(in, true); err == nil {
			t.Errorf("canonicalAddr(%q, true) = %q, want an error", in, got)
		}
	}
}

// A pod never has a loopback or a link-local address. A connection from one
// comes from the node's host network namespace, so a sandbox registered with
// one would be bound to every process there. run builds the config without the
// test seam, as this test does.
func TestANodeBrokerRefusesALoopbackOrLinkLocalAddress(t *testing.T) {
	cfg := nodeConfig(t, io.Discard)
	cfg.loopbackAddresses = false
	r := startBrokerWith(t, cfg, nil)
	for _, bad := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1", "169.254.1.1", "fe80::1"} {
		_, err := r.client.Add(nodeSandbox("brig-a", bad, inventoryRoute()))
		ref := refusal(t, err)
		if ref.Status != http.StatusBadRequest || !strings.Contains(ref.Message, "not one IP address a pod can have") {
			t.Errorf("address %q = %d %q, want 400", bad, ref.Status, ref.Message)
		}
	}
	if st, _ := r.client.Status(); len(st.Sandboxes) != 0 {
		t.Errorf("sandboxes = %v after the refusals", st.Sandboxes)
	}
	// A pod's own address still registers on the same broker.
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Errorf("a pod address: %v", err)
	}
}

func TestPeerAddr(t *testing.T) {
	for in, want := range map[string]string{
		"address:10.0.0.1:4000":        "10.0.0.1",
		"address:[2001:db8::1]:4000":   "2001:db8::1",
		"address:[::ffff:10.0.0.1]:80": "10.0.0.1",
	} {
		if got, ok := peerAddr(in); !ok || got != want {
			t.Errorf("peerAddr(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got, ok := peerAddr("address:garbage"); !ok || got != "" {
		t.Errorf("peerAddr of garbage = %q, %v; want an address arrival with no peer", got, ok)
	}
	for _, in := range []string{"127.0.0.1:4000", "divert:brig-a/10.0.0.1:1", "link-local:brig-a/10.0.0.1:1"} {
		if _, ok := peerAddr(in); ok {
			t.Errorf("peerAddr(%q) took another transport for the address listener", in)
		}
	}
}

// A route that vends needs the host's aws CLI and a profile. A node has
// neither, so a broker started with --listen or --secret-dir refuses it at
// registration, names the route, and never runs aws.
func TestANodeBrokerRefusesAVendingRoute(t *testing.T) {
	for name, flags := range map[string]func(*testing.T, *serverConfig){
		"secret-dir alone": func(t *testing.T, cfg *serverConfig) {
			cfg.secretDir = filesDir(t, map[string][]byte{"aws": []byte(awsPointer)})
		},
		"listen alone": func(_ *testing.T, cfg *serverConfig) { cfg.listen = "127.0.0.1:0" },
		"both": func(t *testing.T, cfg *serverConfig) {
			cfg.secretDir = filesDir(t, map[string][]byte{"aws": []byte(awsPointer)})
			cfg.listen = "127.0.0.1:0"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := serverConfig{logTo: io.Discard}
			flags(t, &cfg)
			r, f := startAWSBroker(t, cfg)
			sb := awsSandbox("brig-a")
			if cfg.listen != "" {
				sb.Address = "10.255.0.1"
			}
			_, err := r.client.Add(sb)
			ref := refusal(t, err)
			if ref.Status != http.StatusBadRequest || ref.Kind != "" {
				t.Errorf("refusal = %d kind %q, want a plain 400", ref.Status, ref.Kind)
			}
			for _, want := range []string{"route aws", "credential vending", "--"} {
				if !strings.Contains(ref.Message, want) {
					t.Errorf("the refusal %q does not say %q", ref.Message, want)
				}
			}
			if st, _ := r.client.Status(); len(st.Sandboxes) != 0 || len(st.Vending) != 0 {
				t.Errorf("status = %+v after the refusal, want nothing registered", st)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "calls")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("aws ran: %v", err)
			}
		})
	}
}

// A node broker still serves the routes that need no host: the refusal is for
// the one route, not the sandbox's other routes.
func TestANodeBrokerRegistersARouteThatDoesNotVend(t *testing.T) {
	r := startBrokerWith(t, nodeConfig(t, io.Discard), nil)
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	_, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.2", inventoryRoute(), awsRoute()))
	if ref := refusal(t, err); ref.Status != http.StatusBadRequest || !strings.Contains(ref.Message, "route aws") {
		t.Errorf("a sandbox with a vending route among others = %d %q", ref.Status, ref.Message)
	}
}

// A mode the build does not serve is refused on a node as everywhere else.
func TestANodeBrokerRefusesAModeTheBuildDoesNotServe(t *testing.T) {
	r := startBrokerWith(t, nodeConfig(t, io.Discard), nil)
	sb := injector.Sandbox{ID: "brig-a", Profile: "splice", Routes: []injector.Route{inventoryRoute()},
		Modes: map[v1alpha1.RouteMode]json.RawMessage{"splice": json.RawMessage(`{}`)}}
	_, err := r.client.Add(sb)
	ref := refusal(t, err)
	if ref.Status != http.StatusBadRequest || !strings.Contains(ref.Message, routemode.NotInBuild("splice")) {
		t.Errorf("a splice registration = %d %q, want 400 with %q", ref.Status, ref.Message, routemode.NotInBuild("splice"))
	}
	if st, _ := r.client.Status(); len(st.Sandboxes) != 0 {
		t.Errorf("sandboxes = %v after the refusal", st.Sandboxes)
	}
}

// Without the flags the vending route reaches the host check as before.
func TestAVendingRouteStillReachesTheHostCheckWithoutTheFlags(t *testing.T) {
	r, f := startAWSBroker(t, serverConfig{logTo: io.Discard})
	if _, err := r.client.Add(awsSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if f.fetches(t) == 0 {
		t.Error("the host check did not run aws")
	}
}

// A re-registration that drops its address and then fails to open its divert
// socket leaves the sandbox unregistered, and its address free for the pod
// that has it now.
func TestAFailedDivertReleasesTheAddress(t *testing.T) {
	r := startBrokerWith(t, nodeConfig(t, io.Discard), nil)
	if _, err := r.client.Add(nodeSandbox("brig-a", "10.255.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	// A file where the divert directory belongs makes openDivert fail.
	dir := filepath.Join(r.dir, injector.DivertDirName)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	again := nodeSandbox("brig-a", "", inventoryRoute())
	again.BrigInternal = internalA
	_, err := r.client.Add(again)
	if ref := refusal(t, err); ref.Status != http.StatusInternalServerError {
		t.Fatalf("a failed divert got %d, want 500", ref.Status)
	}
	if _, err := r.client.Add(nodeSandbox("brig-b", "10.255.0.1", inventoryRoute())); err != nil {
		t.Errorf("the address of a sandbox whose registration failed: %v", err)
	}
}
