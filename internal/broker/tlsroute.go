package broker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/tlspem"
)

// tlsFailure is a failure of the TLS side of a route: the secret that holds
// its client certificate or its CA, an expired certificate, or the handshake
// with the upstream. The audit line and the guest get check, and the cause
// stays out of both, since a backend error or a parse error is the host's.
type tlsFailure struct {
	check string
	cause error
}

func (f *tlsFailure) Error() string { return "brig broker: " + f.check }
func (f *tlsFailure) Unwrap() error { return f.cause }

// errRouteClosed is the answer for a route whose sandbox was removed or
// replaced while a request that had already looked the route up was on its
// way upstream. The sandbox is not served any more, and the request is
// answered as for one that never was.
var errRouteClosed = errors.New("the sandbox of this route is not served any more")

// certEntry is a parsed client certificate and the digest of the bytes it
// came from.
type certEntry struct {
	sum  [32]byte
	cert *tlspem.ClientCertificate
}

// caEntry is a parsed CA bundle and the digest of the bytes it came from.
type caEntry struct {
	sum  [32]byte
	pool *x509.CertPool
}

// tlsKey identifies what a transport was built for: the CA bundle it trusts
// and the chain it presents. A route keeps one transport at a time, and a
// change of either half builds another.
type tlsKey struct {
	ca, cert [32]byte
}

// tlsTransport is a transport of a route with inject.tls, with the
// certificate it presents. The certificate belongs to the transport: its
// GetClientCertificate returns this one and no other, so the connection that
// a request rides on, the certificate the upstream sees on it, and the
// certificate the audit line names are one fact.
type tlsTransport struct {
	tr   *http.Transport
	key  tlsKey
	cert *tls.Certificate
	leaf *tlsLeaf
	// refs counts the requests using the transport; retired is set when a
	// newer one took its place. Both are guarded by the route's tlsMu. The
	// last request off a retired transport closes its idle connections, so
	// a connection that was busy at the rotation does not outlive the
	// request that held it.
	refs    int
	retired bool
}

// tlsLeaf is what the audit line says about a presented certificate.
type tlsLeaf struct {
	fingerprint string
	notAfter    string
}

// defaultHTTPTransport is defaultTransport as the type a route clones.
func defaultHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

// chainDigest is the SHA-256 of the chain the handshake presents, leaf first.
// A leaf reissued under another intermediate has another digest, because the
// upstream sees another chain.
func chainDigest(cc *tlspem.ClientCertificate) [32]byte {
	h := sha256.New()
	for _, der := range cc.Cert.Certificate {
		h.Write(der)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// routeTransport returns the transport of rt for the CA bundle and the client
// certificate the request resolved, and counts the request on it. The caller
// ends with rt.release.
//
// A route with inject.tls has a transport of its own for each sandbox. One
// pool shared by every sandbox would keep a connection that one sandbox's
// handshake opened with its certificate, and serve another sandbox's request
// on it: a second sandbox would present an identity it was never given.
//
// The transport is identified by the digest of the CA bundle and the digest of
// the certificate chain. When either differs from the current transport's, a
// new one is built and becomes the current one, and the old one is retired:
// its idle connections close now, a connection in use finishes its request
// and closes after it. A rotation therefore reaches the next request even
// when an HTTP/2 connection to the upstream is open.
//
// It is cloned from the base transport, so the dial and timeout rules of the
// broker hold, and the base has no proxy unless the caller gave it one. The
// root CAs are the route's ca alone when the route has one. A ca replaces the
// system roots: a route that names a private CA must refuse an upstream that
// a public CA vouches for, or a hostile name in DNS would get the
// certificate.
func (h *Handler) routeTransport(rt *route, pool *x509.CertPool, caSum [32]byte, cc *tlspem.ClientCertificate) (*tlsTransport, error) {
	key := tlsKey{ca: caSum}
	if cc != nil {
		key.cert = chainDigest(cc)
	}
	rt.tlsMu.Lock()
	defer rt.tlsMu.Unlock()
	// A request that looked the route up before its sandbox was removed
	// arrives here after closeTransports ran. A transport built now would be
	// one that nobody closes.
	if rt.closed {
		return nil, errRouteClosed
	}
	if cur := rt.cur; cur != nil && cur.key == key {
		cur.refs++
		return cur, nil
	}
	tt := &tlsTransport{key: key, refs: 1}
	var tr *http.Transport
	if base, ok := h.transport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = defaultHTTPTransport()
	}
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	} else {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	cfg := tr.TLSClientConfig
	// Nothing of one handshake may help another: a session cache would resume
	// a session of another sandbox or another certificate, and a key log
	// would write the secrets of this route's connections where the base
	// sends its own. The base verifies; a route does too.
	cfg.ClientSessionCache = nil
	cfg.KeyLogWriter = nil
	cfg.InsecureSkipVerify = false
	cfg.Certificates = nil
	cfg.GetClientCertificate = nil
	if pool != nil {
		cfg.RootCAs = pool
	}
	if cc != nil {
		cert := cc.Cert
		tt.cert = &cert
		tt.leaf = &tlsLeaf{fingerprint: tlspem.Fingerprint(cc.Leaf), notAfter: cc.Leaf.NotAfter.UTC().Format(time.RFC3339)}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return tt.cert, nil
		}
	}
	tt.tr = tr
	if old := rt.cur; old != nil {
		old.retire()
	}
	rt.cur = tt
	return tt, nil
}

