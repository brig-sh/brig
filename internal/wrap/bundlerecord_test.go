package wrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// The name the release's checksums.txt lists the record under.
const recordRelease = "brig-standalone-v9.9.9-linux-amd64.boot-assets.sha256"

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blobCosign is fakeCosign that also answers verify-blob, with blob: "ok",
// "bad" for a signature that does not verify, or "offline" for a cosign that
// cannot reach Sigstore. Every call is appended to the log it returns.
func blobCosign(t *testing.T, blob string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "cosign"), filepath.Join(dir, "calls")
	answer := map[string]string{
		"ok":      "echo 'Verified OK' >&2",
		"bad":     "echo 'Error: none of the expected identities matched what was in the certificate' >&2; exit 1",
		"offline": `echo 'Error: getting rekor public keys: Get "https://tuf-repo-cdn.sigstore.dev/13.root.json": dial tcp: lookup tuf-repo-cdn.sigstore.dev: no such host' >&2; exit 1`,
	}[blob]
	script := "#!/bin/bash\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1\" in\n" +
		"  triangulate) echo \"ghcr.io/example/image:$(echo " + testDigest + " | sed 's/:/-/').sig\" ;;\n" +
		"  verify) exit 0 ;;\n" +
		"  verify-blob) " + answer + " ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// runtimeBundleDir is a runtime bundle's share/guest as its installer leaves
// it: the kernel and initrd, the record of the published pair, and the
// release's checksums.txt listing that record, with stand-in signature files.
// kernel is what is on disk, which may not be what the record lists.
func runtimeBundleDir(t *testing.T, kernel []byte) runtime.BootAssets {
	t.Helper()
	dir := t.TempDir()
	record := hexDigest(publishedKernel) + "  bzImage\n" + hexDigest(publishedInitrd) + "  container-initrd\n"
	checksums := hexDigest([]byte("tarball")) + "  brig-standalone-v9.9.9-linux-amd64.tar.gz\n" +
		hexDigest([]byte(record)) + "  " + recordRelease + "\n"
	for name, body := range map[string][]byte{
		"bzImage":           kernel,
		"container-initrd":  publishedInitrd,
		"SHA256SUMS":        []byte(record),
		"checksums.txt":     []byte(checksums),
		"checksums.txt.sig": []byte("sig"),
		"checksums.txt.pem": []byte("cert"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return runtime.BootAssets{
		Kernel: filepath.Join(dir, "bzImage"),
		Initrd: filepath.Join(dir, "container-initrd"),
		Named:  true,
	}
}

// bundleRecordRun is a genericBoot run on nerdctl through the runtime
// bundle's launcher, which points BRIG_BOOT_ASSETS at assets' directory. The
// boot bundle in the registry is not asked about: a test that reaches it
// fails.
func bundleRecordRun(t *testing.T, mode verify.Mode, assets runtime.BootAssets, blob string) (*Config, *resolvingRuntime, string) {
	t.Helper()
	t.Setenv("BRIG_BOOT_ASSETS", filepath.Dir(assets.Kernel))
	t.Setenv("BRIG_BOOT_ASSETS_REF", "")
	was := registryDigests
	registryDigests = func(ref, digest string) (verify.BootDigests, error) {
		t.Errorf("the boot bundle's manifest was read for a runtime bundle directory")
		return nil, fmt.Errorf("not here")
	}
	t.Cleanup(func() { registryDigests = was })

	rr := &resolvingRuntime{kind: "nerdctl"}
	c := resolvingConfig(t, mode, rr)
	bin, log := blobCosign(t, blob)
	c.VerifyPolicy.Cosign = bin
	rr.assets = assets
	return c, rr, log
}

func cosignCalls(t *testing.T, log string) string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The runtime bundle's kernel and initrd are its own, so what vouches for
// them is the record its release signed, and the boot bundle's signature is
// not asked for at all. Files that match boot, and the summary names them.
func TestTheRuntimeBundleBootsWhatItsSignedRecordLists(t *testing.T) {
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		c, rr, log := bundleRecordRun(t, mode, runtimeBundleDir(t, publishedKernel), "ok")

		_ = c.EnsureRunning(creds.Set{})

		reachedRun(t, string(mode), rr)
		said := c.Err.(*bytes.Buffer).String()
		if !strings.Contains(said, "image and boot assets verified") {
			t.Errorf("%s: the summary does not name the boot assets:\n%s", mode, said)
		}
		calls := cosignCalls(t, log)
		if strings.Contains(calls, "hull-assets") {
			t.Errorf("%s: cosign was asked about the boot bundle:\n%s", mode, calls)
		}
		dir := filepath.Dir(rr.assets.Kernel)
		want := fmt.Sprintf("verify-blob --certificate %s --signature %s", filepath.Join(dir, "checksums.txt.pem"),
			filepath.Join(dir, "checksums.txt.sig"))
		if !strings.Contains(calls, want) || !strings.Contains(calls, "brig-standalone-linux") ||
			!strings.Contains(calls, filepath.Join(dir, "checksums.txt")+"\n") {
			t.Errorf("%s: cosign did not check the release's checksums.txt against the bundle's workflow:\n%s", mode, calls)
		}
	}
}

// A kernel the signed record does not list is not the bundle's, whoever put
// it there, so it refuses in every mode but off, and says which file.
func TestAKernelTheRuntimeBundleRecordDoesNotListRefuses(t *testing.T) {
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		c, rr, _ := bundleRecordRun(t, mode, runtimeBundleDir(t, modifiedKernel), "ok")

		err := c.EnsureRunning(creds.Set{})

		refused(t, string(mode), err, rr)
		msg := err.Error()
		if !strings.Contains(msg, "bzImage is sha256:"+hexDigest(modifiedKernel)) ||
			!strings.Contains(msg, "sha256:"+hexDigest(publishedKernel)) {
			t.Errorf("%s: the refusal does not name the file and both digests: %v", mode, err)
		}
		if strings.Contains(msg, "container-initrd is") {
			t.Errorf("%s: the refusal names the initrd, which matched: %v", mode, err)
		}
		if !strings.Contains(msg, "install.sh") {
			t.Errorf("%s: the refusal names no way forward: %v", mode, err)
		}
	}
}

