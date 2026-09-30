package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bundleDir is a runtime bundle's share/guest as the installer leaves it: a
// record of two files, and the release's checksums.txt listing the record,
// with stand-ins for its signature files. It returns the record's bytes.
func bundleDir(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	record := []byte(hexOf([]byte("kernel")) + "  bzImage\n" + hexOf([]byte("initrd")) + "  container-initrd\n")
	list := hexOf([]byte("tarball")) + "  brig-standalone-v9-linux-amd64.tar.gz\n" +
		hexOf(record) + "  brig-standalone-v9-linux-amd64.boot-assets.sha256\n"
	for name, body := range map[string][]byte{
		RecordFile:       record,
		releaseChecksums: []byte(list),
		releaseSignature: []byte("sig"),
		releaseCert:      []byte("cert"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, record
}

func TestReadBundleRecordFindsTheRecordTheReleaseLists(t *testing.T) {
	dir, _ := bundleDir(t)
	rec, err := ReadBundleRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Release != "brig-standalone-v9-linux-amd64.boot-assets.sha256" {
		t.Errorf("Release = %q, want the name checksums.txt lists the record under", rec.Release)
	}
	want := BootDigests{
		"bzImage":          "sha256:" + hexOf([]byte("kernel")),
		"container-initrd": "sha256:" + hexOf([]byte("initrd")),
	}
	if len(rec.Files) != len(want) || rec.Files["bzImage"] != want["bzImage"] ||
		rec.Files["container-initrd"] != want["container-initrd"] {
		t.Errorf("Files = %v, want %v", rec.Files, want)
	}
	if rec.Checksums != filepath.Join(dir, "checksums.txt") || rec.Signature != filepath.Join(dir, "checksums.txt.sig") ||
		rec.Cert != filepath.Join(dir, "checksums.txt.pem") {
		t.Errorf("the release files are not the ones beside the record: %+v", rec)
	}
}

// A record changed after the release listed it, one byte is enough, is not
// the release's record.
func TestReadBundleRecordRefusesARecordTheReleaseDoesNotList(t *testing.T) {
	dir, record := bundleDir(t)
	changed := strings.Replace(string(record), hexOf([]byte("kernel")), hexOf([]byte("other kernel")), 1)
	if err := os.WriteFile(filepath.Join(dir, RecordFile), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadBundleRecord(dir)
	var unlisted *UnlistedRecordError
	if !errors.As(err, &unlisted) {
		t.Fatalf("err = %v, want UnlistedRecordError", err)
	}
	if unlisted.Digest != "sha256:"+hexOf([]byte(changed)) {
		t.Errorf("the error names %s, not the record's digest", unlisted.Digest)
	}
}

// Only a line that names a boot-asset record counts. The same bytes listed
// under another name are not the record.
func TestReadBundleRecordTakesOnlyABootAssetRecordLine(t *testing.T) {
	dir, record := bundleDir(t)
	list := hexOf(record) + "  install.sh\n"
	if err := os.WriteFile(filepath.Join(dir, releaseChecksums), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	var unlisted *UnlistedRecordError
	if _, err := ReadBundleRecord(dir); !errors.As(err, &unlisted) {
		t.Fatalf("err = %v, want UnlistedRecordError", err)
	}
}

func TestReadBundleRecordWithoutARecord(t *testing.T) {
	dir, _ := bundleDir(t)
	if err := os.Remove(filepath.Join(dir, RecordFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundleRecord(dir); !errors.Is(err, ErrNoBundleRecord) {
		t.Fatalf("err = %v, want ErrNoBundleRecord", err)
	}
	if HasBundleRecord(dir) {
		t.Error("HasBundleRecord is true for a directory with no record")
	}
}

// A record with no release beside it cannot be checked, and says which file
// is missing. It is not a record the release refused.
func TestReadBundleRecordMissingReleaseFiles(t *testing.T) {
	for _, name := range []string{releaseChecksums, releaseSignature, releaseCert} {
		dir, _ := bundleDir(t)
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		_, err := ReadBundleRecord(dir)
		var unlisted *UnlistedRecordError
		if err == nil || errors.As(err, &unlisted) || errors.Is(err, ErrNoBundleRecord) {
			t.Errorf("without %s: err = %v, want a record that cannot be read", name, err)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: the error does not name it: %v", name, err)
		}
	}
}

func TestParseSums(t *testing.T) {
	h := hexOf([]byte("x"))
	got, err := parseSums([]byte(h + "  text\n" + strings.ToUpper(h) + " *binary\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got["text"] != "sha256:"+h || got["binary"] != "sha256:"+h {
		t.Errorf("parseSums = %v", got)
	}
	for _, bad := range []string{"", "\n", "abc  file\n", h + "\n", h + "  \n"} {
		if _, err := parseSums([]byte(bad)); err == nil {
			t.Errorf("parseSums(%q) accepted it", bad)
		}
	}
}

// Blob asks cosign to verify the file against the release workflow's
// identity, and reads the answer three ways: verified, could not check, and
// failed.
func TestBlobOutcomes(t *testing.T) {
	origLook, origRun := lookPath, run
	t.Cleanup(func() { lookPath, run = origLook, origRun })
	lookPath = func(string) (string, error) { return "/usr/bin/cosign", nil }

	var args []string
	answer := func(out string, err error) {
		run = func(_ string, a ...string) (string, error) { args = a; return out, err }
	}
	p := RuntimeBundlePolicy()

	answer("Verified OK\n", nil)
	if res := p.Blob("c.txt", "c.sig", "c.pem"); res.Outcome != Verified {
		t.Errorf("a verified blob is %v", res.Outcome)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"verify-blob", "--certificate c.pem", "--signature c.sig",
		"--certificate-identity-regexp " + p.Identity, "--certificate-oidc-issuer " + p.Issuer} {
		if !strings.Contains(joined, want) {
			t.Errorf("cosign was not asked %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "c.txt" {
		t.Errorf("cosign checked %s, not the file", args[len(args)-1])
	}

	// What cosign 3 prints with no network, and for a changed file.
	deprecated := "Flag --certificate has been deprecated, please use --bundle\n" +
		"Flag --signature has been deprecated, please use --bundle\n"
	answer(deprecated+`Error: setting up clients and keys: getting rekor public keys: `+
		`tuf: failed to download 13.root.json: Get "https://tuf-repo-cdn.sigstore.dev/13.root.json": `+
		`dial tcp 127.0.0.1:9: connect: connection refused`+"\n", errors.New("exit status 1"))
	res := p.Blob("c.txt", "c.sig", "c.pem")
	if res.Outcome != Unresolved {
		t.Errorf("a cosign with no network is %v, want Unresolved", res.Outcome)
	}
	if strings.HasPrefix(res.Detail, "Flag --") {
		t.Errorf("the detail is cosign's deprecation notice: %s", res.Detail)
	}

	answer(deprecated+`Error: searching log query: [POST /api/v1/log/entries/retrieve][400] `+
		`searchLogQueryBadRequest {"code":400,"message":"verifying signature: invalid signature"}`+"\n",
		errors.New("exit status 1"))
	if res := p.Blob("c.txt", "c.sig", "c.pem"); res.Outcome != Failed || !strings.Contains(res.Detail, "invalid signature") {
		t.Errorf("a changed file is %v (%s), want Failed with cosign's reason", res.Outcome, res.Detail)
	}

	answer(deprecated+"Error: none of the expected identities matched what was in the certificate\n",
		errors.New("exit status 1"))
	if res := p.Blob("c.txt", "c.sig", "c.pem"); res.Outcome != Failed {
		t.Errorf("another signer is %v, want Failed", res.Outcome)
	}

	// A cosign cut off at cosignTimeout checked nothing, whatever it printed
	// before the cut.
	answer(deprecated, fmt.Errorf("wrapped: %w", errTimedOut))
	if res := p.Blob("c.txt", "c.sig", "c.pem"); res.Outcome != Unresolved || !res.TimedOut ||
		!strings.Contains(res.Detail, "did not answer") {
		t.Errorf("a cosign that timed out is %v (timed out %v, %s), want Unresolved", res.Outcome, res.TimedOut, res.Detail)
	}

	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if res := p.Blob("c.txt", "c.sig", "c.pem"); res.Outcome != NoTooling {
		t.Errorf("no cosign is %v, want NoTooling", res.Outcome)
	}
}
