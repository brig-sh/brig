package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dnsServer answers every UDP query on a loopback port with rcode and no
// answers. It stands in for the gateway's resolver refusing a name, and for
// an alternate resolver the guest was not meant to reach.
func dnsServer(t *testing.T, rcode byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := testReply(buf[:n], rcode); r != nil {
				pc.WriteTo(r, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// testReply turns a query into a reply with rcode, keeping the ID and the
// question and dropping anything after it, EDNS included. It is written apart
// from the probe's own parser so a bug there cannot hide in the fixture.
func testReply(q []byte, rcode byte) []byte {
	if len(q) < 12 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	end := i + 1 + 4
	if end > len(q) {
		return nil
	}
	r := append([]byte(nil), q[:end]...)
	r[2] |= 0x80 // QR
	r[3] = 0x80 | rcode
	binary.BigEndian.PutUint16(r[4:], 1)
	for _, off := range []int{6, 8, 10} {
		binary.BigEndian.PutUint16(r[off:], 0)
	}
	return r
}

// resolverAt points the Go resolver at one server, so a test controls the
// answer a name gets without touching the host's resolv.conf.
func resolverAt(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
}

// closedPort returns a loopback address nothing listens on, so a dial to it
// is answered with a reset.
func closedPort(t *testing.T, network string) string {
	t.Helper()
	switch network {
	case "tcp":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	default:
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := pc.LocalAddr().String()
		pc.Close()
		return addr
	}
}

// hangingListener accepts and then says nothing, the way a gateway that
// swallows traffic after the handshake looks to a client.
func hangingListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return l.Addr().String()
}

// noDial fails the test if the probe opens any connection. A case that has
// already failed at resolution, or has no client, must not go on to dial.
func noDial(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, network, addr string) (net.Conn, error) {
		t.Errorf("dialed %s %s, want no connection at all", network, addr)
		return nil, errors.New("dial refused by test")
	}
}

// testProber has no curl and no resolver to ask again, so no test reaches
// the host's own resolver by accident.
func testProber(timeout time.Duration) *prober {
	p := newProber(timeout)
	p.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	p.nameserver = func() (string, error) { return "", errors.New("no resolver in this test") }
	return p
}

// resolvingAt points both the lookup and the second question at server, the
// way the guest's resolv.conf points both at the gateway.
func resolvingAt(p *prober, server string) {
	p.resolver = resolverAt(server)
	p.nameserver = func() (string, error) { return server, nil }
}

// A name the resolver does not find is a resolution failure, and the probe
// never goes on to dial. A policy that stops a name at the resolver has to be
// told apart from one that lets the name resolve and stops the connection.
func TestNXDOMAINIsNotAConnectFailure(t *testing.T) {
	p := testProber(2 * time.Second)
	resolvingAt(p, dnsServer(t, 3))
	p.dialer = noDial(t)

	r := p.tcp(context.Background(), "denied.example.:443")
	if r.outcome != resolveNXDomain {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, resolveNXDomain)
	}
	if exitCode(r.outcome) != exitNotReached {
		t.Errorf("exit = %d, want %d", exitCode(r.outcome), exitNotReached)
	}
}

// hull's gateway answers REFUSED for a name the policy denies and NXDOMAIN
// for a name its upstream lookup failed on. Only the first is the policy's
// doing, so each rcode is an outcome of its own.
func TestLookupFailureNamesTheRcode(t *testing.T) {
	for rcode, want := range map[byte]outcome{
		5: resolveRefused,
		3: resolveNXDomain,
		2: resolveFailed,
		0: resolveFailed,
	} {
		p := testProber(2 * time.Second)
		resolvingAt(p, dnsServer(t, rcode))
		p.dialer = noDial(t)
		r := p.tcp(context.Background(), "denied.example.:443")
		if r.outcome != want {
			t.Errorf("rcode %d: outcome = %q (%s), want %q", rcode, r.outcome, r.detail, want)
		}
		if exitCode(r.outcome) != exitNotReached {
			t.Errorf("rcode %d: exit = %d, want %d", rcode, exitCode(r.outcome), exitNotReached)
		}
	}
}

// A resolver that never answers a lookup is neither a refusal nor a name it
// did not find.
func TestSilentResolverOnALookupIsResolveTimeout(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	p := testProber(500 * time.Millisecond)
	resolvingAt(p, pc.LocalAddr().String())
	p.dialer = noDial(t)
	r := p.tcp(context.Background(), "denied.example.:443")
	if r.outcome != resolveTimeout {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, resolveTimeout)
	}
}

