package wrap

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// The bundle the tag named before a publish moved it: files of its own, and
// hull's record of them.
var (
	olderKernel = []byte("the kernel the previous bundle lists")
	olderInitrd = []byte("the initrd the previous bundle lists")
	olderDigest = "sha256:" + strings.Repeat("4", 64)
)

func olderBundle() verify.BootDigests {
	return verify.BootDigests{"Image": digestOf(olderKernel), "container-initrd": digestOf(olderInitrd)}
}

// fetchesPublished makes a replace put the published bundle over the files,
// as hull or oras would, fetching the digest that verified.
func fetchesPublished(t *testing.T, rr *resolvingRuntime) {
	t.Helper()
	rr.replaced = func() {
		for path, b := range map[string][]byte{rr.assets.Kernel: publishedKernel, rr.assets.Initrd: publishedInitrd} {
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		writeRecord(t, rr.assets, testDigest, published())
	}
}

func verifiedRef() string {
	return verify.RefWithDigest(runtime.BootAssetsRef(), testDigest)
}

// A first run downloads the bundle whose signature verified, by its digest,
// not whatever the tag names by the time the download starts.
func TestTheFirstFetchIsTheDigestThatVerified(t *testing.T) {
	c, rr := digestRun(t, verify.Warn, givenAssets(t))

	_ = c.EnsureRunning(creds.Set{})

	reachedRun(t, "warn", rr)
	if len(rr.fetches) != 1 || rr.fetches[0] != (runtime.BootFetch{Ref: verifiedRef()}) {
		t.Errorf("the resolve was asked for %+v, want one fetch of %s", rr.fetches, verifiedRef())
	}
	if !strings.Contains(verifiedRef(), "@"+testDigest) || strings.Contains(verifiedRef(), ":"+"linux") ||
		strings.Contains(verifiedRef(), ":darwin") {
		t.Errorf("the pinned reference still names the tag: %s", verifiedRef())
	}
}

// Files an earlier fetch left, of the bundle the tag named before a publish,
// are fetched again by the digest that verified, and the run boots. Every run
// after every publish used to refuse until somebody deleted them.
func TestAnOlderBundleIsFetchedAgainNotRefused(t *testing.T) {
	registryServes(t, published())
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		assets := writeBootAssets(t, olderKernel, olderInitrd, false)
		writeRecord(t, assets, olderDigest, olderBundle())
		c, rr := digestRun(t, mode, assets)
		fetchesPublished(t, rr)

		err := c.EnsureRunning(creds.Set{})

		reachedRun(t, string(mode), rr)
		if len(rr.fetches) != 2 || rr.fetches[1] != (runtime.BootFetch{Ref: verifiedRef(), Replace: true}) {
			t.Errorf("%s: the older bundle was not fetched over by the digest that verified: %+v (err %v)",
				mode, rr.fetches, err)
		}
		if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "image and boot assets verified") {
			t.Errorf("%s: the summary does not name the bundle fetched again:\n%s", mode, said)
		}
	}
}

// Files that match no record are somebody's change, not an older bundle. They
// refuse, and nothing is fetched over them to hide it.
func TestFilesThatMatchNoRecordRefuseWithoutAFetch(t *testing.T) {
	registryServes(t, published())
	assets := writeBootAssets(t, modifiedKernel, olderInitrd, false)
	writeRecord(t, assets, olderDigest, olderBundle())
	c, rr := digestRun(t, verify.Warn, assets)
	fetchesPublished(t, rr)

	err := c.EnsureRunning(creds.Set{})

	refused(t, "warn", err, rr)
	for _, f := range rr.fetches {
		if f.Replace {
			t.Errorf("changed files were fetched over: %+v", rr.fetches)
		}
	}
	if b, _ := os.ReadFile(assets.Kernel); !bytes.Equal(b, modifiedKernel) {
		t.Error("the changed kernel is gone, so nobody can look at it")
	}
}

// A directory named in BRIG_BOOT_ASSETS is somebody's, and brig does not
// fetch over it even when it holds an older bundle.
func TestANamedDirectoryIsNeverFetchedOver(t *testing.T) {
	registryServes(t, published())
	assets := writeBootAssets(t, olderKernel, olderInitrd, true)
	writeRecord(t, assets, olderDigest, olderBundle())
	c, rr := digestRun(t, verify.Warn, assets)
	fetchesPublished(t, rr)

	_ = c.EnsureRunning(creds.Set{})

	reachedRun(t, "warn", rr)
	if len(rr.fetches) != 1 {
		t.Errorf("a named directory was fetched over: %+v", rr.fetches)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "not the bundle that verified") {
		t.Errorf("warn did not state the difference:\n%s", said)
	}
}

// A fetch of the bundle that verified that fails leaves the older bundle, and
// the run refuses rather than boot it.
func TestAFailedFetchOfTheVerifiedBundleRefuses(t *testing.T) {
	registryServes(t, published())
	assets := writeBootAssets(t, olderKernel, olderInitrd, false)
	writeRecord(t, assets, olderDigest, olderBundle())
	c, rr := digestRun(t, verify.Warn, assets)
	rr.replaced = func() { rr.err = errors.New("oras pull: 503") }

	err := c.EnsureRunning(creds.Set{})

	refused(t, "warn", err, rr)
	if !strings.Contains(err.Error(), "older bundle") || !strings.Contains(err.Error(), "oras pull: 503") {
		t.Errorf("the refusal does not say the fetch of the verified bundle failed: %v", err)
	}
}
