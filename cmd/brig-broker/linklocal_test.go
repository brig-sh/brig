package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// linkLocalRig is a broker with the fake aws, an upstream every endpoint
// route reaches, and sandboxes registered for brig.internal with an aws route
// and the claude-code route.
type linkLocalRig struct {
	*running
	up *upstream
}

func newLinkLocalRig(t *testing.T) *linkLocalRig {
	t.Helper()
	up := newUpstream(t)
	r, _ := startAWSBroker(t, serverConfig{logTo: io.Discard, transport: up.transport()})
	return &linkLocalRig{running: r, up: up}
}

// add registers id with the aws and claude-code routes and the brig.internal
// address internal.
func (r *linkLocalRig) add(t *testing.T, id, internal string) injector.AddResponse {
	t.Helper()
	added, err := r.client.Add(injector.Sandbox{ID: id, Profile: "claude-code",
		Routes: []injector.Route{awsRoute(), claudeRoute()}, BrigInternal: internal})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket == "" {
		t.Fatal("the broker named no divert socket for a brig.internal sandbox")
	}
	return added
}

// reads is how many times the store was read.
func (r *linkLocalRig) reads() int {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.store.reads
}

// linkLocalClient is a plain HTTP client of a guest whose connections to the
// link-local address the diverter hands to sock, as the AWS SDK's container
// provider sends them.
func linkLocalClient(sock string) *http.Client {
	host, port, _ := net.SplitHostPort(injector.LinkLocalAddr)
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
	}}
}

// linkLocalGet sends one GET to http://<link-local>/<path>, with token as the
// whole Authorization value when it is set, and returns the status and body.
func linkLocalGet(c *http.Client, path, token string) (int, string, error) {
	host, _, _ := net.SplitHostPort(injector.LinkLocalAddr)
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body), err
}

// wantClosedUnanswered reads from c and fails unless the broker closed it
// without a byte. Linux reports that close to the peer of a unix socket as a
// reset, macOS as end of file.
func wantClosedUnanswered(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 64)); n != 0 ||
		!(errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		t.Errorf("%s answered: %d %v", what, n, err)
	}
}

// The AWS SDK's container provider takes plain HTTP from the link-local
// address alone. A guest that asks its aws route there, through its divert
// socket, gets the vended keys, and the connection's audit line names the
// transport.
func TestLinkLocalServesAVendingRouteInPlainHTTP(t *testing.T) {
	rig := newLinkLocalRig(t)
	added := rig.add(t, "brig-a", internalA)
	c := linkLocalClient(added.DivertSocket)
	code, body, err := linkLocalGet(c, "/brig-a/r/aws", rig.mint(t, "brig-a", "aws"))
	if err != nil || code != http.StatusOK || !strings.Contains(body, fakeAWSSecret) {
		t.Fatalf("vend over the link-local address: %d %q %v", code, body, err)
	}
	c.CloseIdleConnections()
	if lines := auditWith(t, rig.dir, "msg=divert", "sandbox=brig-a", "transport=link-local",
		"dst="+injector.LinkLocalAddr); len(lines) == 0 {
		t.Error("no audit line names the link-local connection")
	}
	if lines := auditWith(t, rig.dir, "msg=broker", "status=200", "route=aws", "arrived=brig-a"); len(lines) == 0 {
		t.Error("no audit line names the vend")
	}
}

