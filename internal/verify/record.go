package verify

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The Linux runtime bundle carries its own kernel and initrd, and its launcher
// points BRIG_BOOT_ASSETS at them. None of it comes from the boot bundle in
// the registry, so the registry manifest vouches for none of it. What does is
// the bundle's release: it lists share/guest/SHA256SUMS in a checksums.txt
// that its release workflow signs, and the installer keeps that checksums.txt,
// with its signature and certificate, beside the files. This file is the
// reading of that record (#234).

// RecordFile is the record the runtime bundle keeps beside its kernel and
// initrd: their sha256, in sha256sum's format.
const RecordFile = "SHA256SUMS"

// The release's checksums.txt, and the signature and certificate cosign
// sign-blob wrote for it, as the installer keeps them beside RecordFile.
const (
	releaseChecksums = "checksums.txt"
	releaseSignature = "checksums.txt.sig"
	releaseCert      = "checksums.txt.pem"
)

// recordSuffix is how checksums.txt names the record: the release publishes
// SHA256SUMS as <bundle>.boot-assets.sha256.
const recordSuffix = ".boot-assets.sha256"

// ErrNoBundleRecord is a directory with no RecordFile, which is not a runtime
// bundle's, or one from before the bundle kept a record.
var ErrNoBundleRecord = errors.New("no runtime bundle record")

// BundleRecord is a runtime bundle's record of its kernel and initrd, found in
// the release's checksums.txt. Finding it there says the release listed these
// bytes. It says nothing about who wrote checksums.txt until
// RuntimeBundlePolicy's Blob has checked its signature.
type BundleRecord struct {
	Dir string
	// Files are the digests the record lists, by file name.
	Files BootDigests
	// Release is the name checksums.txt lists the record under, which names
	// the bundle and its version.
	Release string
	// Checksums, Signature and Cert are the paths of the release's
	// checksums.txt and its signature files, for Blob.
	Checksums, Signature, Cert string
}

// UnlistedRecordError is a RecordFile that the release's checksums.txt beside
// it does not list, so it is not the record the release published.
type UnlistedRecordError struct {
	Dir    string
	Digest string
}

func (e *UnlistedRecordError) Error() string {
	return fmt.Sprintf("%s in %s is %s, which the release's %s does not list",
		RecordFile, e.Dir, e.Digest, releaseChecksums)
}

// HasBundleRecord reports whether dir holds a runtime bundle's record. It
// decides which check a directory gets, so it looks only for the file.
func HasBundleRecord(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, RecordFile))
	return err == nil && info.Mode().IsRegular()
}

// RecordDigests reads the record in dir: the sha256 it lists for each file,
// or ErrNoBundleRecord when dir has none. It reads nothing else, so it needs no
// network and no signature, and what it lists is vouched for only once
// ReadBundleRecord has found it in checksums.txt and Blob has checked that.
func RecordDigests(dir string) (BootDigests, error) {
	_, files, err := readRecord(dir)
	return files, err
}

// readRecord is RecordDigests, with the record's bytes.
func readRecord(dir string) ([]byte, BootDigests, error) {
	recordPath := filepath.Join(dir, RecordFile)
	raw, err := os.ReadFile(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNoBundleRecord
	}
	if err != nil {
		return nil, nil, err
	}
	files, err := parseSums(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", recordPath, err)
	}
	return raw, files, nil
}

