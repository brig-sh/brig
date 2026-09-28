package wrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// The bytes the verified bundle lists, and a kernel with one byte changed.
var (
	publishedKernel = []byte("the kernel the bundle lists")
	publishedInitrd = []byte("the initrd the bundle lists")
	modifiedKernel  = []byte("the kernel the bundle lists!")
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeBootAssets puts a kernel and an initrd in a fresh directory and returns
// them the way a resolve does.
func writeBootAssets(t *testing.T, kernel, initrd []byte, named bool) runtime.BootAssets {
	t.Helper()
	dir := t.TempDir()
	assets := runtime.BootAssets{
		Kernel: filepath.Join(dir, "Image"),
		Initrd: filepath.Join(dir, "container-initrd"),
		Named:  named,
	}
	if err := os.WriteFile(assets.Kernel, kernel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assets.Initrd, initrd, 0o644); err != nil {
		t.Fatal(err)
	}
	return assets
}

// registryServes stands the registry in with the published bundle's digests
// for the bundle that verified, testDigest, and an error for any other.
func registryServes(t *testing.T, files verify.BootDigests) {
	t.Helper()
	was := registryDigests
	registryDigests = func(ref, digest string) (verify.BootDigests, error) {
		if digest != testDigest {
			return nil, fmt.Errorf("asked for %s, not the bundle that verified", digest)
		}
		return files, nil
	}
	t.Cleanup(func() { registryDigests = was })
}

func published() verify.BootDigests {
	return verify.BootDigests{"Image": digestOf(publishedKernel), "container-initrd": digestOf(publishedInitrd)}
}

// digestRun is a genericBoot run whose image and bundle signatures verify
// under mode, and whose runtime resolves assets.
func digestRun(t *testing.T, mode verify.Mode, assets runtime.BootAssets) (*Config, *resolvingRuntime) {
	t.Helper()
	t.Setenv("BRIG_BOOT_ASSETS_REF", "")
	rr := &resolvingRuntime{}
	c := resolvingConfig(t, mode, rr)
	rr.assets = assets
	return c, rr
}

func refused(t *testing.T, what string, err error, rr *resolvingRuntime) {
	t.Helper()
	if err == nil || rr.booted != nil {
		t.Fatalf("%s: the run reached the runtime (err %v)", what, err)
	}
	var vr *VerifyRefusedError
	if !errors.As(err, &vr) {
		t.Errorf("%s: the refusal is not a verification refusal: %v", what, err)
	}
}

func reachedRun(t *testing.T, what string, rr *resolvingRuntime) {
	t.Helper()
	if rr.booted == nil {
		t.Fatalf("%s: the run did not reach the runtime", what)
	}
}

// With BRIG_BOOT_ASSETS unset, brig chose the directory and fetched into it,
// so a kernel that is not the one the verified bundle lists has no innocent
// reading. It refuses under warn as under require, and says which file and
// which digest it expected.
func TestAModifiedKernelRefusesWhereBrigChoseTheDirectory(t *testing.T) {
	registryServes(t, published())
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		c, rr := digestRun(t, mode, writeBootAssets(t, modifiedKernel, publishedInitrd, false))

		err := c.EnsureRunning(creds.Set{})

		refused(t, string(mode), err, rr)
		msg := err.Error()
		if !strings.Contains(msg, "Image") || !strings.Contains(msg, digestOf(publishedKernel)) {
			t.Errorf("%s: the refusal does not name the file and the digest it expected: %v", mode, err)
		}
		if strings.Contains(msg, "container-initrd") {
			t.Errorf("%s: the refusal names the initrd, which matched: %v", mode, err)
		}
		if !strings.Contains(msg, "Delete both files") {
			t.Errorf("%s: the refusal names no way forward: %v", mode, err)
		}
		if said := c.Err.(*bytes.Buffer).String(); strings.Contains(said, "boot assets verified") {
			t.Errorf("%s: the summary claimed the boot assets verified:\n%s", mode, said)
		}
	}
}

// And the bytes the bundle lists boot, and only then does the summary name
// the boot assets.
func TestTheBundleBytesBootAndTheSummaryNamesThem(t *testing.T) {
	registryServes(t, published())
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		for _, named := range []bool{false, true} {
			what := fmt.Sprintf("%s, named %v", mode, named)
			c, rr := digestRun(t, mode, writeBootAssets(t, publishedKernel, publishedInitrd, named))

			_ = c.EnsureRunning(creds.Set{})

			reachedRun(t, what, rr)
			if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "image and boot assets verified") {
				t.Errorf("%s: the summary does not name the boot assets after both digests matched:\n%s", what, said)
			}
		}
	}
}

