package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/testpki"
)

// The same leaf, intermediate, root and key are accepted however a PKI hands
// them out: one bundle, separate files given as several -f, the key first or
// last, the root before the leaf, and the Bag Attributes text that openssl
// pkcs12 writes between the blocks. Each stores what cat of the same files
// gives (with a line break after a file that lacks one), and the check reads
// the chain as the leaf then the intermediate, with the root named as not
// sent.
func TestSecretCreateTypeTLSAcceptsEveryLayout(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.Intermediate("inter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := inter.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	bag := []byte("Bag Attributes\n    friendlyName: inventory-client\nsubject=CN=inventory-client\n")
	noEOL := func(b []byte) []byte { return bytes.TrimSuffix(b, []byte("\n")) }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	cases := []struct {
		name  string
		files [][]byte
		// want is what cat gives, with a break after a file that lacks one.
		want []byte
		// root says whether the value holds the root.
		root bool
	}{
		{"one bundle", [][]byte{join(leaf, inter.PEM, key, root.PEM)}, join(leaf, inter.PEM, key, root.PEM), true},
		{"separate files", [][]byte{leaf, inter.PEM, key, root.PEM}, join(leaf, inter.PEM, key, root.PEM), true},
		{"key first", [][]byte{key, leaf, inter.PEM}, join(key, leaf, inter.PEM), false},
		{"key last", [][]byte{leaf, inter.PEM, key}, join(leaf, inter.PEM, key), false},
		{"root before leaf", [][]byte{root.PEM, leaf, inter.PEM, key}, join(root.PEM, leaf, inter.PEM, key), true},
		{"bag attributes between blocks", [][]byte{join(bag, root.PEM, bag, leaf, bag, inter.PEM, bag, key)},
			join(bag, root.PEM, bag, leaf, bag, inter.PEM, bag, key), true},
		{"no final newline on any file", [][]byte{noEOL(leaf), noEOL(inter.PEM), noEOL(key), noEOL(root.PEM)},
			join(noEOL(leaf), []byte("\n"), noEOL(inter.PEM), []byte("\n"), noEOL(key), []byte("\n"), noEOL(root.PEM)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAnnotating(t)
			args := []string{"create", "mtls", "--type", "tls"}
			for _, file := range c.files {
				args = append(args, "-f", writeTemp(t, file))
			}
			var out bytes.Buffer
			errOut := captureStderr(t, func() {
				if err := secretCmd(&out, args); err != nil {
					t.Fatal(err)
				}
			})
			if !bytes.Equal(f.items["mtls"], c.want) {
				t.Errorf("stored %q, want %q", f.items["mtls"], c.want)
			}
			if !strings.Contains(errOut, "2 certificate(s) will be sent") {
				t.Errorf("stderr does not say the chain is the leaf and the intermediate: %q", errOut)
			}
			if got := strings.Contains(errOut, "not sent: "+root.Cert.Subject.String()); got != c.root {
				t.Errorf("stderr names the root as not sent = %v, want %v: %q", got, c.root, errOut)
			}
			for _, stream := range []string{out.String(), errOut} {
				if strings.Contains(stream, "PRIVATE KEY") || strings.Contains(stream, strings.TrimSpace(string(key))) {
					t.Errorf("a stream carries the key: %q", stream)
				}
			}
		})
	}
}

// Two keys, two certificates for one key and no certificate that matches the key
// are each refused from several -f too, nothing is stored, and no stream
// carries the key.
func TestSecretCreateTypeTLSRefusesAnAmbiguousLayout(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, key, err := root.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, otherKey, err := root.Client("other", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	again, err := root.Reissue(key, "inventory-client", now.Add(-time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		files [][]byte
		want  string
	}{
		{"two keys", [][]byte{leaf, key, otherKey}, "more than one private key"},
		{"a second certificate for the key", [][]byte{leaf, again, key}, "more than one certificate matches"},
		{"no certificate matches", [][]byte{otherLeaf, root.PEM, key}, "does not match"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAnnotating(t)
			args := []string{"create", "mtls", "--type", "tls"}
			for _, file := range c.files {
				args = append(args, "-f", writeTemp(t, file))
			}
			var out bytes.Buffer
			var runErr error
			errOut := captureStderr(t, func() { runErr = secretCmd(&out, args) })
			if runErr == nil || !strings.HasPrefix(runErr.Error(), "create mtls: the value is not a client certificate: ") ||
				!strings.Contains(runErr.Error(), c.want) {
				t.Fatalf("got %v, want a refusal saying %q", runErr, c.want)
			}
			if len(f.items["mtls"]) != 0 {
				t.Error("a refused value was stored")
			}
			for _, stream := range []string{out.String(), errOut, runErr.Error()} {
				if strings.Contains(stream, "PRIVATE KEY") || strings.Contains(stream, strings.TrimSpace(string(key))) {
					t.Errorf("a stream carries the key: %q", stream)
				}
			}
		})
	}
}
