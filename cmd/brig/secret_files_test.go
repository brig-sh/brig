package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/testpki"
)

// Several -f flags give what `cat` of the same files gives, including when a
// file has no final line break, and the check reads the result in any order.
func TestSecretCreateSeveralFilesAreAppendedInOrder(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key, err := root.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	noEOL := bytes.TrimSuffix(cert, []byte("\n"))

	f := newAnnotating(t)
	captureStderr(t, func() {
		args := []string{"create", "mtls", "--type", "tls",
			"-f", writeTemp(t, noEOL), "-f", writeTemp(t, key), "--file", writeTemp(t, root.PEM)}
		if err := secretCmd(&bytes.Buffer{}, args); err != nil {
			t.Fatal(err)
		}
	})
	want := append(append(append([]byte{}, noEOL...), '\n'), key...)
	want = append(want, root.PEM...)
	if !bytes.Equal(f.items["mtls"], want) {
		t.Errorf("stored %q, want the files appended with a break after the one that lacked it", f.items["mtls"])
	}
}

// The root before the leaf is a PKI export order, and it is accepted.
func TestSecretCreateTypeTLSAcceptsTheRootFirst(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key, err := root.Client("inventory-client", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := newAnnotating(t)
	errOut := captureStderr(t, func() {
		args := []string{"create", "mtls", "--type", "tls", "-f", writeTemp(t, root.PEM), "-f", writeTemp(t, cert), "-f", writeTemp(t, key)}
		if err := secretCmd(&bytes.Buffer{}, args); err != nil {
			t.Fatal(err)
		}
	})
	if len(f.items["mtls"]) == 0 {
		t.Error("nothing was stored")
	}
	if !strings.Contains(errOut, "1 certificate") {
		t.Errorf("stderr does not say how long the chain is: %q", errOut)
	}
	if strings.Contains(errOut, "PRIVATE KEY") {
		t.Errorf("stderr carries the key: %q", errOut)
	}
}

func TestSecretCreateTypeTLSRefusesTheLeafTwice(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key, err := root.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The leaf issued a second time for the same key is another certificate.
	// The same certificate twice is not (see
	// TestSecretCreateTypeTLSTakesTheSameCertificateOnce).
	again, err := root.Reissue(key, "c", now.Add(-time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := newAnnotating(t)
	err = secretCmd(&bytes.Buffer{}, []string{"create", "mtls", "--type", "tls",
		"-f", writeTemp(t, cert), "-f", writeTemp(t, again), "-f", writeTemp(t, key)})
	if err == nil || !strings.Contains(err.Error(), "more than one certificate") {
		t.Fatalf("err = %v, want the duplicate refused", err)
	}
	if len(f.items) != 0 {
		t.Errorf("a refused value was written: %v", f.items)
	}
}

func TestSecretCreateTypeTLSTakesTheSameCertificateOnce(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key, err := root.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := newAnnotating(t)
	errOut := captureStderr(t, func() {
		err = secretCmd(&bytes.Buffer{}, []string{"create", "mtls", "--type", "tls",
			"-f", writeTemp(t, cert), "-f", writeTemp(t, cert), "-f", writeTemp(t, key)})
	})
	if err != nil {
		t.Fatalf("a leaf that appears twice: %v", err)
	}
	if len(f.items["mtls"]) == 0 || !strings.Contains(errOut, "1 certificate(s) will be sent, 0 will not") {
		t.Errorf("stored %d bytes, stderr %q", len(f.items["mtls"]), errOut)
	}
}

// existingStore refuses a create over a name that is taken, as the real
// stores do.
type existingStore struct{ *fakeAnnotator }

func (s existingStore) Write(name string, value []byte, p secret.Provenance, update bool) error {
	if _, ok := s.items[name]; ok && !update {
		return secret.ErrExists
	}
	return s.fakeAnnotator.Write(name, value, p, update)
}

// The summary says what was stored. A create that the store refuses stored
// nothing, so it prints no summary.
func TestSecretCreateTypeTLSPrintsTheSummaryOnlyAfterTheWrite(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key, err := root.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := newAnnotating(t)
	f.items["mtls"] = []byte("older")
	openStore = func() (secret.Store, error) { return existingStore{f}, nil }
	var errOut string
	errOut = captureStderr(t, func() {
		err = secretCmd(&bytes.Buffer{}, []string{"create", "mtls", "--type", "tls",
			"-f", writeTemp(t, cert), "-f", writeTemp(t, key)})
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want the name taken", err)
	}
	if errOut != "" {
		t.Errorf("a create the store refused printed a summary: %q", errOut)
	}
	if string(f.items["mtls"]) != "older" {
		t.Errorf("the taken name was overwritten")
	}
	errOut = captureStderr(t, func() {
		err = secretCmd(&bytes.Buffer{}, []string{"update", "mtls", "--type", "tls",
			"-f", writeTemp(t, cert), "-f", writeTemp(t, key)})
	})
	if err != nil || !strings.Contains(errOut, "subject CN=c") {
		t.Errorf("an update that was stored: err %v, stderr %q", err, errOut)
	}
}

// -f - among several files is stdin, read the way a lone -f - reads it.
func TestSecretCreateSeveralFilesReadStdinLikeASingleOne(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, "from-stdin\n")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "one", "-f", "-"}); err != nil {
		t.Fatal(err)
	}
	pipeStdin(t, "from-stdin\n")
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "two", "-f", writeTemp(t, []byte("head\n")), "-f", "-"}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.items["one"]); got != "from-stdin" {
		t.Errorf("a lone -f - stored %q, want the line ending stripped", got)
	}
	if got := string(f.items["two"]); got != "head\nfrom-stdin" {
		t.Errorf("-f file -f - stored %q, want the same stdin value after the file", got)
	}
}

// Reading a value from a terminal keeps it in the scrollback, which is why a
// lone -f - refuses one. Beside other files it is no safer.
func TestSecretCreateSeveralFilesRefuseStdinFromATerminal(t *testing.T) {
	f := newFake(t)
	terminalStdin(t)
	err := secretCmd(&bytes.Buffer{}, []string{"create", "two", "-f", writeTemp(t, []byte("head\n")), "-f", "-"})
	if err == nil || !strings.Contains(err.Error(), "no value on stdin") {
		t.Fatalf("err = %v, want the terminal refused", err)
	}
	if len(f.items) != 0 {
		t.Errorf("a refused value was written: %v", f.items)
	}
}

func TestSecretCreateTypeCARefusesALeafWithNoCA(t *testing.T) {
	root, err := testpki.NewCA("root")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, _, err := root.Client("c", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := newAnnotating(t)
	err = secretCmd(&bytes.Buffer{}, []string{"create", "ca", "--type", "ca", "-f", writeTemp(t, cert)})
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("err = %v, want a refusal naming CA", err)
	}
	if len(f.items) != 0 {
		t.Errorf("a refused value was written: %v", f.items)
	}
}