// The summary names the boot assets only when both digests match. A signature
// that verified is not enough, and neither is a kernel that matched beside an
// initrd that did not.
func TestTheSummaryNamesTheBootAssetsOnlyWhenBothDigestsMatch(t *testing.T) {
	registryServes(t, published())
	c, rr := digestRun(t, verify.Warn, writeBootAssets(t, publishedKernel, []byte("another initrd"), true))

	_ = c.EnsureRunning(creds.Set{})

	reachedRun(t, "a named directory under warn", rr)
	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "boot assets verified") {
		t.Errorf("the summary named the boot assets with the initrd differing:\n%s", said)
	}
	if !strings.Contains(said, "image verified") {
		t.Errorf("the image verified and the summary does not say so:\n%s", said)
	}
}

// A directory named in BRIG_BOOT_ASSETS on hull has nothing to do with the
// Linux runtime bundle. A hand-copied directory with one byte changed, under
// require, refused naming that bundle on a Mac. The refusal still names a
// way forward.
func TestANamedDirectoryOnHullDoesNotNameTheLinuxBundle(t *testing.T) {
	registryServes(t, published())
	c, rr := digestRun(t, verify.Require, writeBootAssets(t, modifiedKernel, publishedInitrd, true))
	err := c.EnsureRunning(creds.Set{})
	refused(t, "a differing kernel", err, rr)
	if strings.Contains(err.Error(), "Linux runtime bundle") {
		t.Errorf("a hull run named the Linux runtime bundle: %v", err)
	}
	if !strings.Contains(err.Error(), "BRIG_VERIFY=warn") {
		t.Errorf("the refusal names no way forward: %v", err)
	}

	registryDigests = func(string, string) (verify.BootDigests, error) {
		return nil, errors.New("no registry here")
	}
	c, rr = digestRun(t, verify.Require, writeBootAssets(t, publishedKernel, publishedInitrd, true))
	err = c.EnsureRunning(creds.Set{})
	refused(t, "unread digests", err, rr)
	if strings.Contains(err.Error(), "Linux runtime bundle") {
		t.Errorf("a hull run named the Linux runtime bundle: %v", err)
	}
}

// A directory named in BRIG_BOOT_ASSETS is someone's build. Under warn the
// difference is stated and it boots. Under require it refuses, and on nerdctl
// says why a directory from the Linux runtime bundle lands here.
func TestAModifiedKernelInANamedDirectory(t *testing.T) {
	registryServes(t, published())

	c, rr := digestRun(t, verify.Warn, writeBootAssets(t, modifiedKernel, publishedInitrd, true))
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "warn", rr)
	said := c.Err.(*bytes.Buffer).String()
	if !strings.Contains(said, "BRIG_BOOT_ASSETS") || !strings.Contains(said, digestOf(publishedKernel)) ||
		!strings.Contains(said, "Image") {
		t.Errorf("warn did not state the difference, the file and the expected digest:\n%s", said)
	}

	c, rr = digestRun(t, verify.Require, writeBootAssets(t, modifiedKernel, publishedInitrd, true))
	rr.kind = "nerdctl"
	err := c.EnsureRunning(creds.Set{})
	refused(t, "require", err, rr)
	if !strings.Contains(err.Error(), "Linux runtime bundle") {
		t.Errorf("the refusal does not say why a Linux bundle directory refuses: %v", err)
	}
	if !strings.Contains(err.Error(), "BRIG_VERIFY=warn") {
		t.Errorf("the refusal names no way forward: %v", err)
	}
}

// With no registry answer and no record to fall back on, nothing says what
// the files ought to be. warn states it and boots, require refuses.
func TestUnreadableBootDigestsWarnOrRefuse(t *testing.T) {
	for _, named := range []bool{false, true} {
		c, rr := digestRun(t, verify.Warn, writeBootAssets(t, modifiedKernel, publishedInitrd, named))
		_ = c.EnsureRunning(creds.Set{})
		reachedRun(t, fmt.Sprintf("warn, named %v", named), rr)
		said := c.Err.(*bytes.Buffer).String()
		if !strings.Contains(said, "digests") || strings.Contains(said, "boot assets verified") {
			t.Errorf("named %v: warn did not state that the digests were not read:\n%s", named, said)
		}

		c, rr = digestRun(t, verify.Require, writeBootAssets(t, publishedKernel, publishedInitrd, named))
		err := c.EnsureRunning(creds.Set{})
		refused(t, fmt.Sprintf("require, named %v", named), err, rr)
	}
}