// A relative BRIG_BOOT_ASSETS reaches the runtime as an absolute path, and the
// record is found through the same one: a changed kernel refuses under warn,
// and an intact one verifies.
func TestARelativeRuntimeBundleDirectoryIsCheckedToo(t *testing.T) {
	for _, kernel := range [][]byte{modifiedKernel, publishedKernel} {
		assets := runtimeBundleDir(t, kernel)
		c, rr, _ := bundleRecordRun(t, verify.Warn, assets, "ok")
		dir := filepath.Dir(assets.Kernel)
		t.Chdir(filepath.Dir(dir))
		t.Setenv("BRIG_BOOT_ASSETS", filepath.Base(dir))

		err := c.EnsureRunning(creds.Set{})

		if bytes.Equal(kernel, modifiedKernel) {
			refused(t, "a changed kernel", err, rr)
			if !strings.Contains(err.Error(), "bzImage is sha256:"+hexDigest(modifiedKernel)) {
				t.Errorf("the refusal is not the record's: %v", err)
			}
			continue
		}
		reachedRun(t, "an intact kernel", rr)
		if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "boot assets verified") {
			t.Errorf("an intact kernel through a relative path did not verify:\n%s", said)
		}
	}
}

// A record rewritten to match a changed kernel is not the one the release
// listed, and a checksums.txt whose signature does not verify is not the
// release's. Both refuse under warn.
func TestARuntimeBundleRecordTheReleaseDidNotSignRefuses(t *testing.T) {
	assets := runtimeBundleDir(t, modifiedKernel)
	record := hexDigest(modifiedKernel) + "  bzImage\n" + hexDigest(publishedInitrd) + "  container-initrd\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(assets.Kernel), "SHA256SUMS"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	c, rr, _ := bundleRecordRun(t, verify.Warn, assets, "ok")
	err := c.EnsureRunning(creds.Set{})
	refused(t, "rewritten record", err, rr)
	if !strings.Contains(err.Error(), "does not list") {
		t.Errorf("the refusal does not say the release does not list the record: %v", err)
	}

	c, rr, _ = bundleRecordRun(t, verify.Warn, runtimeBundleDir(t, publishedKernel), "bad")
	err = c.EnsureRunning(creds.Set{})
	refused(t, "bad signature", err, rr)
	if !strings.Contains(err.Error(), "did not verify") || !strings.Contains(err.Error(), "none of the expected identities") {
		t.Errorf("the refusal does not give cosign's reason: %v", err)
	}
}

