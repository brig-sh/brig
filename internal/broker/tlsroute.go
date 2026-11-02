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

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/tlspem"
)

// certKey is the type of certCtxKey.
type certKey struct{}

// certCtxKey carries the client certificate of a request from the round trip
// to the handshake. The transport of a route is shared by its requests, so
// the certificate cannot be a field of its tls.Config; GetClientCertificate
// reads it from the context of the request that dialed.
var certCtxKey = certKey{}

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

// defaultHTTPTransport is defaultTransport as the type a route clones.
func defaultHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

// routeTransport returns the transport of rt, built on first use and again
// when the CA bundle changes.
//
// A route with inject.tls has a transport of its own for each sandbox. One
// pool shared by every sandbox would keep a connection that one sandbox's
// handshake opened with its certificate, and serve another sandbox's request
// on it: a second sandbox would present an identity it was never given.
//
// It is cloned from the base transport, so the dial and timeout rules of the
// broker hold, and the base has no proxy unless the caller gave it one. The
// root CAs are the route's ca alone when the route has one. A ca replaces the
// system roots: a route that names a private CA must refuse an upstream that
// a public CA vouches for, or a hostile name in DNS would get the
// certificate. GetClientCertificate hands over the certificate that the
// request brought in its context, so a rotated certificate reaches the next
// connection without a rebuild.
func (h *Handler) routeTransport(rt *route, pool *x509.CertPool, caSum [32]byte) *http.Transport {
	rt.tlsMu.Lock()
	defer rt.tlsMu.Unlock()
	if rt.tr != nil && rt.caSum == caSum {
		return rt.tr
	}
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
	if pool != nil {
		tr.TLSClientConfig.RootCAs = pool
	}
	if in := rt.spec.Inject; in != nil && in.TLS != nil && in.TLS.ClientCertificate != nil {
		tr.TLSClientConfig.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if c, ok := cri.Context().Value(certCtxKey).(*tls.Certificate); ok && c != nil {
				return c, nil
			}
			return nil, &tlsFailure{check: "tls.certificate"}
		}
	}
	old := rt.tr
	rt.tr, rt.caSum = tr, caSum
	if old != nil {
		old.CloseIdleConnections()
	}
	return tr
}

// clientCertificate resolves the secret name and parses it, for one request.
// Like a header credential it is read again for every request, so a rotation
// reaches the next one; the parse is cached by the bytes. When the bytes
// change, idle connections of every route that presents the secret are
// closed, so the next connection carries the new certificate.
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
			return nil, tlspem.ErrExpired
		}
		return e.cert, nil
	}
	cc, err := tlspem.ParseClientCertificate(data, now)
	if err != nil {
		return nil, err
	}
	h.certMu.Lock()
	h.certs[name] = &certEntry{sum: sum, cert: &cc}
	h.certMu.Unlock()
	h.closeIdleFor(name)
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
	h.certMu.Lock()
	h.cas[name] = &caEntry{sum: sum, pool: pool}
	h.certMu.Unlock()
	return pool, sum, nil
}

// closeIdleFor closes the idle connections of every registered route whose
// clientCertificate is the secret name.
func (h *Handler) closeIdleFor(name string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sb := range h.sandboxes {
		for _, rt := range sb.routes {
			in := rt.spec.Inject
			if in == nil || in.TLS == nil || in.TLS.ClientCertificate == nil ||
				in.TLS.ClientCertificate.SecretRef.Name != name {
				continue
			}
			rt.tlsMu.Lock()
			if rt.tr != nil {
				rt.tr.CloseIdleConnections()
			}
			rt.tlsMu.Unlock()
		}
	}
}

// closeTransports closes the idle connections of the sandbox's route
// transports, so a removed or replaced sandbox leaves no connection that
// holds its certificate open.
func (sb *sandbox) closeTransports() {
	for _, rt := range sb.routes {
		rt.tlsMu.Lock()
		if rt.tr != nil {
			rt.tr.CloseIdleConnections()
			rt.tr = nil
		}
		rt.tlsMu.Unlock()
	}
}

// tlsHandshakeFailed reports whether err is the upstream refusing the TLS
// handshake or the broker refusing the upstream's certificate. With TLS 1.3
// the server's refusal of the client certificate arrives on the first read,
// as a remote alert, and not from the handshake call.
func tlsHandshakeFailed(err error) bool {
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
	}
	return strings.Contains(err.Error(), "tls: ")
}

// certCheck classifies a failure to read the secret of a client certificate.
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