// Where the registry cannot answer, hull's record stands in, but only a record
// of the bundle that verified. One for another bundle says the files came
// from that bundle, which refuses as a differing digest does.
func TestAProvenanceRecordForAnotherDigestRefuses(t *testing.T) {
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		assets := writeBootAssets(t, publishedKernel, publishedInitrd, false)
		writeRecord(t, assets, "sha256:"+strings.Repeat("2", 64), published())
		c, rr := digestRun(t, mode, assets)

		err := c.EnsureRunning(creds.Set{})

		refused(t, string(mode), err, rr)
		if !strings.Contains(err.Error(), testDigest) {
			t.Errorf("%s: the refusal does not name the bundle that verified: %v", mode, err)
		}
	}
}

// A registry that answered the verified digest with bytes brig refused is not
// a registry that did not answer. hull's record does not stand in for it,
// even one that lists the files on disk: require refuses, warn says what the
// registry did and boots without naming the boot assets verified.
func TestARefusedRegistryAnswerIsNotReplacedByTheRecord(t *testing.T) {
	const why = "the registry answered with a manifest whose digest is sha256:0000"
	was := registryDigests
	registryDigests = func(string, string) (verify.BootDigests, error) {
		return nil, &verify.ManifestRefusedError{Reason: why}
	}
	t.Cleanup(func() { registryDigests = was })

	assets := writeBootAssets(t, publishedKernel, publishedInitrd, false)
	writeRecord(t, assets, testDigest, published())
	c, rr := digestRun(t, verify.Require, assets)
	err := c.EnsureRunning(creds.Set{})
	refused(t, "require", err, rr)
	if !strings.Contains(err.Error(), why) {
		t.Errorf("the refusal does not say what the registry answered: %v", err)
	}
	if strings.Contains(err.Error(), "did not answer") {
		t.Errorf("the refusal says the registry did not answer, and it did: %v", err)
	}

	assets = writeBootAssets(t, publishedKernel, publishedInitrd, false)
	writeRecord(t, assets, testDigest, published())
	c, rr = digestRun(t, verify.Warn, assets)
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "warn", rr)
	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "boot assets verified") {
		t.Errorf("hull's record stood in for a registry answer brig refused:\n%s", said)
	}
	if !strings.Contains(said, why) {
		t.Errorf("warn does not say what the registry answered:\n%s", said)
	}
}

// A record of the verified bundle with no entry for the kernel says nothing
// about it, so it counts as unreadable.
func TestAProvenanceRecordWithNoKernelEntryIsUnreadable(t *testing.T) {
	files := verify.BootDigests{"container-initrd": digestOf(publishedInitrd)}

	assets := writeBootAssets(t, publishedKernel, publishedInitrd, false)
	writeRecord(t, assets, testDigest, files)
	c, rr := digestRun(t, verify.Warn, assets)
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "warn", rr)
	if said := c.Err.(*bytes.Buffer).String(); strings.Contains(said, "boot assets verified") {
		t.Errorf("a record with no kernel entry verified the kernel:\n%s", said)
	}

	assets = writeBootAssets(t, publishedKernel, publishedInitrd, false)
	writeRecord(t, assets, testDigest, files)
	c, rr = digestRun(t, verify.Require, assets)
	refused(t, "require", c.EnsureRunning(creds.Set{}), rr)
}

