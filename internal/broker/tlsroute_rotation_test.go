package broker

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/testpki"
	"github.com/brig-sh/brig/internal/tlspem"
)

// validFor is a client chain valid around the clock of the test broker.
func validFor(t *testing.T, ca *testpki.CA, cn string) []byte {
	t.Helper()
	return clientChain(t, ca, cn, rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0))
}

// auditLineFor waits for the audit line of the request for path.
func (r *rig) auditLineFor(path string) string {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(r.audit.String(), "\n") {
			if strings.Contains(line, " path="+path+" ") {
				return line
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no audit line for %s:\n%s", path, r.audit.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitClosed waits for the upstream to have seen n connections close.
func waitClosed(t *testing.T, up *upstream, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for int(up.closed.Load()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the upstream saw %d connections closed, want %d", up.closed.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heldRequest sends a request to the broker in the background and returns
// what comes back.
func (r *rig) heldRequest(sandbox, route, path string) <-chan string {
	r.t.Helper()
	tok := r.mint(sandbox, route)
	req, err := http.NewRequest("GET", r.srv.URL+"/"+sandbox+"/r/"+route+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	out := make(chan string, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out <- resp.Status + " " + string(body)
	}()
	return out
}

// holdingUpstream is an HTTP/2 mTLS upstream whose /hold waits for release,
// so a test keeps a connection busy while it rotates the certificate.
func holdingUpstream(t *testing.T) (up *upstream, started <-chan struct{}, release func()) {
	t.Helper()
	in := make(chan struct{}, 8)
	gate := make(chan struct{})
	var once sync.Once
	up = newMTLSUpstreamH2(t, pki().server, pki().ca.Pool(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			in <- struct{}{}
			<-gate
		}
		_, _ = io.WriteString(w, "ok")
	})
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return up, in, release
}

// A rotation reaches the next request although an HTTP/2 connection to the
// upstream is open and busy: HTTP/2 multiplexes, so a pool that kept the
// connection would send the next request over it with the old certificate.
// The request that was in flight finishes on the connection it started on,
// and the audit line of each names the certificate its connection presented.
func TestRotationReachesTheNextRequestWhileAnHTTP2ConnectionIsBusy(t *testing.T) {
	up, started, release := holdingUpstream(t)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})
	r.creds.set("inventory-client", validFor(t, pki().ca, "cert-a"))

	held := r.heldRequest("brig-a", "inventory", "/hold")
	<-started
	r.creds.set("inventory-client", validFor(t, pki().ca, "cert-b"))

	resp, _ := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	second := up.last(t)
	if second.peer != "cert-b" || second.proto != "HTTP/2.0" {
		t.Fatalf("the next request reached the upstream as %q over %s, want cert-b over HTTP/2.0", second.peer, second.proto)
	}
	if got := up.conns(); got != 2 {
		t.Errorf("%d connections, want 2: the busy one and the one with the new certificate", got)
	}
	if line := r.auditLineFor("/items"); !strings.Contains(line, "cert="+second.peerFP+" ") {
		t.Errorf("the audit line does not name the certificate the upstream saw (%s):\n%s", second.peerFP, line)
	}
	if up.closed.Load() != 0 {
		t.Error("the busy connection was closed under its request")
	}

	release()
	if got := <-held; got != "200 OK ok" {
		t.Fatalf("the request in flight ended with %q", got)
	}
	up.mu.Lock()
	first := up.reqs[0]
	up.mu.Unlock()
	if first.peer != "cert-a" {
		t.Fatalf("the request in flight reached the upstream as %q, want cert-a", first.peer)
	}
	if line := r.auditLineFor("/hold"); !strings.Contains(line, "cert="+first.peerFP+" ") {
		t.Errorf("the audit line of the request in flight does not name cert-a (%s):\n%s", first.peerFP, line)
	}
	// The retired transport has nothing left to carry, and its connection
	// goes with its last request.
	waitClosed(t, up, 1)
	resp, _ = r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).peer; got != "cert-b" {
		t.Errorf("after the rotation the upstream saw %q, want cert-b", got)
	}
}

// The CA is part of what a transport was built for. A connection that was
// opened under the old roots must not carry a request after the CA changed.
func TestRotationOfTheCAReachesTheNextRequest(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "route-ca", false)})
	r.creds.set("route-ca", pki().ca.PEM)

	resp, _ := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	served := up.count()

	other, err := testpki.NewCA("the route's new CA")
	if err != nil {
		t.Fatal(err)
	}
	r.creds.set("route-ca", other.PEM)
	resp, _ = r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.handshake")
	if got := up.count(); got != served {
		t.Fatalf("the upstream served %d requests under a CA that does not vouch for it", got-served)
	}

	r.creds.set("route-ca", pki().ca.PEM)
	resp, _ = r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
}

