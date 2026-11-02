package broker

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/testpki"
)

// mtlsRig is a broker in front of an upstream that takes client certificates
// from the shared test CA, with one cert-only route, "inventory".
func mtlsRig(t *testing.T, opts ...rigOption) (*rig, *upstream) {
	t.Helper()
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)}, opts...)
	return r, up
}

func (r *rig) get(sandbox, route string) (*http.Response, string) {
	r.t.Helper()
	return r.do("GET", "/"+sandbox+"/r/"+route+"/items", bearer(r.mint(sandbox, route)), "")
}

func TestClientCertificateIsPresentedUpstream(t *testing.T) {
	r, up := mtlsRig(t)
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if body != "ok" {
		t.Fatalf("body %q", body)
	}
	if got := up.last(t).peer; got != "inventory-client" {
		t.Fatalf("the upstream saw the client %q, want inventory-client", got)
	}
	line := r.lastAudit()
	if !regexp.MustCompile(`cert=sha256:[0-9a-f]{16}\b`).MatchString(line) {
		t.Errorf("audit line lacks cert=sha256:<16 hex>:\n%s", line)
	}
	if !regexp.MustCompile(`cert\.notAfter=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`).MatchString(line) {
		t.Errorf("audit line lacks cert.notAfter=<RFC3339>:\n%s", line)
	}
	if strings.Contains(line, "check=") {
		t.Errorf("a served request carries a check:\n%s", line)
	}
}

func TestRouteWithoutAClientCertificateIsRefusedUpstream(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{customRoute(t, "inventory", up.URL, nil)})
	resp, _ := r.do("GET", "/brig-a/r/inventory/items", bearer(r.mint("brig-a", "inventory")), "")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=upstream")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream handler ran %d times for a client without a certificate", n)
	}
}

func TestClientCertificateFromAnotherCAIsRefused(t *testing.T) {
	r, up := mtlsRig(t)
	other, err := testpki.NewCA("another root")
	if err != nil {
		t.Fatal(err)
	}
	r.creds.set("inventory-client", clientChain(t, other, "intruder", rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0)))
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.handshake")
	if !strings.Contains(body, "inventory") || !strings.Contains(body, "tls.handshake") {
		t.Errorf("the body does not name the route and the check: %q", body)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream served %d requests for a certificate of another CA", n)
	}
}

func TestTwoSandboxesNeverShareAClientCertificateConnection(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, nil, withSandboxes(
		Sandbox{ID: "brig-a", Profile: "p", Routes: []Route{certRoute(t, "inventory", up.URL, "cert-a", "", false)}},
		Sandbox{ID: "brig-b", Profile: "p", Routes: []Route{certRoute(t, "inventory", up.URL, "cert-b", "", false)}},
	))
	valid := func(cn string) []byte {
		return clientChain(t, pki().ca, cn, rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0))
	}
	r.creds.set("cert-a", valid("sandbox-a"))
	r.creds.set("cert-b", valid("sandbox-b"))
	toks := map[string]string{"brig-a": r.mint("brig-a", "inventory"), "brig-b": r.mint("brig-b", "inventory")}
	want := map[string]string{"brig-a": "sandbox-a", "brig-b": "sandbox-b"}
	order := []string{"brig-a", "brig-b"}
	for i := 0; i < 20; i++ {
		sb := order[i%2]
		resp, _ := r.do("GET", fmt.Sprintf("/%s/r/inventory/items?i=%d", sb, i), bearer(toks[sb]), "")
		wantStatus(t, resp, http.StatusOK)
		if got := up.last(t).peer; got != want[sb] {
			t.Fatalf("request %d from %s reached the upstream as %q, want %q", i, sb, got, want[sb])
		}
	}
	up.mu.Lock()
	n := len(up.reqs)
	up.mu.Unlock()
	if got := up.conns(); got != 2 {
		t.Fatalf("the upstream accepted %d connections for 2 sandboxes, want 2", got)
	}
	if n <= up.conns() {
		t.Fatalf("%d requests over %d connections: the keep-alive connections were not reused", n, up.conns())
	}
}

func TestExpiredClientCertificateNeverDials(t *testing.T) {
	r, up := mtlsRig(t)
	end := r.clock.Now().Add(-time.Hour)
	r.creds.set("inventory-client", clientChain(t, pki().ca, "expired", end.Add(-48*time.Hour), end))
	before, _ := r.creds.counts()
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.expired")
	if !strings.Contains(body, "inventory") || !strings.Contains(body, "tls.expired") {
		t.Errorf("the body does not name the route and the check: %q", body)
	}
	if n := up.conns(); n != 0 {
		t.Fatalf("the broker opened %d connections with an expired certificate", n)
	}
	if after, _ := r.creds.counts(); after == before {
		t.Error("the certificate secret was not resolved")
	}
}

