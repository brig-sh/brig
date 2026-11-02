// Package testpki mints certificates for tests.
//
// It is a test helper like internal/ttytest: nothing outside a test uses it.
// It takes no *testing.T, so a helper process (cmd/brig-broker's) can build
// the same mTLS upstream a unit test does. A key is ECDSA P-256 unless the
// call asks for another kind, and every call draws from crypto/rand, so two
// runs never share a certificate.
package testpki

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CA is a certificate authority: its certificate, its key and the certificate
// as PEM.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	PEM  []byte
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// NewCA mints a self-signed CA good for ten years.
func NewCA(name string) (*CA, error) {
	return newCA(name, nil, nil)
}

func newCA(name string, parent *x509.Certificate, parentKey crypto.Signer) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	signer, signerKey := tmpl, crypto.Signer(key)
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, key.Public(), signerKey)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, PEM: certPEM(der)}, nil
}

// Pool returns a pool that trusts this CA.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.Cert)
	return p
}

// Intermediate mints a CA signed by c.
func (c *CA) Intermediate(name string) (*CA, error) {
	return newCA(name, c.Cert, c.Key)
}

func (c *CA) leaf(cn string, notBefore, notAfter time.Time, usage []x509.ExtKeyUsage, hosts []string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := c.leafFor(key, cn, notBefore, notAfter, usage, hosts)
	if err != nil {
		return nil, nil, err
	}
	return key, der, nil
}

// leafFor signs a leaf for key.
func (c *CA) leafFor(key crypto.Signer, cn string, notBefore, notAfter time.Time, usage []x509.ExtKeyUsage, hosts []string) ([]byte, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return x509.CreateCertificate(rand.Reader, tmpl, c.Cert, key.Public(), c.Key)
}

// KeyKind picks the algorithm of a client key.
type KeyKind int

// The key kinds a client certificate takes.
const (
	ECDSA KeyKind = iota
	RSA
	Ed25519
)

func newKey(kind KeyKind) (crypto.Signer, error) {
	switch kind {
	case RSA:
		return rsa.GenerateKey(rand.Reader, 2048)
	case Ed25519:
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func keyPEM(key crypto.Signer) ([]byte, error) {
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// ClientKind mints a client-auth leaf with a key of the given kind, as Client
// does for ECDSA.
func (c *CA) ClientKind(kind KeyKind, cn string, notBefore, notAfter time.Time) (certPEMOut, keyPEMOut []byte, err error) {
	key, err := newKey(kind)
	if err != nil {
		return nil, nil, err
	}
	der, err := c.leafFor(key, cn, notBefore, notAfter, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return nil, nil, err
	}
	kp, err := keyPEM(key)
	return certPEM(der), kp, err
}

// Reissue mints another client-auth certificate for the key in keyPEMIn, a
// PKCS#8 PEM that Client returned: a second certificate for one key.
func (c *CA) Reissue(keyPEMIn []byte, cn string, notBefore, notAfter time.Time) ([]byte, error) {
	b, _ := pem.Decode(keyPEMIn)
	if b == nil {
		return nil, fmt.Errorf("no PEM block in the key")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("the key cannot sign")
	}
	der, err := c.leafFor(signer, cn, notBefore, notAfter, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	return certPEM(der), err
}

// CrossSign issues c's subject and key again under issuer: the cross-signed
// twin of c. The twin chains to issuer, and c itself stays self-signed.
func (c *CA) CrossSign(issuer *CA) (*CA, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               c.Cert.Subject,
		NotBefore:             c.Cert.NotBefore,
		NotAfter:              c.Cert.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.Cert, c.Key.Public(), issuer.Key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: c.Key, PEM: certPEM(der)}, nil
}

// NonCA issues a certificate with c's subject and key that is not a CA,
// signed by issuer. Its name fits the issuer field of a leaf that c signed,
// and its key even verifies the signature, but a client certificate must not
// chain through it.
func (c *CA) NonCA(issuer *CA) ([]byte, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      c.Cert.Subject,
		NotBefore:    c.Cert.NotBefore,
		NotAfter:     c.Cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.Cert, c.Key.Public(), issuer.Key)
	return certPEM(der), err
}

// X25519Key returns a PKCS#8 PEM key that parses and cannot sign.
func X25519Key() ([]byte, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// RSAPSSKey returns a PKCS#8 PEM key wrapped in the RSA-PSS algorithm
// identifier, which Go's parser does not take.
func RSAPSSKey() ([]byte, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	kb, err := asn1.Marshal(struct {
		Version int
		Algo    pkix.AlgorithmIdentifier
		Key     []byte
	}{0, pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}},
		x509.MarshalPKCS1PrivateKey(k)})
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// Client mints a client-auth leaf. certPEM holds the leaf alone and keyPEM
// its PKCS#8 key; the chain a test stores is certPEM followed by keyPEM.
func (c *CA) Client(cn string, notBefore, notAfter time.Time) (certPEMOut, keyPEM []byte, err error) {
	key, der, err := c.leaf(cn, notBefore, notAfter, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// ClientWithUsage mints a leaf with the extended key usages given, or with no
// extended key usage extension when none is given.
func (c *CA) ClientWithUsage(cn string, notBefore, notAfter time.Time, usage ...x509.ExtKeyUsage) (certPEMOut, keyPEM []byte, err error) {
	key, der, err := c.leaf(cn, notBefore, notAfter, usage, nil)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// Server mints a server-auth certificate for hosts. DNS and IP names are told
// apart by parsing; 127.0.0.1 and ::1 are always added.
func (c *CA) Server(hosts ...string) (tls.Certificate, error) {
	all := append(append([]string{}, hosts...), "127.0.0.1", "::1")
	now := time.Now()
	key, der, err := c.leaf("server", now.Add(-time.Hour), now.AddDate(1, 0, 0), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, all)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