// ReadBundleRecord reads the record in dir and finds it in the release's
// checksums.txt beside it. ErrNoBundleRecord when dir has no record, and
// UnlistedRecordError when checksums.txt does not list it, which needs no
// signature to decide. Any other error is a record brig cannot read, a missing
// signature file among them.
func ReadBundleRecord(dir string) (*BundleRecord, error) {
	raw, files, err := readRecord(dir)
	if err != nil {
		return nil, err
	}
	rec := &BundleRecord{
		Dir:       dir,
		Files:     files,
		Checksums: filepath.Join(dir, releaseChecksums),
		Signature: filepath.Join(dir, releaseSignature),
		Cert:      filepath.Join(dir, releaseCert),
	}
	listRaw, err := os.ReadFile(rec.Checksums)
	if err != nil {
		return nil, fmt.Errorf("the release's %s is not beside %s: %w", releaseChecksums, RecordFile, err)
	}
	listed, err := parseSums(listRaw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rec.Checksums, err)
	}
	digest := sha256Digest(raw)
	for name, d := range listed {
		if strings.HasSuffix(name, recordSuffix) && d == digest {
			rec.Release = name
			break
		}
	}
	if rec.Release == "" {
		return nil, &UnlistedRecordError{Dir: dir, Digest: digest}
	}
	for _, path := range []string{rec.Signature, rec.Cert} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("the release's signature files are not beside %s: %w", RecordFile, err)
		}
	}
	return rec, nil
}

// parseSums reads sha256sum's output: a hex digest, a space, and a name
// marked " " for text or "*" for binary.
func parseSums(raw []byte) (BootDigests, error) {
	files := BootDigests{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimPrefix(name, " "), "*")
		d := "sha256:" + strings.ToLower(sum)
		if !ok || name == "" || !isDigest(d) {
			return nil, fmt.Errorf("line %d is not a sha256sum line", n)
		}
		files[name] = d
	}
	if len(files) == 0 {
		return nil, errors.New("lists no files")
	}
	return files, nil
}

// RuntimeBundlePolicy is the trust root for the Linux runtime bundle's
// releases: the release workflow of its repository, on a tag. Its own
// identity, as the boot bundle has, because a signature from brig's image
// workflow says nothing about a release of the runtime bundle.
func RuntimeBundlePolicy() Policy {
	return Policy{
		Identity: `^https://github\.com/NOFireAI/brig-standalone-linux/\.github/workflows/release\.yml@refs/tags/`,
		Issuer:   "https://token.actions.githubusercontent.com",
		Cosign:   "cosign",
	}
}

// Blob checks the keyless signature cosign sign-blob made over file.
//
// cosign reaches Sigstore's TUF repository and the transparency log to check
// it. A cosign that could not reach them has not checked anything, so a
// failure that reads like the network is Unresolved, "could not check", the
// way a registry out of reach is for an image. So is a cosign cut off at
// cosignTimeout. Any other failure is Failed.
func (p Policy) Blob(file, sig, cert string) Result {
	if _, err := lookPath(p.Cosign); err != nil {
		return Result{Policy: p, Outcome: NoTooling, Image: file}
	}
	out, err := run(p.Cosign,
		"verify-blob",
		"--certificate", cert,
		"--signature", sig,
		"--certificate-identity-regexp", p.Identity,
		"--certificate-oidc-issuer", p.Issuer,
		file,
	)
	if err == nil {
		return Result{Policy: p, Outcome: Verified, Image: file}
	}
	if errors.Is(err, errTimedOut) {
		return Result{Policy: p, Outcome: Unresolved, Image: file, TimedOut: true,
			Detail: fmt.Sprintf("cosign did not answer within %s", cosignTimeout)}
	}
	detail := firstBlobLine(out, err)
	if unreachable(out) {
		return Result{Policy: p, Outcome: Unresolved, Image: file, Detail: detail}
	}
	return Result{Policy: p, Outcome: Failed, Image: file, Detail: detail}
}

// unreachable reports whether cosign's output reads like a network that did
// not answer, rather than an answer that refused the signature.
func unreachable(out string) bool {
	for _, s := range []string{
		"dial tcp", "no such host", "i/o timeout", "connection refused",
		"connection reset", "network is unreachable", "TLS handshake timeout",
		"context deadline exceeded", "Client.Timeout",
	} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// firstBlobLine is firstLine past the two deprecation notices cosign 3 prints
// for --certificate and --signature before any real answer.
func firstBlobLine(out string, err error) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Flag --") {
			continue
		}
		kept = append(kept, line)
	}
	return firstLine(strings.Join(kept, "\n"), err)
}