// An upstream that speaks HTTP/2 takes the certificate as an HTTP/1.1 one
// does.
func TestHTTP2UpstreamAcceptsTheClientCertificate(t *testing.T) {
	up := newMTLSUpstreamH2(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if body != "ok" || got.peer != "inventory-client" || got.proto != "HTTP/2.0" {
		t.Fatalf("body %q, peer %q over %s", body, got.peer, got.proto)
	}
}

// With TLS 1.3 an upstream that speaks HTTP/2 refuses a client certificate
// after the handshake, and net/http reports only that the client connection
// could not be established. For a route that presents a certificate that is a
// handshake failure, and it is named as one.
func TestHTTP2UpstreamRefusingTheClientCertificateIsAHandshakeFailure(t *testing.T) {
	up := newMTLSUpstreamH2(t, pki().server, pki().ca.Pool(), nil)
	other, err := testpki.NewCA("another root")
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, up, []Route{
		certRoute(t, "inventory", up.URL, "inventory-client", "", false),
		customRoute(t, "plain", up.URL, nil),
	})
	r.creds.set("inventory-client", validFor(t, other, "intruder"))
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.handshake")
	if !strings.Contains(body, "tls.handshake") || strings.Contains(body, "http2") {
		t.Errorf("the body does not give the check alone: %q", body)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream served %d requests for a certificate of another CA", n)
	}

	// A route that presents no certificate keeps the general check: its
	// failure is not evidence about a certificate.
	resp, _ = r.get("brig-a", "plain")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=upstream")
}

func TestNotYetValidClientCertificateNeverDials(t *testing.T) {
	r, up := mtlsRig(t)
	start := r.clock.Now().Add(26 * time.Hour)
	r.creds.set("inventory-client", clientChain(t, pki().ca, "soon", start, start.AddDate(1, 0, 0)))
	resp, body := r.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.certificate")
	if !strings.Contains(body, "tls.certificate") {
		t.Errorf("the body does not name the check: %q", body)
	}
	if n := up.conns(); n != 0 {
		t.Fatalf("the broker opened %d connections with a certificate that is not valid yet", n)
	}
}

// A request that looked its route up before the sandbox went must not build
// a transport that nobody closes. A request in flight when the sandbox goes
// finishes on its connection, and the connection closes after it.
func TestRemovedSandboxBuildsNoTransportAfterwards(t *testing.T) {
	up, started, release := holdingUpstream(t)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})

	rt := r.h.sandbox("brig-a").routes["inventory"]
	later := r.mint("brig-a", "inventory")
	held := r.heldRequest("brig-a", "inventory", "/hold")
	<-started
	r.h.RemoveSandbox("brig-a")
	if up.closed.Load() != 0 {
		t.Fatal("the connection of a request in flight was closed under it")
	}
	// The route the lookup found is closed now.
	cc, err := tlspem.ParseClientCertificate(validFor(t, pki().ca, "late"), rigStart)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.routeTransport(rt, nil, [32]byte{}, &cc); !errors.Is(err, errRouteClosed) {
		t.Fatalf("routeTransport on a closed route = %v, want %v", err, errRouteClosed)
	}
	if rt.cur != nil {
		t.Error("a transport was installed on a route whose sandbox is gone")
	}

	release()
	if got := <-held; got != "200 OK ok" {
		t.Fatalf("the request in flight ended with %q", got)
	}
	waitClosed(t, up, 1)

	// A request after the removal is answered as for a sandbox that is not
	// served.
	resp, _ := r.do("GET", "/brig-a/r/inventory/items", bearer(later), "")
	wantStatus(t, resp, http.StatusNotFound)
	if n := up.conns(); n != 1 {
		t.Errorf("%d connections, want the one of the request in flight", n)
	}
}