// With no resolver to ask again, a failed lookup is still a resolution
// failure, and it does not claim an rcode nobody read.
func TestLookupFailureWithNoResolverToAskIsResolveFailed(t *testing.T) {
	p := testProber(2 * time.Second)
	p.resolver = resolverAt(dnsServer(t, 5))
	p.dialer = noDial(t)
	r := p.tcp(context.Background(), "denied.example.:443")
	if r.outcome != resolveFailed || !strings.Contains(r.detail, "no resolver to ask") {
		t.Fatalf("outcome = %q (%s), want %q saying why", r.outcome, r.detail, resolveFailed)
	}
}

func TestFirstNameserver(t *testing.T) {
	for conf, want := range map[string]string{
		"# comment\nsearch example\nnameserver 198.18.0.1\nnameserver 1.1.1.1\n": "198.18.0.1:53",
		"nameserver\tfd00::1\n":         "[fd00::1]:53",
		"options ndots:1\n":             "",
		"nameserver resolver.example\n": "",
	} {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := firstNameserver(path)
		if want == "" {
			if err == nil {
				t.Errorf("%q: got %q, want an error", conf, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v, want %q", conf, got, err, want)
		}
	}
}

// A port that answers with a reset is refused, and it says so at once. A
// probe that waits for the timeout makes a refused port look like a dropped
// one.
func TestRefusedPortIsNotATimeout(t *testing.T) {
	const timeout = 3 * time.Second
	p := testProber(timeout)
	start := time.Now()
	r := p.tcp(context.Background(), closedPort(t, "tcp"))
	if r.outcome != connectRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
	if took := time.Since(start); took > timeout/2 {
		t.Errorf("took %v, want an answer well inside the %v timeout", took, timeout)
	}
}

// With no DoH client in the guest the probe reports client-missing, never a
// pass and never the not-reached code. dohCurl says why.
func TestMissingClientIsAnErrorNeverAPass(t *testing.T) {
	p := testProber(time.Second)
	p.dialer = noDial(t)
	r := p.dohCurl(context.Background(), "https://127.0.0.1:1/dns-query", "example.com")
	if r.outcome != clientMissing {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, clientMissing)
	}
	code := exitCode(r.outcome)
	if code == exitReached || code == exitNotReached {
		t.Errorf("exit = %d, want neither the reached nor the not-reached code", code)
	}
}

// The same through the command line, with a PATH that holds no curl. This is
// the shape the suite sees in a guest image that ships none.
func TestMissingClientOnTheCommandLine(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out, errOut bytes.Buffer
	code := run([]string{"doh", "-client", "curl", "https://127.0.0.1:1/dns-query", "example.com"}, &out, &errOut)
	if code != exitCannotMeasure {
		t.Errorf("exit = %d, want %d; stderr %q", code, exitCannotMeasure, errOut.String())
	}
	if !strings.HasPrefix(out.String(), string(clientMissing)+" ") {
		t.Errorf("stdout = %q, want it to start with %q", out.String(), clientMissing)
	}
}

// An alternate resolver that answers at all has been reached, NXDOMAIN or
// not. The policy question is whether the guest can talk to it, and a reply
// of any kind means it can.
func TestAlternateResolverAnsweringNXDOMAINIsReached(t *testing.T) {
	p := testProber(2 * time.Second)
	r := p.askResolver(context.Background(), dnsServer(t, 3), "example.com")
	if r.outcome != reached {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, reached)
	}
	if !strings.Contains(r.detail, "NXDOMAIN") {
		t.Errorf("detail = %q, want it to name the rcode", r.detail)
	}
}

// A resolver that never answers is a timeout, not a refusal.
func TestSilentResolverIsATimeout(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	p := testProber(300 * time.Millisecond)
	r := p.askResolver(context.Background(), pc.LocalAddr().String(), "example.com")
	if r.outcome != timedOut {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, timedOut)
	}
}

// A resolver that drops the first question and answers the second is
// reached. The resend loop in askResolver says why the question goes again.
func TestFirstQuestionLostIsAskedAgain(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for seen := 0; ; seen++ {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if seen == 0 {
				continue
			}
			pc.WriteTo(testReply(buf[:n], 0), from)
		}
	}()
	p := testProber(5 * time.Second)
	r := p.askResolver(context.Background(), pc.LocalAddr().String(), "example.com")
	if r.outcome != reached {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, reached)
	}
}

