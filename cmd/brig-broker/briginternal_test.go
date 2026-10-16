package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/injector"
)

// The brig.internal addresses of the sandboxes of these tests: each one's own
// gateway.
const (
	internalA = "198.18.1.1"
	internalB = "198.18.1.5"
)

// internalRig is a broker in this process with sandboxes registered for
// brig.internal, and an upstream every endpoint route reaches.
type internalRig struct {
	*running
	up *upstream
}

func newInternalRig(t *testing.T) *internalRig {
	t.Helper()
	rig := &internalRig{up: newUpstream(t)}
	cfg := serverConfig{dir: shortDir(t), logTo: io.Discard}
	rig.running = startBrokerWith(t, cfg, rig.up)
	if err := rig.store.Update("claude-credentials", fakeDoc("real-access-token", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return rig
}

// add registers id with the claude-code route and the brig.internal address
// internal, and returns the broker's answer.
func (r *internalRig) add(t *testing.T, id, internal string, routes ...injector.Route) injector.AddResponse {
	t.Helper()
	if len(routes) == 0 {
		routes = []injector.Route{claudeRoute()}
	}
	added, err := r.client.Add(injector.Sandbox{ID: id, Profile: "claude-code", Routes: routes, BrigInternal: internal})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket == "" {
		t.Fatal("the broker named no divert socket for a brig.internal sandbox")
	}
	return added
}

// caPool is the pool a guest builds from the CA the broker handed over.
func caPool(t *testing.T, added injector.AddResponse) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(added.CA)) {
		t.Fatalf("the broker's CA is not a PEM certificate: %q", added.CA)
	}
	return pool
}

// guestClient is an HTTPS client of a guest whose brig.internal connections
// the diverter hands to sock, with dst in the preamble. A nil pool trusts the
// system roots alone, as a guest without the brig CA.
func guestClient(sock, dst string, pool *x509.CertPool) *http.Client {
	host, port, _ := net.SplitHostPort(dst)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
			if err != nil {
				return nil, err
			}
			if _, err := fmt.Fprintf(c, "BRIG-DIVERT/1 %s %s 10.100.0.2 40000\n", host, port); err != nil {
				_ = c.Close()
				return nil, err
			}
			return c, nil
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: injector.BrigInternal},
	}}
}

// post sends one request to https://brig.internal/<path>, with token in
// Authorization when it is set, and returns the status.
func post(c *http.Client, path, token string) (int, error) {
	code, _, err := exchange(c, path, token)
	return code, err
}

// exchange is post that also returns the body.
func exchange(c *http.Client, path, token string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+path, strings.NewReader(`{}`))
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body), err
}