// The parsed certificates hold private keys. They stay while a registered
// route names the secret, and go with the last route that did.
func TestParsedCertificatesAreDroppedWithTheLastRouteThatNamesThem(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "example-root-ca", false)})
	for _, sb := range []string{"brig-a", "brig-b"} {
		resp, _ := r.get(sb, "inventory")
		wantStatus(t, resp, http.StatusOK)
	}
	cached := func() (int, int) {
		r.h.certMu.Lock()
		defer r.h.certMu.Unlock()
		return len(r.h.certs), len(r.h.cas)
	}
	if c, a := cached(); c != 1 || a != 1 {
		t.Fatalf("cached %d certificates and %d CA bundles, want 1 and 1", c, a)
	}
	r.h.RemoveSandbox("brig-a")
	if c, a := cached(); c != 1 || a != 1 {
		t.Errorf("a route still names the secrets, and %d certificates and %d CA bundles are cached", c, a)
	}
	r.h.RemoveSandbox("brig-b")
	if c, a := cached(); c != 0 || a != 0 {
		t.Errorf("no route names the secrets, and %d certificates and %d CA bundles are cached", c, a)
	}
	// A replacing AddSandbox whose routes name other secrets drops them too.
	if err := r.h.AddSandbox(Sandbox{ID: "brig-c", Routes: []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)}}); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.get("brig-c", "inventory")
	wantStatus(t, resp, http.StatusOK)
	if err := r.h.AddSandbox(Sandbox{ID: "brig-c", Routes: []Route{customRoute(t, "plain", up.URL, nil)}}); err != nil {
		t.Fatal(err)
	}
	if c, a := cached(); c != 0 || a != 0 {
		t.Errorf("the replaced sandbox's secrets are cached: %d certificates, %d CA bundles", c, a)
	}
}

// No TLS state is shared across routes through the base transport: not a
// session cache, which would resume a session of another sandbox or another
// certificate, not a key log, and not a switch that turns verification off.
func TestRouteTransportSharesNoTLSStateWithTheBase(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	base := up.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(8)
	base.TLSClientConfig.KeyLogWriter = io.Discard
	base.TLSClientConfig.InsecureSkipVerify = true
	base.TLSClientConfig.Certificates = []tls.Certificate{pki().server}
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)}, withTransport(base))

	rt := r.h.sandbox("brig-a").routes["inventory"]
	cc, err := tlspem.ParseClientCertificate(validFor(t, pki().ca, "c"), rigStart)
	if err != nil {
		t.Fatal(err)
	}
	tt, err := r.h.routeTransport(rt, nil, [32]byte{}, &cc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := tt.tr.TLSClientConfig
	if cfg.ClientSessionCache != nil || cfg.KeyLogWriter != nil || cfg.InsecureSkipVerify || len(cfg.Certificates) != 0 {
		t.Errorf("the route's TLS config shares state with the base: cache %v, key log %v, skip verify %v, %d certificates",
			cfg.ClientSessionCache != nil, cfg.KeyLogWriter != nil, cfg.InsecureSkipVerify, len(cfg.Certificates))
	}
	// And the base itself is as it was.
	if base.TLSClientConfig.ClientSessionCache == nil || !base.TLSClientConfig.InsecureSkipVerify {
		t.Error("building the route's transport changed the base")
	}
	// A route that skipped verification would reach an upstream that no
	// root vouches for.
	other, err := testpki.NewCA("an unrelated root")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := other.Server()
	if err != nil {
		t.Fatal(err)
	}
	stranger := newMTLSUpstream(t, srv, pki().ca.Pool(), nil)
	r2 := newRig(t, stranger, []Route{certRoute(t, "inventory", stranger.URL, "inventory-client", "", false)}, withTransport(base))
	resp, _ := r2.get("brig-a", "inventory")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r2.lastAudit(), "check=tls.handshake")
	if n := stranger.count(); n != 0 {
		t.Fatalf("an upstream that no root vouches for served %d requests", n)
	}
}

