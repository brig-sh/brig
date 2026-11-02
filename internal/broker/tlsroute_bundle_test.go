package broker

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"testing"
	"time"
)

// A PKI whose root signs both the client certificate and the upstream's
// hands out one bundle. ca.secretRef may name the secret that holds the
// client certificate: the roots are then the CA certificates in it, and the
// private key in it is not read for that. The upstream's certificate is
// signed by the same root as the client's.
func TestRouteCAMayNameTheClientCertificateSecret(t *testing.T) {
	up := newMTLSUpstream(t, pki().server, pki().ca.Pool(), nil)
	// The base trusts nothing, so only the ca can make the handshake pass.
	base := up.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.RootCAs = x509.NewCertPool()
	r := newRig(t, up, []Route{
		certRoute(t, "samebundle", up.URL, "inventory-client", "inventory-client", false),
		certRoute(t, "badkeyinca", up.URL, "inventory-client", "ca-with-unreadable-key", false),
		certRoute(t, "leafasca", up.URL, "inventory-client", "leaf-and-key", false),
	}, withTransport(base))

	leafAndKey := clientChain(t, pki().ca, "inventory-client", rigStart.Add(-time.Hour), rigStart.AddDate(1, 0, 0))
	// The key in the CA secret is one the client reader would refuse, which
	// shows the CA reader never looks at it.
	encrypted := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("opaque")})
	r.creds.set("inventory-client", bytes.Join([][]byte{leafAndKey, pki().ca.PEM}, nil))
	r.creds.set("ca-with-unreadable-key", bytes.Join([][]byte{pki().ca.PEM, encrypted}, nil))
	r.creds.set("leaf-and-key", leafAndKey)

	resp, _ := r.get("brig-a", "samebundle")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).peer; got != "inventory-client" {
		t.Fatalf("the upstream saw the client %q, want inventory-client", got)
	}

	resp, _ = r.get("brig-a", "badkeyinca")
	wantStatus(t, resp, http.StatusOK)

	// Negative: a bundle with a leaf and a key and no CA gives the route no
	// roots, so it is refused as a CA before the dial. The leaf is not a
	// trust anchor.
	n := up.conns()
	resp, _ = r.get("brig-a", "leafasca")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=tls.ca")
	if got := up.conns(); got != n {
		t.Fatalf("the broker opened %d connections to an upstream with a CA bundle that holds no CA", got-n)
	}
}