// auditWith returns the audit lines that hold every one of want, once one
// does.
func auditWith(t *testing.T, dir string, want ...string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		blob, _ := os.ReadFile(filepath.Join(dir, injector.AuditName))
		var out []string
		for _, l := range strings.Split(string(blob), "\n") {
			hit := l != ""
			for _, w := range want {
				hit = hit && strings.Contains(l, w)
			}
			if hit {
				out = append(out, l)
			}
		}
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A guest that trusts the brig CA reaches its route at brig.internal through
// its divert socket. The broker terminates TLS with the brig.internal
// certificate, checks the session token and sends the real credential
// upstream. The guest's token never goes upstream.
func TestBrigInternalIsServedOnTheDivertSocket(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	token := rig.mint(t, "brig-a", "anthropic")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	for i := range 3 {
		got, err := post(c, "/brig-a/r/anthropic/v1/messages", token)
		if err != nil || got != http.StatusOK {
			t.Fatalf("request %d over brig.internal: %d %v", i, got, err)
		}
	}
	reqs := rig.up.requests()
	if len(reqs) != 3 {
		t.Fatalf("the upstream saw %d requests, want 3", len(reqs))
	}
	for _, h := range reqs {
		if h.Get("Authorization") != "Bearer real-access-token" || strings.Contains(fmt.Sprint(h), token) {
			t.Errorf("the upstream saw %v", h)
		}
	}
	// The connection's line is written when it closes.
	c.CloseIdleConnections()
	if lines := auditWith(t, rig.dir, "msg=divert", "sandbox=brig-a", "transport=brig.internal",
		"dst="+internalA+":443"); len(lines) == 0 {
		t.Error("no audit line names the brig.internal connection")
	}
}

// The CA the broker hands a brig.internal sandbox is one certificate, and
// never its key. It vouches for brig.internal and for no other name.
func TestTheCAHandedOverHoldsNoKey(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	rest := []byte(added.CA)
	var blocks []*pem.Block
	for {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		blocks = append(blocks, b)
	}
	if len(blocks) != 1 || blocks[0].Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("the CA handed over is %d blocks, not one certificate", len(blocks))
	}
	if strings.Contains(added.CA, "PRIVATE KEY") {
		t.Fatal("the CA handed over holds a private key")
	}
	key, err := os.ReadFile(filepath.Join(rig.dir, injector.CAKeyName))
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := pem.Decode(key)
	if kb == nil || strings.Contains(added.CA, string(pem.EncodeToMemory(kb))) {
		t.Fatal("the CA key is in what the broker hands over")
	}
	cert, err := x509.ParseCertificate(blocks[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != injector.BrigInternal {
		t.Errorf("the CA handed over is not constrained to brig.internal: %v", cert.PermittedDNSDomains)
	}
	// A sandbox that reaches the broker over loopback is handed no CA.
	plain, err := rig.client.Add(injector.Sandbox{ID: "brig-h", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}})
	if err != nil {
		t.Fatal(err)
	}
	if plain.CA != "" || plain.DivertSocket != "" {
		t.Errorf("a loopback sandbox was handed %+v", plain)
	}
}

// The sandbox a brig.internal request is for is the one whose socket it
// arrived on. A token of sandbox A sent on B's socket is refused, whichever
// sandbox the path names, and so are a missing token and A's token sent to
// the loopback port. Nothing reaches the upstream.
func TestBrigInternalRefusesATokenOnAnotherSandboxsSocket(t *testing.T) {
	rig := newInternalRig(t)
	a := rig.add(t, "brig-a", internalA)
	b := rig.add(t, "brig-b", internalB)
	tokA := rig.mint(t, "brig-a", "anthropic")
	onB := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	// A's path on B's socket is a sandbox B's guest cannot reach, and reads
	// as one that does not exist.
	for _, c := range []struct {
		name, path, token string
		want              int
	}{
		{"A's path and token on B's socket", "/brig-a/r/anthropic/v1/messages", tokA, http.StatusNotFound},
		{"B's path and A's token on B's socket", "/brig-b/r/anthropic/v1/messages", tokA, http.StatusUnauthorized},
		{"no token", "/brig-b/r/anthropic/v1/messages", "", http.StatusUnauthorized},
	} {
		got, err := post(onB, c.path, c.token)
		if err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, got, err, c.want)
		}
	}
	if lines := auditWith(t, rig.dir, "msg=broker", "status=404", "check=arrival", "arrived=brig-b"); len(lines) == 0 {
		t.Error("no audit line names the arrival check")
	}
	if got := rig.send(t, "brig-a", "anthropic", tokA, "/v1/messages"); got != http.StatusNotFound {
		t.Errorf("A's token on the loopback port got %d, want 404", got)
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	// The same token on A's own socket is served.
	onA := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	if got, err := post(onA, "/brig-a/r/anthropic/v1/messages", tokA); err != nil || got != http.StatusOK {
		t.Errorf("A's token on A's socket: %d %v", got, err)
	}
}

