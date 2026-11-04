package tlspem

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/testpki"
)

func mustCA(t *testing.T) *testpki.CA {
	t.Helper()
	ca, err := testpki.NewCA("test-ca")
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func block(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// signedBy returns a leaf PEM and its key as the given key encodings, signed by
// ca, so the same certificate can be paired with each key format.
func signedBy(t *testing.T, ca *testpki.CA, key any, pub any) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	_ = key
	return block("CERTIFICATE", der)
}

func TestParseClientCertificateAcceptsAChainWithAnIntermediate(t *testing.T) {
	root := mustCA(t)
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := inter.Client("inventory-client", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	cc, err := ParseClientCertificate(cat(certPEM, inter.PEM, keyPEM), now)
	if err != nil {
		t.Fatalf("leaf+intermediate+key: %v", err)
	}
	if cc.Leaf.Subject.CommonName != "inventory-client" || len(cc.Cert.Certificate) != 2 {
		t.Errorf("leaf %q with %d certificates", cc.Leaf.Subject.CommonName, len(cc.Cert.Certificate))
	}
	if _, err := ParseClientCertificate(cat(certPEM, keyPEM), now); err != nil {
		t.Errorf("leaf+key: %v", err)
	}

	// An RSA key in PKCS#1 form and an EC key in SEC1 form, both under the
	// same CA, load as well as the PKCS#8 key above.
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaCert := signedBy(t, root, rk, &rk.PublicKey)
	if _, err := ParseClientCertificate(cat(rsaCert, block("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rk))), now); err != nil {
		t.Errorf("RSA PRIVATE KEY: %v", err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecCert := signedBy(t, root, ek, &ek.PublicKey)
	sec1, err := x509.MarshalECPrivateKey(ek)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseClientCertificate(cat(ecCert, block("EC PRIVATE KEY", sec1)), now); err != nil {
		t.Errorf("EC PRIVATE KEY: %v", err)
	}
}

func TestParseClientCertificateRefusesEachStoreTimeCase(t *testing.T) {
	ca := mustCA(t)
	now := time.Now()
	certPEM, keyPEM, err := ca.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := ca.Client("other", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expCert, expKey, err := ca.Client("old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	futureCert, futureKey, err := ca.Client("soon", now.Add(26*time.Hour), now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey, err := ca.ClientWithUsage("srv", now.Add(-time.Hour), now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	if err != nil {
		t.Fatal(err)
	}
	codeCert, codeKey, err := ca.ClientWithUsage("code", now.Add(-time.Hour), now.Add(time.Hour), x509.ExtKeyUsageCodeSigning, x509.ExtKeyUsageEmailProtection)
	if err != nil {
		t.Fatal(err)
	}
	x25519, err := testpki.X25519Key()
	if err != nil {
		t.Fatal(err)
	}
	pss, err := testpki.RSAPSSKey()
	if err != nil {
		t.Fatal(err)
	}
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00000000000000000000000000000000"},
		Bytes:   x509.MarshalPKCS1PrivateKey(rk),
	}

	cases := []struct {
		name  string
		value []byte
		want  error
	}{
		{"no certificate", keyPEM, ErrNoCertificate},
		{"empty", nil, ErrNoCertificate},
		{"no key", certPEM, ErrNoKey},
		{"two keys", cat(certPEM, keyPEM, otherKey), ErrManyKeys},
		{"pkcs8 encrypted", cat(certPEM, block("ENCRYPTED PRIVATE KEY", []byte("opaque"))), ErrEncryptedKey},
		{"legacy encrypted", cat(certPEM, pem.EncodeToMemory(legacy)), ErrEncryptedKey},
		{"key of another certificate", cat(certPEM, otherKey), ErrKeyMismatch},
		{"expired", cat(expCert, expKey), ErrExpired},
		{"bad certificate der", cat(block("CERTIFICATE", []byte("not der")), keyPEM), ErrNoCertificate},
		{"bad key der", cat(certPEM, block("PRIVATE KEY", []byte("not der"))), ErrBadKey},
		{"not yet valid", cat(futureCert, futureKey), ErrNotYetValid},
		{"serverAuth alone", cat(serverCert, serverKey), ErrNoClientAuth},
		{"neither clientAuth nor any", cat(codeCert, codeKey), ErrNoClientAuth},
		{"openssh key block", cat(certPEM, block("OPENSSH PRIVATE KEY", []byte("opaque"))), ErrUnsupportedKeyBlock},
		{"dsa key block", cat(certPEM, block("DSA PRIVATE KEY", []byte("opaque"))), ErrUnsupportedKeyBlock},
		{"x25519 key", cat(certPEM, x25519), ErrUnsupportedKey},
		{"rsa-pss key", cat(certPEM, pss), ErrUnsupportedKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClientCertificate(tc.value, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			text := err.Error()
			if errors.Is(err, ErrUnsupportedKeyBlock) {
				// The refusal names the type of the block, which ends in
				// "PRIVATE KEY"; that is the label and no key material.
				text = strings.ReplaceAll(text, "PRIVATE KEY", "KEY")
			}
			checkNoKey(t, text, keyPEM, otherKey, expKey)
		})
	}
}

func TestParseClientCertificateSaysWhyInWords(t *testing.T) {
	ca := mustCA(t)
	now := time.Now().UTC().Truncate(time.Second)
	cert, key, err := ca.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	old, oldKey, err := ca.Client("old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	soon, soonKey, err := ca.Client("soon", now.Add(26*time.Hour), now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	x25519, err := testpki.X25519Key()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value []byte
		want  string
	}{
		{"expired", cat(old, oldKey), "the certificate has expired on " + now.Add(-time.Hour).Format(time.RFC3339)},
		{"not yet valid", cat(soon, soonKey), "the certificate is not yet valid: it is valid from " + now.Add(26*time.Hour).Format(time.RFC3339)},
		{"unsupported block", cat(cert, block("OPENSSH PRIVATE KEY", []byte("x"))), `unsupported key block "OPENSSH PRIVATE KEY"`},
		{"unsupported key", cat(cert, x25519), "unsupported private key type"},
		{"damaged key", cat(cert, block("PRIVATE KEY", []byte("x"))), "the private key block does not parse"},
		{"other key", cat(cert, soonKey), "the private key does not match the certificate"},
	} {
		_, err := ParseClientCertificate(tc.value, now)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	_ = key
}

func TestParseClientCertificateAcceptsEachClientAuthUsage(t *testing.T) {
	ca := mustCA(t)
	now := time.Now()
	for name, usage := range map[string][]x509.ExtKeyUsage{
		"no extension":         nil,
		"clientAuth":           {x509.ExtKeyUsageClientAuth},
		"clientAuth and other": {x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		"any":                  {x509.ExtKeyUsageAny},
	} {
		t.Run(name, func(t *testing.T) {
			leaf, key, err := ca.ClientWithUsage("inventory-client", now.Add(-time.Hour), now.Add(time.Hour), usage...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseClientCertificate(cat(leaf, key), now); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseClientCertificateAcceptsEachKeyKind(t *testing.T) {
	root := mustCA(t)
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, kind := range map[string]testpki.KeyKind{"ecdsa": testpki.ECDSA, "rsa": testpki.RSA, "ed25519": testpki.Ed25519} {
		t.Run(name, func(t *testing.T) {
			leaf, key, err := inter.ClientKind(kind, "inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			cc, err := ParseClientCertificate(cat(key, root.PEM, inter.PEM, leaf), now)
			if err != nil {
				t.Fatal(err)
			}
			if cc.Chain() != 2 || !bytes.Equal(cc.Cert.Certificate[1], inter.Cert.Raw) {
				t.Errorf("chain of %d does not read leaf, intermediate", cc.Chain())
			}
		})
	}
}

func TestParseClientCertificateCountsOnlyThreeKeyBlockTypes(t *testing.T) {
	ca := mustCA(t)
	now := time.Now()
	cert, key, err := ca.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A key block of a type the broker does not take is refused by name, also
	// when a usable key sits beside it; it is not counted as the second key.
	_, err = ParseClientCertificate(cat(cert, key, block("OPENSSH PRIVATE KEY", []byte("x"))), now)
	if !errors.Is(err, ErrUnsupportedKeyBlock) {
		t.Fatalf("got %v, want %v", err, ErrUnsupportedKeyBlock)
	}
	// An unrelated block type is ignored.
	if _, err := ParseClientCertificate(cat(cert, key, block("PUBLIC KEY", []byte("x"))), now); err != nil {
		t.Fatalf("a PUBLIC KEY block: %v", err)
	}
}

func TestParseCARefusesAValueWithNoCertificate(t *testing.T) {
	ca := mustCA(t)
	other := mustCA(t)
	_, keyPEM, err := ca.Client("c", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string][]byte{
		"one":      ca.PEM,
		"several":  cat(ca.PEM, other.PEM),
		"with key": cat(ca.PEM, keyPEM),
	} {
		pool, n, err := ParseCA(value)
		if err != nil || pool == nil || n < 1 {
			t.Errorf("%s: %v, %d", name, err, n)
		}
	}
	if _, n, _ := ParseCA(cat(ca.PEM, other.PEM)); n != 2 {
		t.Errorf("several: counted %d", n)
	}

	cases := []struct {
		name  string
		value []byte
		want  error
	}{
		{"empty", nil, ErrNoCertificate},
		{"only a key", keyPEM, ErrNoCertificate},
		{"bad der", cat(ca.PEM, block("CERTIFICATE", []byte("not der"))), ErrBadCertificate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseCA(tc.value)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			checkNoKey(t, err.Error(), keyPEM)
		})
	}
}

func TestFingerprintAndSummaryNeverCarryTheKey(t *testing.T) {
	ca := mustCA(t)
	now := time.Now().Truncate(time.Second)
	certPEM, keyPEM, err := ca.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := ParseClientCertificate(cat(certPEM, keyPEM), now)
	if err != nil {
		t.Fatal(err)
	}

	fp := Fingerprint(cc.Leaf)
	if !regexp.MustCompile(`^sha256:[0-9a-f]{16}$`).MatchString(fp) {
		t.Errorf("fingerprint %q", fp)
	}
	sum := Summary(cc.Leaf)
	for _, want := range []string{
		"subject CN=inventory-client",
		"issuer CN=test-ca",
		"expires " + now.Add(time.Hour).UTC().Format(time.RFC3339),
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary %q lacks %q", sum, want)
		}
	}
	checkNoKey(t, fp+"\n"+sum, keyPEM)
}

// checkNoKey fails when text carries a PEM header, a key's body or any of its
// base64 lines.
func checkNoKey(t *testing.T, text string, keys ...[]byte) {
	t.Helper()
	if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, "-----") {
		t.Errorf("text carries a PEM marker: %q", text)
	}
	for _, k := range keys {
		b, _ := pem.Decode(k)
		if b == nil {
			continue
		}
		if strings.Contains(text, base64.StdEncoding.EncodeToString(b.Bytes)) {
			t.Errorf("text carries the key body: %q", text)
		}
		for _, line := range strings.Split(string(k), "\n") {
			if len(line) >= 16 && !strings.HasPrefix(line, "-----") && strings.Contains(text, line) {
				t.Errorf("text carries a key line: %q", text)
			}
		}
	}
}
