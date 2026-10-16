package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/injector"
)

// hostKeySize is the length of the host key, the output size of SHA-256.
const hostKeySize = 32

// loadOrCreateHostKey reads the host key at path, and draws one at first
// start. The key signs every session token, so the file is 0600, is never a
// link, and is read by this process alone. It never goes on a command line
// or into a log.
func loadOrCreateHostKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, hostKeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := writeNew(path, key); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the host key %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := checkPrivate(f, path); err != nil {
		return nil, err
	}
	key, err := io.ReadAll(io.LimitReader(f, hostKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(key) != hostKeySize {
		return nil, fmt.Errorf("refusing the host key %s: it is %d bytes, not %d. Remove it, and the "+
			"next start draws a new one", path, len(key), hostKeySize)
	}
	return key, nil
}

// writeNew creates path 0600 with data, and fails when it exists.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// The lifetimes of the brig CA and of the brig.internal certificate it
// signs. The leaf is drawn at every start and its key is never written. A
// broker that runs for longer signs a new one leafRenew before the old one
// expires.
const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 30 * 24 * time.Hour
	leafRenew    = 7 * 24 * time.Hour
)

// authority is the brig CA of this host. It signs brig.internal and nothing
// else: its name constraints permit that one DNS name and exclude every IP
// address, so a guest that trusts it trusts no other site.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu sync.Mutex
	// leaf is the brig.internal certificate, signed at start and again
	// before it expires.
	leaf tls.Certificate
	// leafEnd is when leaf expires.
	leafEnd time.Time
}

// loadOrCreateCA reads the CA in dir, creates it at first start, and signs
// the brig.internal certificate. A key without its certificate, or a
// certificate without its key, is a start that stopped halfway, and both are
// made again.
func loadOrCreateCA(dir string) (*authority, error) {
	certPath := filepath.Join(dir, injector.CACertName)
	keyPath := filepath.Join(dir, injector.CAKeyName)
	a, err := readCA(certPath, keyPath)
	if errors.Is(err, os.ErrNotExist) {
		a, err = createCA(certPath, keyPath)
	}
	if err != nil {
		return nil, err
	}
	if _, err := a.current(time.Now()); err != nil {
		return nil, err
	}
	return a, nil
}

// current returns the brig.internal certificate, signing a new one when the
// one held expires within leafRenew.
func (a *authority) current(now time.Time) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaf.Certificate == nil || now.Add(leafRenew).After(a.leafEnd) {
		leaf, err := a.sign(injector.BrigInternal, now)
		if err != nil {
			return nil, err
		}
		a.leaf, a.leafEnd = leaf, now.Add(leafLifetime)
	}
	leaf := a.leaf
	return &leaf, nil
}

// certPEM is the CA certificate in PEM, what a guest trusts brig.internal by.
// It holds no key.
func (a *authority) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

func readCA(certPath, keyPath string) (*authority, error) {
	kf, err := os.OpenFile(keyPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = kf.Close() }()
	if err := checkPrivate(kf, keyPath); err != nil {
		return nil, err
	}
	keyPEM, err := io.ReadAll(io.LimitReader(kf, 16<<10))
	if err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	cb, _ := pem.Decode(certPEM)
	if kb == nil || cb == nil {
		return nil, fmt.Errorf("refusing the brig CA in %s: it is not PEM. Remove %s and %s, and the next "+
			"start creates a new one", filepath.Dir(certPath), certPath, keyPath)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA || !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 ||
		cert.PermittedDNSDomains[0] != injector.BrigInternal {
		return nil, fmt.Errorf("refusing the brig CA %s: it is not constrained to %s", certPath, injector.BrigInternal)
	}
	// A pair from two starts, one stopped between its two writes, would
	// otherwise fail every start with an x509 error that names neither file.
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("refusing the brig CA in %s: %s is not the key of %s. Remove %s and %s, and "+
			"the next start creates a new one", filepath.Dir(certPath), keyPath, certPath, certPath, keyPath)
	}
	return &authority{cert: cert, key: key}, nil
}

func createCA(certPath, keyPath string) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "brig CA for " + injector.BrigInternal},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// The constraints are what keep this CA from vouching for an
		// upstream: a guest that trusts it trusts brig.internal and no other
		// name, and no address.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{injector.BrigInternal},
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	// The certificate first: a key is read only beside its certificate.
	if err := writeReplace(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	if err := writeReplace(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key}, nil
}

// sign issues a server certificate for name, with a key that stays in
// memory.
func (a *authority) sign(name string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}, nil
}

// serverConfig is the TLS configuration that serves brig.internal on the
// divert sockets. The route server reads HTTP/1.1 from the connections it is
// handed, so that is the one protocol offered.
func (a *authority) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.current(time.Now())
		},
	}
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}