// Linux hands the ICMP port unreachable back to a connected UDP socket. That
// is the resolver port being closed, which is a refusal, not silence.
func TestClosedResolverPortIsRefused(t *testing.T) {
	p := testProber(2 * time.Second)
	r := p.askResolver(context.Background(), closedPort(t, "udp"), "example.com")
	if r.outcome != connectRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
}

// A name as the alternate resolver is a usage error. The dns case in run
// says why.
func TestAlternateResolverMustBeAnAddress(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"dns", "dns.google", "example.com"}, &out, &errOut); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
}

// A TLS peer that accepts and then says nothing is an error on DoT, not a
// timeout. The SYN got through, and a timeout is what a dropped SYN gets.
func TestDoTSilentPeerIsAnError(t *testing.T) {
	p := testProber(300 * time.Millisecond)
	r := p.dot(context.Background(), hangingListener(t), "example.com")
	if r.outcome != failedAfterConnect {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, failedAfterConnect)
	}
	if !strings.Contains(r.detail, "connected") {
		t.Errorf("detail = %q, want it to say the TCP connection was made", r.detail)
	}
}

// tlsHangingListener completes a TLS handshake with the certificate httptest
// issues for 127.0.0.1, then says nothing. The peer's identity is proven at
// that point, so whatever stops the reply is past the connection.
func tlsHangingListener(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	l, err := tls.Listen("tcp", "127.0.0.1:0", ts.TLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			go c.(*tls.Conn).Handshake()
		}
	}()
	return l.Addr().String(), roots
}

// A stall after a verified handshake is an error on DoT and DoH, never a
// timeout. An enforcement point that lets the handshake through and drops
// the reply has to read differently from one that drops the SYN.
func TestStallAfterHandshakeIsAnError(t *testing.T) {
	addr, roots := tlsHangingListener(t)
	p := testProber(300 * time.Millisecond)
	p.roots = roots
	for mode, r := range map[string]result{
		"dot": p.dot(context.Background(), addr, "example.com"),
		"doh": p.doh(context.Background(), "https://"+addr+"/dns-query", "example.com"),
	} {
		if r.outcome != failedAfterConnect {
			t.Errorf("%s: outcome = %q (%s), want %q", mode, r.outcome, r.detail, failedAfterConnect)
		}
		if !strings.Contains(r.detail, "then read") {
			t.Errorf("%s: detail = %q, want the failure after the handshake", mode, r.detail)
		}
	}
}

// A bracketed IPv6 literal with no port gets the default port, the way a bare
// one does. The tcp usage line teaches brackets, so people write them here.
func TestWithPortTakesABracketedIPv6Literal(t *testing.T) {
	for _, in := range []string{"2606:4700:4700::1111", "[2606:4700:4700::1111]"} {
		got, err := withPort(in, "853")
		if err != nil {
			t.Fatalf("withPort(%q): %v", in, err)
		}
		host, port, err := net.SplitHostPort(got)
		if err != nil {
			t.Fatalf("withPort(%q) = %q, which does not split: %v", in, got, err)
		}
		if host != "2606:4700:4700::1111" || port != "853" {
			t.Errorf("withPort(%q) = %q, want host 2606:4700:4700::1111 port 853", in, got)
		}
	}
	for in, want := range map[string]string{
		"[2606:4700:4700::1111]:5353": "[2606:4700:4700::1111]:5353",
		"1.1.1.1":                     "1.1.1.1:853",
		"dns.example":                 "dns.example:853",
	} {
		if got, err := withPort(in, "853"); err != nil || got != want {
			t.Errorf("withPort(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

func TestDoTRefusedPort(t *testing.T) {
	p := testProber(2 * time.Second)
	r := p.dot(context.Background(), closedPort(t, "tcp"), "example.com")
	if r.outcome != connectRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
}

// A certificate the guest does not trust is not a reach. It is an error the
// record has to show, since the case measured nothing.
func TestDoTUntrustedCertificateIsAnError(t *testing.T) {
	addr, _ := dotServer(t)
	p := testProber(2 * time.Second)
	r := p.dot(context.Background(), addr, "example.com")
	if r.outcome != failedAfterConnect {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, failedAfterConnect)
	}
	if exitCode(r.outcome) != exitCannotMeasure {
		t.Errorf("exit = %d, want %d", exitCode(r.outcome), exitCannotMeasure)
	}
}

func TestDoTReached(t *testing.T) {
	addr, roots := dotServer(t)
	p := testProber(2 * time.Second)
	p.roots = roots
	r := p.dot(context.Background(), addr, "example.com")
	if r.outcome != reached {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, reached)
	}
}

// dotServer answers DNS over TLS with NXDOMAIN, using the certificate
// httptest issues for 127.0.0.1.
func dotServer(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	l, err := tls.Listen("tcp", "127.0.0.1:0", ts.TLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var n uint16
				if binary.Read(c, binary.BigEndian, &n) != nil {
					return
				}
				q := make([]byte, n)
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				r := testReply(q, 3)
				binary.Write(c, binary.BigEndian, uint16(len(r)))
				c.Write(r)
			}(c)
		}
	}()
	return l.Addr().String(), roots
}

