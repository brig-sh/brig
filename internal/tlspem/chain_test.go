package tlspem

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/testpki"
)

func TestParseClientCertificateBuildsTheChainInAnyOrder(t *testing.T) {
	root := mustCA(t)
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := inter.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	bag := []byte("Bag Attributes\n    friendlyName: client\n")

	orders := map[string][]byte{
		"leaf inter key root":    cat(leaf, inter.PEM, key, root.PEM),
		"root leaf inter key":    cat(root.PEM, leaf, inter.PEM, key),
		"key root inter leaf":    cat(key, root.PEM, inter.PEM, leaf),
		"inter leaf key":         cat(inter.PEM, leaf, key),
		"bag attributes between": cat(bag, root.PEM, bag, leaf, bag, inter.PEM, bag, key),
	}
	for name, value := range orders {
		t.Run(name, func(t *testing.T) {
			cc, err := ParseClientCertificate(value, now)
			if err != nil {
				t.Fatal(err)
			}
			if cc.Leaf.Subject.CommonName != "inventory-client" {
				t.Errorf("leaf %q", cc.Leaf.Subject.CommonName)
			}
			// The chain is the leaf then the intermediate, without the root.
			if cc.Chain() != 2 || !bytes.Equal(cc.Cert.Certificate[0], cc.Leaf.Raw) ||
				!bytes.Equal(cc.Cert.Certificate[1], inter.Cert.Raw) {
				t.Errorf("chain of %d does not read leaf, intermediate", cc.Chain())
			}
			if want := strings.Count(string(value), "BEGIN CERTIFICATE") - 2; cc.NotSent != want {
				t.Errorf("NotSent %d, want %d", cc.NotSent, want)
			}
			if len(cc.Unsent) != cc.NotSent || (cc.NotSent == 1 && cc.Unsent[0] != root.Cert.Subject.String()) {
				t.Errorf("Unsent %v, want the root alone or nothing", cc.Unsent)
			}
		})
	}
}

func TestParseClientCertificateLeavesOutACertificateOffTheChain(t *testing.T) {
	ca := mustCA(t)
	stray := mustCA(t)
	now := time.Now()
	leaf, key, err := ca.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := ParseClientCertificate(cat(stray.PEM, leaf, key), now)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Chain() != 1 || cc.NotSent != 1 {
		t.Errorf("chain %d, not sent %d", cc.Chain(), cc.NotSent)
	}
}