// A record brig cannot check -- Sigstore out of reach, or no signature files
// beside it -- is "cannot check": said under warn, and the run boots without
// the summary naming the boot assets. require refuses.
func TestARuntimeBundleRecordBrigCannotCheckWarnsOrRefuses(t *testing.T) {
	noSig := func(t *testing.T) runtime.BootAssets {
		assets := runtimeBundleDir(t, publishedKernel)
		if err := os.Remove(filepath.Join(filepath.Dir(assets.Kernel), "checksums.txt.sig")); err != nil {
			t.Fatal(err)
		}
		return assets
	}
	cases := []struct {
		name   string
		assets func(*testing.T) runtime.BootAssets
		blob   string
		cause  string
	}{
		{"offline", func(t *testing.T) runtime.BootAssets { return runtimeBundleDir(t, publishedKernel) }, "offline", "could not reach Sigstore"},
		{"no signature", noSig, "ok", "checksums.txt.sig"},
	}
	for _, tc := range cases {
		c, rr, _ := bundleRecordRun(t, verify.Warn, tc.assets(t), tc.blob)
		_ = c.EnsureRunning(creds.Set{})
		reachedRun(t, tc.name+", warn", rr)
		said := c.Err.(*bytes.Buffer).String()
		if !strings.Contains(said, "cannot check the Linux runtime bundle's kernel and initrd") ||
			!strings.Contains(said, tc.cause) {
			t.Errorf("%s: warn did not say what it could not check and why:\n%s", tc.name, said)
		}
		if strings.Contains(said, "boot assets verified") {
			t.Errorf("%s: the summary claimed the boot assets verified:\n%s", tc.name, said)
		}

		c, rr, _ = bundleRecordRun(t, verify.Require, tc.assets(t), tc.blob)
		err := c.EnsureRunning(creds.Set{})
		refused(t, tc.name+", require", err, rr)
		if !strings.Contains(err.Error(), "BRIG_VERIFY=require") {
			t.Errorf("%s: the refusal does not name the setting: %v", tc.name, err)
		}
	}
}