func dohServer(t *testing.T, status int) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "want a POST of application/dns-message", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		q, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(testReply(q, 0))
	}))
	t.Cleanup(ts.Close)
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	return ts, roots
}

func TestDoHReached(t *testing.T) {
	ts, roots := dohServer(t, http.StatusOK)
	p := testProber(2 * time.Second)
	p.roots = roots
	r := p.doh(context.Background(), ts.URL+"/dns-query", "example.com")
	if r.outcome != reached {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, reached)
	}
}

// A DoH endpoint that answers with an HTTP error has not resolved anything.
// A gateway or proxy page in front of the resolver looks like this.
func TestDoHHTTPErrorIsNotAReach(t *testing.T) {
	ts, roots := dohServer(t, http.StatusForbidden)
	p := testProber(2 * time.Second)
	p.roots = roots
	r := p.doh(context.Background(), ts.URL+"/dns-query", "example.com")
	if r.outcome != failedAfterConnect {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, failedAfterConnect)
	}
}

func TestDoHRefusedPort(t *testing.T) {
	p := testProber(2 * time.Second)
	r := p.doh(context.Background(), "https://"+closedPort(t, "tcp")+"/dns-query", "example.com")
	if r.outcome != connectRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
}

// The DoH host name goes through the same resolution step as a plain name,
// so a DoH endpoint the resolver does not find is a resolution failure.
func TestDoHNameNXDOMAIN(t *testing.T) {
	p := testProber(2 * time.Second)
	resolvingAt(p, dnsServer(t, 3))
	p.dialer = noDial(t)
	r := p.doh(context.Background(), "https://doh.denied.example./dns-query", "example.com")
	if r.outcome != resolveNXDomain {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, resolveNXDomain)
	}
}

// fakeCurl writes a curl that prints stdout and stderr and exits with code,
// so each of curl's results can be checked without a network that produces
// it. stdout stands in for what -w "%{http_code}" prints.
func fakeCurl(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "curl")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '" + stdout + "'\nprintf '%s\\n' '" + stderr + "' >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Each case is a line of curl's verbose output with the exit code curl gave
// alongside it. dohCurl says why the verbose output is read.
func TestCurlExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		stderr string
		code   int
		want   outcome
	}{
		{"could not resolve", "000", "curl: (6) Could not resolve host: doh.example", 6, resolveFailed},
		{"refused", "000", "* connect to 1.1.1.1 port 443 from 10.0.0.2 port 5 failed: Connection refused", 7, connectRefused},
		{"no route", "000", "* connect to 1.1.1.1 port 443 from 10.0.0.2 port 5 failed: No route to host", 7, noRoute},
		{"network unreachable", "000", "* Immediate connect fail for 2606:4700::1111: Network is unreachable", 7, noRoute},
		{"exit 7 with no reason", "000", "curl: (7) Failed to connect", 7, failed},
		{"timed out", "000", "curl: (28) Connection timed out after 3001 milliseconds", 28, timedOut},
		// curl says "Connection timed out" for a stall after it connected
		// too. The connect line is what tells the two apart. curl 8.16
		// changed its wording.
		{"timed out after connecting", "000", "* Connected to 1.1.1.1 (1.1.1.1) port 443\ncurl: (28) Connection timed out after 3001 milliseconds", 28, failedAfterConnect},
		{"timed out after connecting, curl 8.16", "000", "* Established connection to 1.1.1.1 (1.1.1.1 port 443) from 10.0.0.2 port 5 \ncurl: (28) Connection timed out after 3001 milliseconds", 28, failedAfterConnect},
		{"proxy hung up", "000", "* Connected to 10.0.0.1 (10.0.0.1) port 3128\ncurl: (56) Proxy CONNECT aborted", 56, failedAfterConnect},
		{"certificate", "000", "curl: (60) SSL certificate problem", 60, failed},
		// The curl counterpart of TestDoHHTTPErrorIsNotAReach.
		{"HTTP 403", "403", "* Connected to 1.1.1.1 (1.1.1.1) port 443", 0, failedAfterConnect},
		{"HTTP 403, curl 8.16", "403", "* Established connection to 1.1.1.1 (1.1.1.1 port 443) from 10.0.0.2 port 5 ", 0, failedAfterConnect},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			curl := fakeCurl(t, c.stdout, c.stderr, c.code)
			p := testProber(2 * time.Second)
			p.lookPath = func(string) (string, error) { return curl, nil }
			r := p.dohCurl(context.Background(), "https://doh.example/dns-query", "example.com")
			if r.outcome != c.want {
				t.Errorf("outcome = %q (%s), want %q", r.outcome, r.detail, c.want)
			}
		})
	}
}