// retire is called with the route's tlsMu held.
func (tt *tlsTransport) retire() {
	tt.retired = true
	if tt.refs == 0 {
		tt.tr.CloseIdleConnections()
	}
}

// release ends one request's use of tt.
func (rt *route) release(tt *tlsTransport) {
	rt.tlsMu.Lock()
	defer rt.tlsMu.Unlock()
	tt.refs--
	if tt.retired && tt.refs == 0 {
		tt.tr.CloseIdleConnections()
	}
}

// clientCertificate resolves the secret name and parses it, for one request.
// Like a header credential it is read again for every request, so a rotation
// reaches the next one; the parse is cached by the bytes. A rotation does not
// close anything here: the next request that resolves the new bytes builds a
// transport for them (see routeTransport).
func (h *Handler) clientCertificate(ctx context.Context, name string, wl secret.Workload) (*tlspem.ClientCertificate, error) {
	data, err := h.creds.Resolve(ctx, name, wl)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	now := h.now()
	h.certMu.Lock()
	e := h.certs[name]
	h.certMu.Unlock()
	if e != nil && e.sum == sum {
		// A cached certificate still expires.
		if now.After(e.cert.Leaf.NotAfter) {
			return nil, fmt.Errorf("%w on %s", tlspem.ErrExpired, e.cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		return e.cert, nil
	}
	cc, err := tlspem.ParseClientCertificate(data, now)
	if err != nil {
		return nil, err
	}
	h.cacheCert(name, &certEntry{sum: sum, cert: &cc})
	return &cc, nil
}

// caPool resolves the secret name and parses it as a CA bundle, cached by its
// bytes. The digest says when a route's transport is out of date.
func (h *Handler) caPool(ctx context.Context, name string, wl secret.Workload) (*x509.CertPool, [32]byte, error) {
	data, err := h.creds.Resolve(ctx, name, wl)
	if err != nil {
		return nil, [32]byte{}, err
	}
	sum := sha256.Sum256(data)
	h.certMu.Lock()
	e := h.cas[name]
	h.certMu.Unlock()
	if e != nil && e.sum == sum {
		return e.pool, sum, nil
	}
	pool, _, err := tlspem.ParseCA(data)
	if err != nil {
		return nil, [32]byte{}, err
	}
	h.cacheCA(name, &caEntry{sum: sum, pool: pool})
	return pool, sum, nil
}

// named reports which secrets the registered routes name as a client
// certificate and as a CA. The caller holds h.mu.
func (h *Handler) named() (certs, cas map[string]bool) {
	certs, cas = map[string]bool{}, map[string]bool{}
	for _, sb := range h.sandboxes {
		for _, rt := range sb.routes {
			in := rt.spec.Inject
			if in == nil || in.TLS == nil {
				continue
			}
			if c := in.TLS.ClientCertificate; c != nil {
				certs[c.SecretRef.Name] = true
			}
			if c := in.TLS.CA; c != nil {
				cas[c.SecretRef.Name] = true
			}
		}
	}
	return certs, cas
}

// cacheCert keeps a parsed client certificate, with its private key, while a
// registered route names the secret. A request of a sandbox that was removed
// meanwhile must not put one back. The lock order is h.mu, then certMu.
func (h *Handler) cacheCert(name string, e *certEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if certs, _ := h.named(); !certs[name] {
		return
	}
	h.certMu.Lock()
	h.certs[name] = e
	h.certMu.Unlock()
}

func (h *Handler) cacheCA(name string, e *caEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if _, cas := h.named(); !cas[name] {
		return
	}
	h.certMu.Lock()
	h.cas[name] = e
	h.certMu.Unlock()
}

// pruneCerts drops the parsed certificates and CA bundles that no registered
// route names any more. The cache holds private keys, and a secret that the
// last route to name it has left has no reason to stay in memory.
func (h *Handler) pruneCerts() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	certs, cas := h.named()
	h.certMu.Lock()
	defer h.certMu.Unlock()
	for name := range h.certs {
		if !certs[name] {
			delete(h.certs, name)
		}
	}
	for name := range h.cas {
		if !cas[name] {
			delete(h.cas, name)
		}
	}
}

// closeTransports retires the sandbox's route transports and marks the routes
// closed, so a removed or replaced sandbox leaves no connection that holds
// its certificate open, and a request still on its way builds none.
func (sb *sandbox) closeTransports() {
	for _, rt := range sb.routes {
		rt.tlsMu.Lock()
		rt.closed = true
		if rt.cur != nil {
			rt.cur.retire()
			rt.cur = nil
		}
		rt.tlsMu.Unlock()
	}
}

// tlsHandshakeFailed reports whether err is the upstream refusing the TLS
// handshake or the broker refusing the upstream's certificate. With TLS 1.3
// the server's refusal of the client certificate arrives on the first read,
// as a remote alert, and not from the handshake call. The error types were
// checked against what net/http returns: a *net.OpError with Op "remote
// error" for the alert, and a *tls.CertificateVerificationError for an
// upstream the roots do not vouch for.
//
// Two more errors are the same refusal, and both are only that on a new
// connection (newConn: the request did not reuse a kept-alive one). An
// upstream that speaks HTTP/2 lets the alert reach the connection while it is
// being set up, and net/http says only that the client connection could not
// be established. net/http does not export that error, so it is matched by
// its text, and TestHTTP2UpstreamRefusingTheClientCertificateIsAHandshakeFailure
// fails if a Go release changes it. And the upstream closes the connection
// after its alert: a write the broker makes before it reads the alert fails
// with a broken pipe or a reset, and the alert is never read. On a connection
// that served requests before, a reset is not evidence about the handshake,
// and it keeps the general classification.
func tlsHandshakeFailed(err error, newConn bool) bool {
	var (
		cv   *tls.CertificateVerificationError
		ua   x509.UnknownAuthorityError
		ci   x509.CertificateInvalidError
		he   x509.HostnameError
		rh   tls.RecordHeaderError
		oper *net.OpError
	)
	switch {
	case errors.As(err, &cv), errors.As(err, &ua), errors.As(err, &ci), errors.As(err, &he), errors.As(err, &rh):
		return true
	case errors.As(err, &oper) && oper.Op == "remote error":
		return true
	case newConn && strings.Contains(err.Error(), "http2: client conn could not be established"):
		return true
	case newConn && (errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)):
		return true
	}
	return false
}

// certCheck classifies a failure to read the secret of a client certificate.
// A certificate that is not yet valid is a certificate that cannot be used,
// not an expired one.
func certCheck(err error) string {
	if errors.Is(err, tlspem.ErrExpired) {
		return "tls.expired"
	}
	return "tls.certificate"
}

// tlsBody is the answer for a TLS failure. It names the route and the check
// and nothing the host holds.
func tlsBody(route, check string) string {
	var what string
	switch check {
	case "tls.ca":
		what = "the CA of route " + route + " could not be used"
	case "tls.expired":
		what = "the client certificate of route " + route + " has expired"
	case "tls.handshake":
		what = "the TLS handshake with the upstream of route " + route + " failed"
	default:
		what = "the client certificate of route " + route + " could not be used"
	}
	return fmt.Sprintf("brig broker: %s (check %s)", what, check)
}