// Two requests of one route that resolved different certificates each ride on
// the transport of their own, and the audit line says which.
func TestTransportsAreKeyedByTheCertificateTheyPresent(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	r := newRig(t, up, []Route{certRoute(t, "inventory", up.URL, "inventory-client", "", false)})
	rt := r.h.sandbox("brig-a").routes["inventory"]
	a, err := tlspem.ParseClientCertificate(validFor(t, pki().ca, "a"), rigStart)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tlspem.ParseClientCertificate(validFor(t, pki().ca, "b"), rigStart)
	if err != nil {
		t.Fatal(err)
	}
	t1, err := r.h.routeTransport(rt, nil, [32]byte{}, &a)
	if err != nil {
		t.Fatal(err)
	}
	t1again, err := r.h.routeTransport(rt, nil, [32]byte{}, &a)
	if err != nil || t1again != t1 {
		t.Fatalf("the same certificate got another transport: %v", err)
	}
	t2, err := r.h.routeTransport(rt, nil, [32]byte{}, &b)
	if err != nil || t2 == t1 {
		t.Fatalf("another certificate got the same transport: %v", err)
	}
	if !t1.retired || t2.retired {
		t.Errorf("retired: old %v, new %v", t1.retired, t2.retired)
	}
	c1, _ := t1.tr.TLSClientConfig.GetClientCertificate(nil)
	c2, _ := t2.tr.TLSClientConfig.GetClientCertificate(nil)
	if string(c1.Certificate[0]) != string(a.Leaf.Raw) || string(c2.Certificate[0]) != string(b.Leaf.Raw) {
		t.Error("a transport does not present the certificate it was built for")
	}
}

// The upstream closes the connection after it refuses the certificate. When
// the broker writes before it reads the alert, the write fails and the alert
// is never read. TestHTTP2UpstreamRefusingTheClientCertificateIsAHandshakeFailure
// reaches this only now and then, so the classification is checked here.
func TestAResetIsAHandshakeFailureOnlyOnANewConnection(t *testing.T) {
	for _, err := range []error{
		&net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}},
		&net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}},
		errors.New("http2: client conn could not be established"),
	} {
		if !tlsHandshakeFailed(err, true) {
			t.Errorf("%v on a new connection is not tls.handshake", err)
		}
		// A kept-alive connection finished its handshake long ago.
		if tlsHandshakeFailed(err, false) {
			t.Errorf("%v on a reused connection is tls.handshake", err)
		}
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	if tlsHandshakeFailed(refused, true) {
		t.Error("a refused dial is tls.handshake")
	}
}

// The classification above is only as good as the report of a new
// connection, so the report is checked over HTTP/1.1 and HTTP/2.
func TestRoundTripReportsANewConnectionOnlyOnce(t *testing.T) {
	for name, up := range map[string]*upstream{
		"http/1.1": newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil),
		"http/2":   newMTLSUpstreamH2(t, pki().server, pki().ca.Pool(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			cert, err := tls.X509KeyPair(pki().clientCert, pki().clientKey)
			if err != nil {
				t.Fatal(err)
			}
			tr := defaultHTTPTransport()
			tr.ForceAttemptHTTP2 = name == "http/2"
			tr.TLSClientConfig = &tls.Config{RootCAs: pki().ca.Pool(), Certificates: []tls.Certificate{cert}}
			defer tr.CloseIdleConnections()
			for i, want := range []bool{true, false} {
				req, err := http.NewRequest(http.MethodGet, up.URL+"/items", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, newConn, err := roundTrip(tr, req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if newConn != want {
					t.Errorf("request %d: newConn = %v, want %v", i+1, newConn, want)
				}
			}
		})
	}
}