// Comparing the files with the record needs no network and no signature, so a
// changed kernel refuses under warn even where the signature cannot be
// checked. So does a record checksums.txt does not list, with the signature
// file gone.
func TestAChangedKernelRefusesWhereTheSignatureCannotBeChecked(t *testing.T) {
	dropSig := func(assets runtime.BootAssets) {
		if err := os.Remove(filepath.Join(filepath.Dir(assets.Kernel), "checksums.txt.sig")); err != nil {
			t.Fatal(err)
		}
	}
	offline := runtimeBundleDir(t, modifiedKernel)
	noSig := runtimeBundleDir(t, modifiedKernel)
	dropSig(noSig)
	for name, tc := range map[string]struct {
		assets runtime.BootAssets
		blob   string
	}{"offline": {offline, "offline"}, "no signature": {noSig, "ok"}} {
		c, rr, _ := bundleRecordRun(t, verify.Warn, tc.assets, tc.blob)
		err := c.EnsureRunning(creds.Set{})
		refused(t, name, err, rr)
		if !strings.Contains(err.Error(), "bzImage is sha256:"+hexDigest(modifiedKernel)) {
			t.Errorf("%s: the refusal is not the comparison's: %v", name, err)
		}
	}

	unlisted := runtimeBundleDir(t, publishedKernel)
	dropSig(unlisted)
	f, err := os.OpenFile(filepath.Join(filepath.Dir(unlisted.Kernel), "SHA256SUMS"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n")
	_ = f.Close()
	c, rr, _ := bundleRecordRun(t, verify.Warn, unlisted, "ok")
	err = c.EnsureRunning(creds.Set{})
	refused(t, "unlisted, no signature", err, rr)
	if !strings.Contains(err.Error(), "does not list") {
		t.Errorf("an unlisted record without its signature file is not refused as unlisted: %v", err)
	}
}

// The runtime bundle runs on Linux alone. On hull, a named directory that
// holds a SHA256SUMS is somebody's build, and it gets the boot bundle's
// check, not the runtime bundle's.
func TestASHA256SUMSOnHullIsNotTheRuntimeBundle(t *testing.T) {
	c, rr, log := bundleRecordRun(t, verify.Warn, runtimeBundleDir(t, publishedKernel), "ok")
	rr.kind = "hull"
	registryServes(t, published())

	_ = c.EnsureRunning(creds.Set{})

	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "Linux runtime bundle") {
		t.Errorf("a hull run took the runtime bundle's check:\n%s", said)
	}
	if calls := cosignCalls(t, log); strings.Contains(calls, "verify-blob") || !strings.Contains(calls, "hull-assets") {
		t.Errorf("a hull run did not check the boot bundle's signature:\n%s", calls)
	}
}

// A runtime bundle from before the record reaches the boot bundle's check,
// which lists neither of its files. The warning says so, not only the
// refusal under require.
func TestAnOlderRuntimeBundleIsNamedUnderWarnToo(t *testing.T) {
	registryServes(t, published())
	c, rr := digestRun(t, verify.Warn, writeBootAssets(t, modifiedKernel, publishedInitrd, true))
	rr.kind = "nerdctl"
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "warn", rr)
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "re-run brig's install.sh") {
		t.Errorf("warn does not say an older runtime bundle keeps no record:\n%s", said)
	}
}

// A runtime bundle released from a fork is signed by that fork's workflow.
// BRIG_VERIFY_RUNTIME_IDENTITY and _ISSUER point the record check at it, as
// INSTALL_BRIG_SIG_IDENTITY points the bundle's installer; per agent too, like
// every other setting.
func TestTheRuntimeBundleSignerCanBeRepointed(t *testing.T) {
	vars := map[string]string{
		"BRIG_VERIFY_RUNTIME_IDENTITY":           `^https://github\.com/me/fork/`,
		"BRIG_CLAUDE_CODE_VERIFY_RUNTIME_ISSUER": "https://issuer.example",
	}
	env := NewEnv("claude-code", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	p := runtimePolicy(env)
	if p.Identity != vars["BRIG_VERIFY_RUNTIME_IDENTITY"] || p.Issuer != "https://issuer.example" {
		t.Errorf("runtimePolicy = %+v, want the identity and issuer set", p)
	}
	if def := runtimePolicy(NewEnv("claude-code", func(string) (string, bool) { return "", false })); def != verify.RuntimeBundlePolicy() {
		t.Errorf("with nothing set, runtimePolicy = %+v, want the bundle's own workflow", def)
	}

	c, rr, log := bundleRecordRun(t, verify.Require, runtimeBundleDir(t, publishedKernel), "ok")
	c.RuntimePolicy = p
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "require", rr)
	calls := cosignCalls(t, log)
	if !strings.Contains(calls, "--certificate-identity-regexp "+p.Identity) ||
		!strings.Contains(calls, "--certificate-oidc-issuer https://issuer.example") ||
		strings.Contains(calls, "brig-standalone-linux") {
		t.Errorf("the record was not checked against the fork's signer:\n%s", calls)
	}
}