func TestParseClientCertificateRefusesTwoCertificatesThatMatchTheKey(t *testing.T) {
	ca := mustCA(t)
	now := time.Now()
	leaf, key, err := ca.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A second, different certificate for the same key is not the first one
	// again.
	again, err := ca.Reissue(key, "c", now.Add(-time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseClientCertificate(cat(leaf, again, key), now)
	if !errors.Is(err, ErrManyLeaves) {
		t.Fatalf("got %v, want %v", err, ErrManyLeaves)
	}
	checkNoKey(t, err.Error(), key)
}

// A fullchain.pem that repeats its leaf holds one certificate.
func TestParseClientCertificateCountsTheSameCertificateOnce(t *testing.T) {
	root := mustCA(t)
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := inter.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := ParseClientCertificate(cat(leaf, inter.PEM, leaf, key, inter.PEM, root.PEM), now)
	if err != nil {
		t.Fatalf("a repeated leaf: %v", err)
	}
	// The leaf and the intermediate go out; the root is the one not sent.
	if cc.Chain() != 2 || cc.NotSent != 1 || len(cc.Unsent) != 1 {
		t.Errorf("chain %d, not sent %d %v", cc.Chain(), cc.NotSent, cc.Unsent)
	}
}

// A CA that re-keyed under one name leaves two intermediates with one subject.
// The one that signed the leaf is the one that goes out, in either order.
func TestParseClientCertificateTakesTheIntermediateThatSignedTheLeaf(t *testing.T) {
	root := mustCA(t)
	oldInter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	newInter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := newInter.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"old first": cat(leaf, key, oldInter.PEM, newInter.PEM),
		"new first": cat(leaf, key, newInter.PEM, oldInter.PEM),
	} {
		t.Run(name, func(t *testing.T) {
			cc, err := ParseClientCertificate(value, now)
			if err != nil {
				t.Fatal(err)
			}
			if cc.Chain() != 2 || !bytes.Equal(cc.Cert.Certificate[1], newInter.Cert.Raw) {
				t.Errorf("the chain does not end in the intermediate that signed the leaf")
			}
			if len(cc.Unsent) != 1 || cc.Unsent[0] != oldInter.Cert.Subject.String() {
				t.Errorf("Unsent %v, want the other intermediate", cc.Unsent)
			}
		})
	}
}

// The self-signed copy of a cross-signed intermediate must not end the chain
// before the copy that a root signed.
func TestParseClientCertificatePrefersTheCrossSignedCopy(t *testing.T) {
	root := mustCA(t)
	inter, err := testpki.NewCA("cross-inter") // self-signed; its twin has its subject and key
	if err != nil {
		t.Fatal(err)
	}
	cross, err := inter.CrossSign(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := inter.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"self-signed first":  cat(leaf, key, inter.PEM, cross.PEM, root.PEM),
		"cross-signed first": cat(leaf, key, cross.PEM, inter.PEM, root.PEM),
	} {
		t.Run(name, func(t *testing.T) {
			cc, err := ParseClientCertificate(value, now)
			if err != nil {
				t.Fatal(err)
			}
			if cc.Chain() != 2 || !bytes.Equal(cc.Cert.Certificate[1], cross.Cert.Raw) {
				t.Fatalf("chain of %d does not read leaf, cross-signed intermediate", cc.Chain())
			}
			if cc.NotSent != 2 {
				t.Errorf("NotSent %d, want the self-signed copy and the root", cc.NotSent)
			}
		})
	}
	// With nothing but the self-signed copy, it is a root and ends the chain.
	cc, err := ParseClientCertificate(cat(leaf, key, inter.PEM), now)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Chain() != 1 || cc.NotSent != 1 {
		t.Errorf("a self-signed root alone: chain %d, not sent %d", cc.Chain(), cc.NotSent)
	}
}

// A certificate that carries the issuer's name is not the issuer unless it is
// a CA that signed the leaf.
func TestParseClientCertificateNeverSendsANonCAWithTheIssuersName(t *testing.T) {
	root := mustCA(t)
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := inter.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Same subject, different key: it did not sign the leaf.
	namesake, _, err := root.Client("inter", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Same subject and same key, signed by the root, but not a CA.
	impostor, err := inter.NonCA(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"namesake alone":                 cat(leaf, key, namesake),
		"impostor alone":                 cat(leaf, key, impostor),
		"impostor before the real one":   cat(impostor, leaf, key, inter.PEM),
		"namesake after the real one":    cat(leaf, key, inter.PEM, namesake),
		"impostor and namesake together": cat(namesake, leaf, key, impostor),
	} {
		t.Run(name, func(t *testing.T) {
			cc, err := ParseClientCertificate(value, now)
			if err != nil {
				t.Fatal(err)
			}
			real := strings.Contains(name, "real one")
			if real && (cc.Chain() != 2 || !bytes.Equal(cc.Cert.Certificate[1], inter.Cert.Raw)) {
				t.Errorf("the real intermediate was not sent")
			}
			if !real && cc.Chain() != 1 {
				t.Errorf("chain of %d: a certificate that is not a CA went out", cc.Chain())
			}
		})
	}
}

func TestParseCAKeepsOnlyTheCertificatesThatAreCAs(t *testing.T) {
	root := mustCA(t)
	now := time.Now()
	leaf, key, err := root.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// A ca that names the client bundle finds the root in it and trusts
	// nothing else in it.
	pool, n, err := ParseCA(cat(root.PEM, leaf, key))
	if err != nil || n != 1 {
		t.Fatalf("bundle: %d, %v", n, err)
	}
	b, _ := pem.Decode(leaf)
	lc, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if _, err := lc.Verify(opts); err != nil {
		t.Errorf("leaf does not verify against the root in its bundle: %v", err)
	}
	// The leaf is not an anchor: a pool of the leaf alone is refused.
	if _, _, err := ParseCA(leaf); !errors.Is(err, ErrNoCA) {
		t.Errorf("a leaf alone: got %v, want %v", err, ErrNoCA)
	}
}