func TestUpstreamNotChainingToTheRouteCAIsRefused(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	other, err := testpki.NewCA("the route's CA")
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "route-ca", false)})
	r.creds.set("route-ca", other.PEM)
	resp, _ := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.handshake")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream served %d requests although its certificate does not chain to the route's CA", n)
	}
}

func TestRouteCAReplacesTheSystemRoots(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	// The base trusts nothing, so only a ca can make the handshake pass.
	base := up.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.RootCAs = x509.NewCertPool()
	r := newRig(t, up, []Route{
		certRoute(t, "withca", up.URL, "inventory-client", "example-root-ca", false),
		certRoute(t, "noca", up.URL, "inventory-client", "", false),
		customRoute(t, "plain", up.URL, nil),
	}, withTransport(base))

	resp, _ := r.get("brig-a", "withca")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).peer; got != "inventory-client" {
		t.Fatalf("peer %q", got)
	}

	n := up.count()
	resp, _ = r.get("brig-a", "plain")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=upstream")
	resp, _ = r.get("brig-a", "noca")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.handshake")
	if up.count() != n {
		t.Fatalf("a route served the request through the trust of the base transport")
	}
}

func TestRotatedClientCertificateIsPresentedOnTheNextConnection(t *testing.T) {
	r, up := mtlsRig(t)
	r.creds.set("inventory-client", clientChain(t, pki().ca, "cert-a", rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0)))
	tok := r.mint("brig-a", "inventory")
	resp, _ := r.do("GET", "/brig-a/r/inventory/items", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).peer; got != "cert-a" {
		t.Fatalf("peer %q, want cert-a", got)
	}
	r.creds.set("inventory-client", clientChain(t, pki().ca, "cert-b", rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0)))
	resp, _ = r.do("GET", "/brig-a/r/inventory/items", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).peer; got != "cert-b" {
		t.Fatalf("peer %q after the rotation, want cert-b", got)
	}
	if n := up.conns(); n != 2 {
		t.Errorf("%d connections, want 2: the rotation must not keep serving on the old one", n)
	}
}

// keyMaterial returns the strings that would show a private key: its PEM
// marker and the base64 of its DER.
func keyMaterial(t *testing.T, chain []byte) []string {
	t.Helper()
	out := []string{"PRIVATE KEY"}
	for rest := chain; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if strings.HasSuffix(b.Type, "PRIVATE KEY") {
			out = append(out, base64.StdEncoding.EncodeToString(b.Bytes)[:40])
		}
	}
	return out
}

// A route with a client certificate and no credential sets no header, so
// nothing overwrites the guest's Authorization, and the broker has to drop it
// for the session token to stay away from the upstream.
func TestCertificateOnlyRouteSendsNoSessionTokenUpstream(t *testing.T) {
	for name, up := range map[string]*upstream{
		"http/1.1": newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil),
		"http/2":   newMTLSUpstreamH2(t, pki().server, pki().ca.Pool(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})
			tok := r.mint("brig-a", "inventory")
			resp, _ := r.do("GET", "/brig-a/r/inventory/items", bearer(tok), "")
			wantStatus(t, resp, http.StatusOK)
			got := up.last(t)
			if v, ok := got.header["Authorization"]; ok {
				t.Fatalf("the upstream got Authorization %q", v)
			}
			for k, vs := range got.header {
				for _, v := range vs {
					if strings.Contains(v, tok) {
						t.Fatalf("the session token reached the upstream in %s", k)
					}
				}
			}
		})
	}
}

func TestTheClientKeyNeverReachesAuditLogOrBody(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	r, _ := mtlsRig(t)
	chain := append(bytes.Clone(pki().clientCert), pki().clientKey...)
	secrets := keyMaterial(t, chain)

	var bodies []string
	_, b := r.get("brig-a", "inventory")
	bodies = append(bodies, b)
	// A damaged secret, and the same one expired, fail in different places.
	r.creds.set("inventory-client", append(bytes.Clone(pki().clientCert), pki().clientKey[:len(pki().clientKey)/2]...))
	_, b = r.get("brig-a", "inventory")
	bodies = append(bodies, b)
	end := r.clock.Now().Add(-time.Hour)
	expired := clientChain(t, pki().ca, "expired", end.Add(-time.Hour), end)
	r.creds.set("inventory-client", expired)
	// The expired certificate has a key of its own, and it is searched for
	// too: a leak on this path would not carry the other key.
	secrets = append(secrets, keyMaterial(t, expired)...)
	_, b = r.get("brig-a", "inventory")
	bodies = append(bodies, b)

	all := append(bodies, r.audit.String(), logs.String())
	for _, text := range all {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("key material %q... reached the audit log, a log or a body:\n%s", s[:min(len(s), 12)], text)
			}
		}
	}
}