// A route that does not vend is not served in clear text. Its token, sent to
// the link-local address, is refused as a route the sandbox does not have:
// its credential is never read, nothing reaches the upstream, and the token
// is in no log. The same request over brig.internal reads the credential, so
// the count this test reads does move.
func TestLinkLocalRefusesARouteThatDoesNotVend(t *testing.T) {
	rig := newLinkLocalRig(t)
	added := rig.add(t, "brig-a", internalA)
	tok := rig.mint(t, "brig-a", "anthropic")
	before := rig.reads()
	for _, value := range []string{tok, "Bearer " + tok} {
		code, _, err := linkLocalGet(linkLocalClient(added.DivertSocket), "/brig-a/r/anthropic/v1/models", value)
		if err != nil || code != http.StatusNotFound {
			t.Errorf("anthropic over the link-local address with %.6q...: %d %v, want 404", value, code, err)
		}
	}
	if n := rig.reads() - before; n != 0 {
		t.Errorf("the refused route read the store %d times", n)
	}
	if n := len(rig.up.requests()); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if lines := auditWith(t, rig.dir, "msg=broker", "status=404", "check=route.cleartext",
		"arrived=brig-a"); len(lines) != 2 {
		t.Errorf("%d audit lines name the clear-text refusal, want 2: %q", len(lines), lines)
	}
	// The control: over brig.internal the same token reads the store and
	// reaches the upstream.
	onA := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if got, err := post(onA, "/brig-a/r/anthropic/v1/messages", tok); err != nil || got != http.StatusOK {
		t.Fatalf("anthropic over brig.internal: %d %v", got, err)
	}
	if rig.reads() == before || len(rig.up.requests()) != 1 {
		t.Fatal("the request over brig.internal read no credential, so this test checks nothing")
	}
	err := filepath.WalkDir(rig.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(blob), tok) {
			t.Errorf("%s holds the session token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The sandbox a link-local request is for is the one whose socket it arrived
// on, as on brig.internal. A's token on B's socket is refused whichever
// sandbox the path names, and no key is vended for it.
func TestLinkLocalRefusesATokenOnAnotherSandboxsSocket(t *testing.T) {
	rig := newLinkLocalRig(t)
	a := rig.add(t, "brig-a", internalA)
	b := rig.add(t, "brig-b", internalB)
	tokA := rig.mint(t, "brig-a", "aws")
	onB := linkLocalClient(b.DivertSocket)
	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"A's path and token on B's socket", "/brig-a/r/aws", http.StatusNotFound},
		{"B's path and A's token on B's socket", "/brig-b/r/aws", http.StatusUnauthorized},
	} {
		code, body, err := linkLocalGet(onB, c.path, tokA)
		if err != nil || code != c.want || strings.Contains(body, fakeAWSAccess) {
			t.Errorf("%s: %d %q %v, want %d", c.name, code, body, err, c.want)
		}
	}
	if lines := auditWith(t, rig.dir, "msg=broker", "status=404", "check=arrival", "arrived=brig-b"); len(lines) == 0 {
		t.Error("no audit line names the arrival check")
	}
	// The same token on A's own socket is served.
	if code, _, err := linkLocalGet(linkLocalClient(a.DivertSocket), "/brig-a/r/aws", tokA); err != nil ||
		code != http.StatusOK {
		t.Errorf("A's token on A's socket: %d %v", code, err)
	}
}

// Only port 80 of the link-local address is served. Another port is closed
// unanswered, as any destination nothing claims.
func TestLinkLocalOtherPortIsClosed(t *testing.T) {
	rig := newLinkLocalRig(t)
	added := rig.add(t, "brig-a", internalA)
	host, _, _ := net.SplitHostPort(injector.LinkLocalAddr)
	c, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := fmt.Fprintf(c, "BRIG-DIVERT/1 %s 443 10.100.0.2 40001\nGET /brig-a/r/aws HTTP/1.1\r\nHost: %s\r\n\r\n",
		host, host); err != nil {
		t.Fatal(err)
	}
	wantClosedUnanswered(t, c, "port 443 of the link-local address")
	if lines := auditWith(t, rig.dir, "check=link-local.port", "dst="+host+":443"); len(lines) == 0 {
		t.Error("no audit line names the refused port")
	}
}

// A TLS client on port 80 gets no handshake and no credential, and the broker
// goes on serving the next request. The broker answers the ClientHello at
// once: a handshake that only times out would mean a stuck connection that
// holds its slot.
func TestLinkLocalRefusesATLSClientHello(t *testing.T) {
	rig := newLinkLocalRig(t)
	added := rig.add(t, "brig-a", internalA)
	host, port, _ := net.SplitHostPort(injector.LinkLocalAddr)
	raw, err := net.Dial("unix", added.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(raw, "BRIG-DIVERT/1 %s %s 10.100.0.2 40002\n", host, port); err != nil {
		t.Fatal(err)
	}
	// Well under the broker's 10s header timeout, so a broker that waits for
	// more bytes fails here rather than passing on its own timeout.
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	tc := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: x509.NewCertPool()})
	err = tc.Handshake()
	_ = tc.Close()
	if err == nil {
		t.Fatal("a TLS handshake on port 80 of the link-local address completed")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the broker did not answer a TLS ClientHello on port 80: %v", err)
	}
	if code, body, err := linkLocalGet(linkLocalClient(added.DivertSocket), "/brig-a/r/aws",
		rig.mint(t, "brig-a", "aws")); err != nil || code != http.StatusOK || !strings.Contains(body, fakeAWSAccess) {
		t.Fatalf("the vend after a TLS client: %d %q %v", code, body, err)
	}
}

// The clear-text refusal is for the link-local address alone. A sandbox that
// reaches the broker on the loopback port, with no brig.internal address, is
// served its endpoint route there as before.
func TestLoopbackServesARouteThatDoesNotVend(t *testing.T) {
	rig := newLinkLocalRig(t)
	if _, err := rig.client.Add(injector.Sandbox{ID: "brig-c", Profile: "claude-code",
		Routes: []injector.Route{awsRoute(), claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	tok := rig.mint(t, "brig-c", "anthropic")
	if code := rig.send(t, "brig-c", "anthropic", tok, "/v1/messages"); code != http.StatusOK {
		t.Fatalf("anthropic on the loopback port: %d, want 200", code)
	}
	if n := len(rig.up.requests()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
	if lines := auditWith(t, rig.dir, "msg=broker", "status=200", "route=anthropic"); len(lines) == 0 {
		t.Error("no audit line names the loopback request as served")
	}
}