// A record of the verified bundle that lists the files on disk sits beside
// them, and anything that rewrote the kernel could have rewritten it to match.
// So a match is "cannot check" and never "boot assets verified": warn says so
// and boots, require refuses. The changed kernel with a record rewritten to
// match it is the case that matters.
func TestAProvenanceRecordOfTheVerifiedBundleDoesNotVerify(t *testing.T) {
	for _, kernel := range [][]byte{publishedKernel, modifiedKernel} {
		files := verify.BootDigests{"Image": digestOf(kernel), "container-initrd": digestOf(publishedInitrd)}

		assets := writeBootAssets(t, kernel, publishedInitrd, false)
		writeRecord(t, assets, testDigest, files)
		c, rr := digestRun(t, verify.Require, assets)
		err := c.EnsureRunning(creds.Set{})
		refused(t, "require", err, rr)
		if !strings.Contains(err.Error(), "does not vouch for them") {
			t.Errorf("the refusal does not say why the record is not enough: %v", err)
		}

		assets = writeBootAssets(t, kernel, publishedInitrd, false)
		writeRecord(t, assets, testDigest, files)
		c, rr = digestRun(t, verify.Warn, assets)
		_ = c.EnsureRunning(creds.Set{})
		reachedRun(t, "warn", rr)
		said := c.Err.(*bytes.Buffer).String()
		if strings.Contains(said, "boot assets verified") {
			t.Errorf("hull's record verified the boot assets:\n%s", said)
		}
		if !strings.Contains(said, "no registry in unit tests") {
			t.Errorf("warn does not say why the registry manifest could not be read:\n%s", said)
		}
	}
}

// writeRecord writes hull's provenance.json beside the assets.
func writeRecord(t *testing.T, assets runtime.BootAssets, digest string, files verify.BootDigests) {
	t.Helper()
	var entries []string
	for name, d := range files {
		entries = append(entries, fmt.Sprintf(`%q:{"sha256":%q,"size":1}`, name, strings.TrimPrefix(d, "sha256:")))
	}
	body := fmt.Sprintf(`{"ref":"ghcr.io/nofireai/hull-assets:darwin-arm64","digest":%q,"verifiedDigest":%q,"files":{%s}}`,
		digest, digest, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(filepath.Dir(assets.Kernel), "provenance.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// BRIG_VERIFY=off skips the signature and the digest checks, and says so on
// one line that names BRIG_VERIFY=off once. A modified kernel boots.
func TestVerifyOffSkipsTheDigestCheckOnOneLine(t *testing.T) {
	registryServes(t, published())
	c, rr := digestRun(t, verify.Off, writeBootAssets(t, modifiedKernel, publishedInitrd, false))

	_ = c.EnsureRunning(creds.Set{})

	reachedRun(t, "off", rr)
	said := c.Err.(*bytes.Buffer).String()
	if got := strings.Count(said, "BRIG_VERIFY=off"); got != 1 {
		t.Errorf("BRIG_VERIFY=off stated %d times, want 1:\n%s", got, said)
	}
	for _, line := range strings.Split(said, "\n") {
		if strings.Contains(line, "BRIG_VERIFY=off") && !strings.Contains(line, "digest") {
			t.Errorf("the off line does not say the digest check is skipped: %q", line)
		}
	}
}

// BRIG_BOOT_ASSETS_REF pins the bundle. One under brig's boot-asset registry
// verifies and binds its own digest, and the manifest is read for that
// reference. Any other reference has no signature of ours, so nothing binds
// and the summary does not name the boot assets.
func TestAnOverriddenBundleReferenceBindsOnlyItsOwnDigest(t *testing.T) {
	asked := ""
	was := registryDigests
	registryDigests = func(ref, digest string) (verify.BootDigests, error) {
		asked = ref
		return published(), nil
	}
	t.Cleanup(func() { registryDigests = was })

	c, rr := digestRun(t, verify.Warn, writeBootAssets(t, publishedKernel, publishedInitrd, false))
	t.Setenv("BRIG_BOOT_ASSETS_REF", "docker.io/someone/kernels:latest")
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "a foreign reference under warn", rr)
	if asked != "" {
		t.Errorf("a reference brig does not publish had its manifest read at %s", asked)
	}
	if said := c.Err.(*bytes.Buffer).String(); strings.Contains(said, "boot assets verified") {
		t.Errorf("a reference with no signature of ours was called verified:\n%s", said)
	}

	const pinned = "ghcr.io/nofireai/hull-assets:darwin-arm64-0.1.5"
	c, rr = digestRun(t, verify.Warn, writeBootAssets(t, publishedKernel, publishedInitrd, false))
	t.Setenv("BRIG_BOOT_ASSETS_REF", pinned)
	_ = c.EnsureRunning(creds.Set{})
	reachedRun(t, "a pinned reference under warn", rr)
	if asked != pinned {
		t.Errorf("the manifest was read for %q, want the pinned %q", asked, pinned)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "image and boot assets verified") {
		t.Errorf("the pinned bundle matched and the summary does not say so:\n%s", said)
	}
}