// curl killed at the probe's deadline after it connected has let a packet
// through, the same as a stall its own --max-time ends.
//
// The test kills curl by cancelling the caller's context once the stand-in
// has written its line, which takes the same path as the deadline. A short
// deadline raced the stand-in's startup: under a loaded -race run the kill
// came before the echo, stderr was empty and the probe rightly said timeout.
// A 1 s timeout still lost that race on a busy machine.
func TestCurlKilledAfterConnectIsNotATimeout(t *testing.T) {
	for line, want := range map[string]outcome{
		"* Established connection to 1.1.1.1 (1.1.1.1 port 443) from 10.0.0.2 port 5": failedAfterConnect,
		"* Trying 1.1.1.1:443...": timedOut,
	} {
		dir := t.TempDir()
		path, said := filepath.Join(dir, "curl"), filepath.Join(dir, "said")
		script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo '%s' >&2
# The line is out: the test can kill curl now. The marker sits beside
# this script, so no temp path is spliced into it.
: >"$(dirname "$0")/said"
exec sleep 30
`, line)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		// The deadline stays well past any startup delay, so it fires only
		// if the stand-in never writes its line.
		p := testProber(10 * time.Second)
		p.lookPath = func(string) (string, error) { return path, nil }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		// saw closes before the cancel, so once dohCurl returns it tells a
		// kill by this test from the deadline. The "Trying" case ends in
		// timedOut either way and would pass on the deadline alone.
		saw := make(chan struct{})
		go func() {
			for {
				if _, err := os.Stat(said); err == nil {
					close(saw)
					cancel()
					return
				}
				select {
				case <-done:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
		r := p.dohCurl(ctx, "https://doh.example/dns-query", "example.com")
		close(done)
		cancel()
		select {
		case <-saw:
		default:
			t.Errorf("%q: the stand-in never wrote its marker, so this test did not end curl (%s)", line, r.detail)
		}
		if r.outcome != want {
			t.Errorf("%q: outcome = %q (%s), want %q", line, r.outcome, r.detail, want)
		}
	}
}

// curl says only "Could not resolve host", so the probe asks the resolver
// again for the rcode, as it does for its own lookups.
func TestCurlCouldNotResolveNamesTheRcode(t *testing.T) {
	curl := fakeCurl(t, "000", "curl: (6) Could not resolve host: doh.denied.example", 6)
	p := testProber(2 * time.Second)
	resolvingAt(p, dnsServer(t, 5))
	p.lookPath = func(string) (string, error) { return curl, nil }
	r := p.dohCurl(context.Background(), "https://doh.denied.example./dns-query", "example.com")
	if r.outcome != resolveRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, resolveRefused)
	}
}

// A reply that is not an answer to the question still came back, so the
// path to the resolver was open.
func TestGarbledReplyFromAnAlternateResolverIsAfterConnect(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo([]byte("short"), from)
		}
	}()
	p := testProber(2 * time.Second)
	r := p.askResolver(context.Background(), pc.LocalAddr().String(), "example.com")
	if r.outcome != failedAfterConnect {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, failedAfterConnect)
	}
}

// A guest with a translated locale still gets the connect line in English,
// or a refused port falls through to error.
func TestCurlRunsInTheCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")
	path := filepath.Join(t.TempDir(), "curl")
	script := `#!/bin/sh
cat >/dev/null
if [ "$LC_ALL" = C ]; then why="Connection refused"; else why="Connexion refusée"; fi
echo "* connect to 1.1.1.1 port 443 failed: $why" >&2
exit 7
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := testProber(2 * time.Second)
	p.lookPath = func(string) (string, error) { return path, nil }
	r := p.dohCurl(context.Background(), "https://doh.example/dns-query", "example.com")
	if r.outcome != connectRefused {
		t.Errorf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
}

// The real curl, where the host has one, against a closed port. This holds
// the verbose line the probe reads to the curl that is actually installed.
func TestRealCurlRefusedPort(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this host")
	}
	p := newProber(3 * time.Second)
	r := p.dohCurl(context.Background(), "https://"+closedPort(t, "tcp")+"/dns-query", "example.com")
	if r.outcome != connectRefused {
		t.Fatalf("outcome = %q (%s), want %q", r.outcome, r.detail, connectRefused)
	}
}

// The kernel does not produce an unreachable route or a connect timeout on
// demand on loopback, so those go through the classifier with the errors a
// Linux guest returns. A resolver timeout stays a resolution failure, which
// is why the DNS error is looked at before any errno.
func TestClassify(t *testing.T) {
	dial := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	cases := []struct {
		name string
		err  error
		want outcome
	}{
		{"refused", dial(syscall.ECONNREFUSED), connectRefused},
		{"host unreachable", dial(syscall.EHOSTUNREACH), noRoute},
		{"network unreachable, as for IPv6 with no v6 route", dial(syscall.ENETUNREACH), noRoute},
		{"connect timed out", dial(syscall.ETIMEDOUT), timedOut},
		{"deadline", context.DeadlineExceeded, timedOut},
		{"resolver timeout", &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true,
			UnwrapErr: context.DeadlineExceeded}, resolveFailed},
		{"no such host", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, resolveFailed},
		{"reset", dial(syscall.ECONNRESET), failed},
		// An error before any connection is never marked as one after it.
		{"not permitted", dial(syscall.EPERM), failed},
		{"anything else", errors.New("tls: bad certificate"), failed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// Every outcome maps to a code, and only reached maps to zero. The suite
// branches on these, and a new outcome that falls through to zero passes a
// case it never measured.
func TestExitCodes(t *testing.T) {
	want := map[outcome]int{
		reached:            exitReached,
		resolveRefused:     exitNotReached,
		resolveNXDomain:    exitNotReached,
		resolveTimeout:     exitNotReached,
		resolveFailed:      exitNotReached,
		connectRefused:     exitNotReached,
		timedOut:           exitNotReached,
		noRoute:            exitNotReached,
		clientMissing:      exitCannotMeasure,
		failed:             exitCannotMeasure,
		failedAfterConnect: exitCannotMeasure,
	}
	for o, code := range want {
		if got := exitCode(o); got != code {
			t.Errorf("exitCode(%q) = %d, want %d", o, got, code)
		}
	}
	if exitCode("made-up") != exitCannotMeasure {
		t.Errorf("an unknown outcome maps to %d, want %d", exitCode("made-up"), exitCannotMeasure)
	}
}

// The first word of the line is the outcome, so the suite reads it with
// plain sh.
func TestCommandLineFirstWordIsTheOutcome(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"tcp", "-timeout", "2s", closedPort(t, "tcp")}, &out, &errOut)
	if code != exitNotReached {
		t.Errorf("exit = %d, want %d; stderr %q", code, exitNotReached, errOut.String())
	}
	if f := strings.Fields(out.String()); len(f) < 3 || f[0] != string(connectRefused) || f[1] != "tcp" {
		t.Errorf("stdout = %q, want \"connect-refused tcp <target> ...\"", out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"ping", "example.com"},
		{"tcp"},
		{"tcp", "example.com"},
		{"doh", "http://1.1.1.1/dns-query", "example.com"},
		{"doh", "-client", "wget", "https://1.1.1.1/dns-query", "example.com"},
		// A deadline at or below zero fails every dial before it starts.
		{"tcp", "-timeout", "0s", "127.0.0.1:1"},
		{"dot", "-timeout", "-1s", "1.1.1.1", "example.com"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != exitUsage {
			t.Errorf("run(%q) = %d, want %d", args, code, exitUsage)
		}
	}
}

// A name longer than DNS allows is refused before anything is sent.
func TestQueryRefusesABadName(t *testing.T) {
	for _, name := range []string{"", "a..b", strings.Repeat("x", 64) + ".example"} {
		if _, err := buildQuery(1, name, typeA); err == nil {
			t.Errorf("buildQuery(%q) succeeded, want an error", name)
		}
	}
}
