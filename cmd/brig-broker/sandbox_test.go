package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/injector"
)

// sandboxConfig is a node broker that serves pod-a alone, on a listener the
// test opened and handed over, as a platform hands one over with
// --listen-fd.
func sandboxConfig(t *testing.T) serverConfig {
	t.Helper()
	cfg := nodeConfig(t, io.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.listen = ""
	cfg.listener = ln
	cfg.sandbox = "pod-a"
	return cfg
}

// A broker started for one sandbox refuses to register any other, so a
// broker that a guest reaches cannot be made to serve a second sandbox, and
// a request that names another sandbox finds nothing.
func TestASandboxBrokerRefusesAnotherSandbox(t *testing.T) {
	u := newUpstream(t)
	r := startBrokerWith(t, sandboxConfig(t), u)
	_, err := r.client.Add(nodeSandbox("pod-b", "127.0.0.1", inventoryRoute()))
	wantRefused(t, err, http.StatusForbidden, "a registration of pod-b on the broker of pod-a")
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 0 {
		t.Fatalf("the refused registration left sandboxes %v", st.Sandboxes)
	}

	added, err := r.client.Add(nodeSandbox("pod-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "pod-a", "inventory")
	pod := nodeClient(listenAddr(r), caPool(t, added), nil)
	if code, _ := sendTLS(t, pod, http.MethodGet, "pod-b", "inventory", token, "/v2/items/7"); code != http.StatusNotFound {
		t.Errorf("a request naming pod-b got %d, want 404", code)
	}
	if _, err := r.client.Mint(injector.MintRequest{Sandbox: "pod-b", Session: "pod-b", Route: "inventory", TTLSeconds: 60}); err == nil {
		t.Error("the broker of pod-a minted a token for pod-b")
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests, want none", n)
	}
	if code, body := sendTLS(t, pod, http.MethodGet, "pod-a", "inventory", token, "/v2/items/7"); code != http.StatusOK {
		t.Fatalf("pod-a's own request got %d: %s", code, body)
	}
}

// A broker started for one sandbox holds one policy. A second one is refused
// with 409 and the first stays. The same policy stored again, as the node's
// sync does on every change, is taken.
func TestASandboxBrokerHoldsOnePolicy(t *testing.T) {
	r := startBrokerWith(t, sandboxConfig(t), nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-sonnet-*"))

	other := injector.PolicyRef{Namespace: "sandboxes", Name: "other"}
	_, err := r.client.PutPolicy(other, injector.PutPolicyRequest{Version: "1", Policy: claudePolicy("other", false, "claude-*")})
	wantRefused(t, err, http.StatusConflict, "a second policy on the broker of one sandbox")
	elsewhere := injector.PolicyRef{Namespace: "team-b", Name: "claude"}
	_, err = r.client.PutPolicy(elsewhere, injector.PutPolicyRequest{Version: "1", Policy: claudePolicy("claude", false, "claude-*")})
	wantRefused(t, err, http.StatusConflict, "the same name in another namespace")

	if got := r.putPolicy(t, sandboxesClaude, "2", claudePolicy("claude", false, "claude-haiku-*")); got.Version != "2" {
		t.Errorf("the update of the held policy answered version %q, want 2", got.Version)
	}
	held := r.policies(t)
	if len(held) != 1 || held[0].Namespace != "sandboxes" || held[0].Name != "claude" || held[0].Version != "2" {
		t.Fatalf("the broker holds %+v, want sandboxes/claude version 2 alone", held)
	}

	// Once the held policy is gone, another may take its place: the broker
	// still holds one.
	if code, body := r.control(t, http.MethodDelete, "/policies/sandboxes/claude", ""); code != http.StatusOK {
		t.Fatalf("delete answered %d: %s", code, body)
	}
	r.putPolicy(t, other, "1", claudePolicy("other", false, "claude-*"))
}

// A broker without --sandbox keeps every policy it is given: the limit is
// the per-sandbox broker's alone.
func TestANodeBrokerHoldsManyPolicies(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-*"))
	r.putPolicy(t, injector.PolicyRef{Namespace: "sandboxes", Name: "other"}, "1", claudePolicy("other", false, "claude-*"))
	if n := len(r.policies(t)); n != 2 {
		t.Errorf("the node broker holds %d policies, want 2", n)
	}
}

// A sandbox registered by reference on its own broker is served on the
// listener it was handed, and its policy's rules apply.
func TestASandboxBrokerServesItsPolicyOnTheHandedListener(t *testing.T) {
	u := newUpstream(t)
	cfg := sandboxConfig(t)
	handed := cfg.listener.Addr().String()
	r := startBrokerWith(t, cfg, u)
	if got := listenAddr(r); got != handed {
		t.Fatalf("the routes are on %s, want the handed listener %s", got, handed)
	}
	st, err := r.client.Status()
	if err != nil || st.Port != 0 {
		t.Fatalf("status = %+v, %v; want port 0", st, err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, injector.PortName)); err == nil {
		t.Error("a broker on a handed listener wrote the port file")
	}
	r.putPolicy(t, sandboxesClaude, "1", claudePolicy("claude", false, "claude-sonnet-*"))
	reg := byRef("pod-a", sandboxesClaude)
	reg.Address = "127.0.0.1"
	added, err := r.client.Add(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(added.Routes) != 1 || added.Routes[0].Name != "anthropic" {
		t.Fatalf("the registration answered routes %+v, want anthropic", added.Routes)
	}
	if added.CA == "" {
		t.Fatal("the registration answered no CA")
	}
}

// The CA and host key are the broker directory's, so two per-sandbox
// brokers, each on a directory of its own, present different CAs: a guest
// that trusts its own broker does not trust another's.
func TestTwoSandboxBrokersHaveTwoCAs(t *testing.T) {
	a := startBrokerWith(t, sandboxConfig(t), nil)
	cfgB := sandboxConfig(t)
	cfgB.sandbox = "pod-b"
	b := startBrokerWith(t, cfgB, nil)
	addedA, err := a.client.Add(nodeSandbox("pod-a", "127.0.0.1", inventoryRoute()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.client.Add(nodeSandbox("pod-b", "127.0.0.1", inventoryRoute())); err != nil {
		t.Fatal(err)
	}
	if addedA.CA == "" {
		t.Fatal("no CA")
	}
	key := func(r *running) []byte {
		data, err := os.ReadFile(filepath.Join(r.dir, injector.HostKeyName))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if bytes.Equal(key(a), key(b)) {
		t.Error("the two brokers share a host key")
	}
	// pod-a's client, which trusts pod-a's CA alone, cannot talk to the
	// broker of pod-b.
	cross := nodeClient(listenAddr(b), caPool(t, addedA), nil)
	if resp, err := cross.Get("https://" + injector.BrigInternal + "/"); err == nil {
		_ = resp.Body.Close()
		t.Error("a client that trusts pod-a's CA got an answer from pod-b's broker")
	}
}

// --listen-fd takes a listening TCP socket and nothing else.
func TestInheritedListenerTakesOnlyAListeningTCPSocket(t *testing.T) {
	file := func(t *testing.T, f interface{ File() (*os.File, error) }) int {
		t.Helper()
		of, err := f.File()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = of.Close() })
		return int(of.Fd())
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	ln, err := inheritedListener(file(t, tcp.(*net.TCPListener)))
	if err != nil {
		t.Fatalf("a listening TCP socket was refused: %v", err)
	}
	if ln.Addr().String() != tcp.Addr().String() {
		t.Errorf("the inherited listener is on %s, want %s", ln.Addr(), tcp.Addr())
	}
	_ = ln.Close()

	unix, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	if _, err := inheritedListener(file(t, unix.(*net.UnixListener))); err == nil || !strings.Contains(err.Error(), "not TCP") {
		t.Errorf("a unix socket = %v, want refused as not TCP", err)
	}

	plain, err := os.CreateTemp(shortDir(t), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := inheritedListener(int(plain.Fd())); err == nil {
		t.Error("a regular file was taken as a listener")
	}
}

// The command line refuses --listen with --listen-fd, a descriptor that is
// not a socket and a --sandbox that is not a sandbox id, before it opens the
// directory.
func TestRunRefusesBadSandboxFlags(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	f, err := tcp.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd := strconv.Itoa(int(f.Fd()))
	for name, args := range map[string][]string{
		"both listeners": {"--listen", "127.0.0.1:0", "--listen-fd", fd},
		"closed fd":      {"--listen-fd", "9999"},
		"bad sandbox id": {"--sandbox", "../pod"},
	} {
		dir := filepath.Join(shortDir(t), "broker")
		var stderr bytes.Buffer
		if got := run(append([]string{"--dir", dir}, args...), &stderr, serverConfig{}); got != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, got, stderr.String())
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: the broker directory was created", name)
		}
	}
}
