// Package tlspem checks the PEM values that certificate secrets hold.
//
// A client certificate secret is one PEM value: the leaf, any intermediates,
// and the private key, in any order. The checks run when the secret is stored,
// so a value the broker could not use is refused at "brig secret create" and
// not at the first request. The package imports only the standard library and
// no error or summary it returns carries key material, so a caller can print
// either.
package tlspem

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The reasons a value is refused. Errors wrap one of these and carry no bytes
// of the value.
var (
	ErrNoCertificate       = errors.New("no usable CERTIFICATE block")
	ErrBadCertificate      = errors.New("a CERTIFICATE block does not parse")
	ErrNoCA                = errors.New("no CERTIFICATE block is a CA (basic constraints CA:TRUE)")
	ErrNoKey               = errors.New("no private key block")
	ErrManyKeys            = errors.New("more than one private key block")
	ErrEncryptedKey        = errors.New("the private key is encrypted")
	ErrUnsupportedKeyBlock = errors.New("unsupported key block")
	ErrUnsupportedKey      = errors.New("unsupported private key type")
	ErrBadKey              = errors.New("the private key block does not parse")
	ErrKeyMismatch         = errors.New("the private key does not match the certificate")
	ErrManyLeaves          = errors.New("more than one certificate matches the private key")
	ErrExpired             = errors.New("the certificate has expired")
	ErrNotYetValid         = errors.New("the certificate is not yet valid")
	ErrNoClientAuth        = errors.New("the certificate's extended key usage does not allow client authentication")
)

// ClientCertificate is a parsed client certificate: the pair to hand to a TLS
// handshake and its leaf for the expiry and the audit fingerprint. Cert holds
// the chain that goes out in the handshake, leaf first.
type ClientCertificate struct {
	Cert tls.Certificate
	Leaf *x509.Certificate
	// NotSent counts the certificates in the value that stay out of the
	// handshake: a self-signed root and anything that does not continue the
	// chain. Unsent names them by subject, in the order the value holds them,
	// so the person storing the value sees what the broker will leave out.
	NotSent int
	Unsent  []string
}

// Chain is the number of certificates the handshake carries, the leaf
// included.
func (c ClientCertificate) Chain() int { return len(c.Cert.Certificate) }