// From its own divert socket, a guest cannot tell another sandbox from one
// that does not exist: a path of another sandbox is a 404 with the body of an
// unknown one, with or without a token, whatever route it names. The audit
// line names the sandbox the request arrived for, not the one in its path.
func TestAnotherSandboxsPathReadsAsUnknownFromThisSocket(t *testing.T) {
	rig := newInternalRig(t)
	a := rig.add(t, "brig-a", internalA)
	rig.add(t, "brig-b", internalB)
	tokB := rig.mint(t, "brig-b", "anthropic")
	onA := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	code, body, err := exchange(onA, "/brig-zzz/r/anthropic/v1/messages", "")
	if err != nil || code != http.StatusNotFound {
		t.Fatalf("an unknown sandbox: %d %v, want 404", code, err)
	}
	for _, c := range []struct{ name, path, token string }{
		{"B's route, no token", "/brig-b/r/anthropic/v1/messages", ""},
		{"B's unknown route, no token", "/brig-b/r/nosuch/v1/messages", ""},
		{"B's route with B's token", "/brig-b/r/anthropic/v1/messages", tokB},
		{"an unknown sandbox with B's token", "/brig-zzz/r/anthropic/v1/messages", tokB},
	} {
		gotCode, gotBody, err := exchange(onA, c.path, c.token)
		if err != nil || gotCode != code || gotBody != body {
			t.Errorf("%s: %d %q %v, want the answer to an unknown sandbox: %d %q", c.name, gotCode, gotBody, err, code, body)
		}
	}
	// The arrival check answers all five, the unknown sandbox too.
	lines := auditWith(t, rig.dir, "msg=broker", "status=404", "check=arrival")
	if len(lines) != 5 {
		t.Errorf("%d audit lines name the arrival check, want 5: %q", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "arrived=brig-a") || strings.Contains(l, "brig-b") {
			t.Errorf("the audit line does not name the sandbox the request arrived for: %s", l)
		}
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// Only port 443 of the brig.internal address is served, and only to a client
// that trusts the brig CA. Neither failure reaches the upstream.
func TestBrigInternalServesTLSOnPort443Alone(t *testing.T) {
	rig := newInternalRig(t)
	added := rig.add(t, "brig-a", internalA)
	token := rig.mint(t, "brig-a", "anthropic")

	// Another port: the broker closes the connection unanswered.
	c, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c, "BRIG-DIVERT/1 %s 80 10.100.0.2 40001\nGET / HTTP/1.1\r\nHost: brig.internal\r\n\r\n", internalA); err != nil {
		t.Fatal(err)
	}
	// The broker closes with the request still unread. Linux reports that
	// close to the peer of a unix socket as a reset, macOS as end of file.
	// Either one is the connection closed unanswered.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 64)); n != 0 ||
		!(errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		t.Errorf("port 80 of brig.internal answered: %d %v", n, err)
	}
	_ = c.Close()
	if lines := auditWith(t, rig.dir, "check=brig.internal.port", "dst="+internalA+":80"); len(lines) == 0 {
		t.Error("no audit line names the refused port")
	}

	// A client without the brig CA does not complete the handshake.
	if _, err := post(guestClient(added.DivertSocket, internalA+":443", x509.NewCertPool()),
		"/brig-a/r/anthropic/v1/messages", token); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a client without the brig CA: %v, want a certificate error", err)
	}
	if lines := auditWith(t, rig.dir, "transport=brig.internal", "check=tls"); len(lines) == 0 {
		t.Error("no audit line names the failed handshake")
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// A registration with a brig.internal address that is not one is refused,
// and opens no socket.
func TestABadBrigInternalAddressIsRefused(t *testing.T) {
	rig := newInternalRig(t)
	for _, bad := range []string{"brig.internal", "0.0.0.0", "127.0.0.1", "224.0.0.1", "198.18.1.01", "fe80::1%en0"} {
		_, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
			Routes: []injector.Route{claudeRoute()}, BrigInternal: bad})
		if err == nil || !strings.Contains(err.Error(), "brig.internal address") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rig.dir, injector.DivertDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused registration left the divert directory: %v", err)
	}
}

// The brig.internal certificate is signed again before it expires, so a
// broker that runs for longer than its lifetime keeps serving.
func TestTheBrigInternalCertificateIsRenewed(t *testing.T) {
	a, err := loadOrCreateCA(shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.current(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	same, _ := a.current(time.Now().Add(leafLifetime - leafRenew - time.Hour))
	if string(same.Certificate[0]) != string(first.Certificate[0]) {
		t.Error("the certificate was signed again well before it expires")
	}
	later := time.Now().Add(leafLifetime - leafRenew + time.Hour)
	next, err := a.current(later)
	if err != nil {
		t.Fatal(err)
	}
	if string(next.Certificate[0]) == string(first.Certificate[0]) {
		t.Fatal("the certificate was not signed again before it expires")
	}
	leaf, _ := x509.ParseCertificate(next.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: injector.BrigInternal, Roots: pool,
		CurrentTime: later.Add(leafRenew)}); err != nil {
		t.Errorf("the renewed certificate does not verify: %v", err)
	}
}