func TestBadClientCertificateSecretIsTLSCertificate(t *testing.T) {
	for name, set := range map[string]func(*fakeCreds){
		"damaged": func(f *fakeCreds) {
			f.set("inventory-client", []byte("-----BEGIN CERTIFICATE-----\nZ2FyYmFnZQ==\n-----END CERTIFICATE-----\n"))
		},
		"keyless": func(f *fakeCreds) { f.set("inventory-client", pki().clientCert) },
		"missing": func(f *fakeCreds) { f.drop("inventory-client") },
	} {
		t.Run(name, func(t *testing.T) {
			r, up := mtlsRig(t)
			set(r.creds)
			resp, body := r.get("brig-a", "inventory")
			wantStatus(t, resp, http.StatusBadGateway)
			wantAudit(t, r.lastAudit(), "check=tls.certificate")
			if !strings.Contains(body, "route inventory") || !strings.Contains(body, "tls.certificate") {
				t.Errorf("the body does not name the route and the check: %q", body)
			}
			for _, leak := range []string{"BEGIN", "garbage", "not found", "ErrNotFound", "Z2FyYmFnZQ"} {
				if strings.Contains(body, leak) {
					t.Errorf("the body holds %q: %q", leak, body)
				}
			}
			if n := up.conns(); n != 0 {
				t.Fatalf("%d connections were opened without a usable certificate", n)
			}
		})
	}
}

func TestBadCASecretIsTLSCA(t *testing.T) {
	for name, set := range map[string]func(*fakeCreds){
		"damaged": func(f *fakeCreds) { f.set("example-root-ca", []byte("not a pem bundle")) },
		"missing": func(f *fakeCreds) { f.drop("example-root-ca") },
	} {
		t.Run(name, func(t *testing.T) {
			up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
			r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "example-root-ca", false)})
			set(r.creds)
			resp, body := r.get("brig-a", "inventory")
			wantStatus(t, resp, http.StatusBadGateway)
			wantAudit(t, r.lastAudit(), "check=tls.ca")
			if !strings.Contains(body, "route inventory") || !strings.Contains(body, "tls.ca") {
				t.Errorf("the body does not name the route and the check: %q", body)
			}
			for _, leak := range []string{"BEGIN", "not a pem", "not found"} {
				if strings.Contains(body, leak) {
					t.Errorf("the body holds %q: %q", leak, body)
				}
			}
			if n := up.conns(); n != 0 {
				t.Fatalf("%d connections were opened with an unusable CA", n)
			}
		})
	}
}

func TestRemovedSandboxDropsItsTransports(t *testing.T) {
	waitClosed := func(up *upstream, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for int(up.closed.Load()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("the upstream saw %d connections closed, want %d", up.closed.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	r, up := mtlsRig(t)
	resp, _ := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if up.closed.Load() != 0 {
		t.Fatal("the connection closed before the sandbox went")
	}
	r.h.RemoveSandbox("brig-a")
	waitClosed(up, 1)

	// A replacing AddSandbox closes the transports of the sandbox it replaces.
	sb := Sandbox{ID: "brig-b", Profile: "claude-code", Routes: []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)}}
	resp, _ = r.get("brig-b", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if err := r.h.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	waitClosed(up, 2)
	resp, _ = r.get("brig-b", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if n := up.conns(); n != 3 {
		t.Errorf("%d connections, want 3: one for each registration", n)
	}
}

func TestACertificateOnlyRouteIsAccepted(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})
	if err := r.h.AddSandbox(Sandbox{ID: "brig-c", Routes: []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)}}); err != nil {
		t.Fatal(err)
	}
	// A route with neither a credential nor a certificate is still refused.
	bad := certRoute(t, "inventory", up.URL, "inventory-client", "", false)
	bad.Route.Inject.TLS.ClientCertificate = nil
	if err := r.h.AddSandbox(Sandbox{ID: "brig-d", Routes: []Route{bad}}); err == nil {
		t.Fatal("a route with no credential and no client certificate was accepted")
	}
}

func TestCredentialAndCertificateTogether(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "example-root-ca", true)})
	resp, _ := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if got.peer != "inventory-client" || got.header.Get("X-Api-Key") != "fake-inventory-token" {
		t.Fatalf("peer %q, X-Api-Key %q", got.peer, got.header.Get("X-Api-Key"))
	}
}