// ParseClientCertificate reads a stored value in any block order. The leaf is
// the one certificate whose public key matches the one unencrypted private key
// (PKCS#8, PKCS#1 or SEC1); a certificate that appears twice in the value,
// byte for byte, counts once. The chain is built from the leaf up: each next
// certificate is one that signed the one before and is a CA, and the chain
// stops before a self-signed root. The leaf must be valid at now, and an
// extended key usage on it must allow client authentication.
func ParseClientCertificate(data []byte, now time.Time) (ClientCertificate, error) {
	var certs []*x509.Certificate
	seen := map[string]bool{}
	var keyDER []byte
	keys := 0
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch {
		case b.Type == "ENCRYPTED PRIVATE KEY" || b.Headers["Proc-Type"] == "4,ENCRYPTED":
			return ClientCertificate{}, ErrEncryptedKey
		case b.Type == "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return ClientCertificate{}, fmt.Errorf("%w: a certificate block does not parse", ErrNoCertificate)
			}
			// The same certificate twice, as a fullchain.pem that repeats its
			// leaf, is one certificate.
			if !seen[string(c.Raw)] {
				seen[string(c.Raw)] = true
				certs = append(certs, c)
			}
		case b.Type == "PRIVATE KEY" || b.Type == "RSA PRIVATE KEY" || b.Type == "EC PRIVATE KEY":
			keys++
			keyDER = b.Bytes
		case strings.HasSuffix(b.Type, " PRIVATE KEY"):
			// OPENSSH, DSA and the rest: a key the broker cannot sign with.
			// Reading it as no key would report "no private key block" for a
			// file that has one.
			return ClientCertificate{}, fmt.Errorf("%w %q", ErrUnsupportedKeyBlock, b.Type)
		}
	}
	switch {
	case len(certs) == 0:
		return ClientCertificate{}, ErrNoCertificate
	case keys == 0:
		return ClientCertificate{}, ErrNoKey
	case keys > 1:
		return ClientCertificate{}, ErrManyKeys
	}
	signer, err := parseKey(keyDER)
	if err != nil {
		return ClientCertificate{}, err
	}

	var leaf *x509.Certificate
	for _, c := range certs {
		eq, ok := c.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
		if !ok || !eq.Equal(signer.Public()) {
			continue
		}
		if leaf != nil {
			return ClientCertificate{}, ErrManyLeaves
		}
		leaf = c
	}
	if leaf == nil {
		return ClientCertificate{}, ErrKeyMismatch
	}
	if now.After(leaf.NotAfter) {
		return ClientCertificate{}, fmt.Errorf("%w on %s", ErrExpired, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(leaf.NotBefore) {
		return ClientCertificate{}, fmt.Errorf("%w: it is valid from %s", ErrNotYetValid,
			leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !allowsClientAuth(leaf) {
		return ClientCertificate{}, ErrNoClientAuth
	}

	chain := []*x509.Certificate{leaf}
	for cur := leaf; !selfSigned(cur); {
		next := issuerOf(cur, certs, chain)
		// A root is the upstream's to hold, so it ends the chain unsent.
		if next == nil || selfSigned(next) {
			break
		}
		chain = append(chain, next)
		cur = next
	}
	der := make([][]byte, len(chain))
	for i, c := range chain {
		der[i] = c.Raw
	}
	var unsent []string
	for _, c := range certs {
		if !inChain(chain, c) {
			unsent = append(unsent, c.Subject.String())
		}
	}
	return ClientCertificate{
		Cert:    tls.Certificate{Certificate: der, PrivateKey: signer, Leaf: leaf},
		Leaf:    leaf,
		NotSent: len(certs) - len(chain),
		Unsent:  unsent,
	}, nil
}

// issuerOf picks the certificate that continues the chain above cur, or nil.
// A name alone does not say who signed cur: a CA that was re-keyed leaves two
// certificates with one subject, and a cross-signed intermediate has a
// self-signed twin. So a candidate must verify cur's signature and be a CA
// (basic constraints valid). Among those, a certificate that is not
// self-signed is preferred, so that a self-signed copy of an intermediate
// does not end the chain short of the copy that a root signed. A matching
// AuthorityKeyId and SubjectKeyId breaks the tie; after that, the order in
// the value does.
func issuerOf(cur *x509.Certificate, certs, chain []*x509.Certificate) *x509.Certificate {
	var best *x509.Certificate
	rank := -1
	for _, c := range certs {
		if inChain(chain, c) || !c.BasicConstraintsValid || !c.IsCA || cur.CheckSignatureFrom(c) != nil {
			continue
		}
		r := 0
		if !selfSigned(c) {
			r += 2
		}
		if len(cur.AuthorityKeyId) > 0 && bytes.Equal(cur.AuthorityKeyId, c.SubjectKeyId) {
			r++
		}
		if r > rank {
			best, rank = c, r
		}
	}
	return best
}

// allowsClientAuth reports whether c may authenticate a TLS client. A
// certificate with no extended key usage may; one with the extension needs
// clientAuth or anyExtendedKeyUsage in it. An upstream that verifies the
// client refuses a certificate for serverAuth alone at the handshake.
func allowsClientAuth(c *x509.Certificate) bool {
	if len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 {
		return true
	}
	for _, u := range c.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func parseKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		// X25519 parses as PKCS#8 and cannot sign.
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
		return nil, ErrUnsupportedKey
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	// A well-formed PKCS#8 wrapper around an algorithm Go does not take
	// (RSA-PSS, X448) is a key of a type the broker does not support, not a
	// damaged one.
	var p struct {
		Version int
		Algo    pkix.AlgorithmIdentifier
		Key     []byte
	}
	if rest, err := asn1.Unmarshal(der, &p); err == nil && len(rest) == 0 &&
		!p.Algo.Algorithm.Equal(oidRSA) && !p.Algo.Algorithm.Equal(oidEC) && !p.Algo.Algorithm.Equal(oidEd25519) {
		return nil, ErrUnsupportedKey
	}
	return nil, ErrBadKey
}

var (
	oidRSA     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidEC      = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidEd25519 = asn1.ObjectIdentifier{1, 3, 101, 112}
)

// selfSigned reports whether c is signed by its own key. A subject equal to
// the issuer is not enough: a CA that re-keys under its own name issues a
// certificate to its new key that the old key signed.
func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawSubject, c.RawIssuer) &&
		c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

func inChain(chain []*x509.Certificate, c *x509.Certificate) bool {
	for _, x := range chain {
		if x == c {
			return true
		}
	}
	return false
}

// ParseCA checks a CA bundle: every CERTIFICATE block parses and at least one
// is a CA (basic constraints CA:TRUE). Only those enter the pool, so a bundle
// that also holds a client certificate does not make it a trust anchor. It
// returns the pool and the number of CA certificates.
func ParseCA(data []byte) (*x509.CertPool, int, error) {
	pool := x509.NewCertPool()
	blocks, n := 0, 0
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		blocks++
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, 0, fmt.Errorf("%w (block %d)", ErrBadCertificate, blocks)
		}
		if !c.BasicConstraintsValid || !c.IsCA {
			continue
		}
		pool.AddCert(c)
		n++
	}
	switch {
	case blocks == 0:
		return nil, 0, ErrNoCertificate
	case n == 0:
		return nil, 0, ErrNoCA
	}
	return pool, n, nil
}

// Fingerprint names a certificate in the audit log: "sha256:" and the first 16
// hex digits of the digest of its DER.
func Fingerprint(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// Summary describes a certificate for the person storing it.
func Summary(leaf *x509.Certificate) string {
	return fmt.Sprintf("subject %s, issuer %s, expires %s",
		leaf.Subject.String(), leaf.Issuer.String(), leaf.NotAfter.UTC().Format(time.RFC3339))
}
