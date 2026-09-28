package wrap

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// resolvingRuntime is verifyRuntime with a BootResolver. It records what the
// run had said by the time it was asked, and the spec Run received.
type resolvingRuntime struct {
	verifyRuntime
	assets runtime.BootAssets
	err    error
	// kind stands in for Runtime.Kind. Empty is hull, as verifyRuntime says.
	kind string

	said      *bytes.Buffer
	asked     int
	atResolve string
	booted    *runtime.RunSpec
	// fetches are what each resolve was asked to fetch, and replaced, when
	// set, stands in for a fetch over the files there.
	fetches  []runtime.BootFetch
	replaced func()
}

// A stand-in that drifted from BootResolver would pass every test here by
// not resolving at all.
var _ runtime.BootResolver = (*resolvingRuntime)(nil)

func (r *resolvingRuntime) Kind() string {
	if r.kind != "" {
		return r.kind
	}
	return r.verifyRuntime.Kind()
}

func (r *resolvingRuntime) ResolveBootAssets(fetch runtime.BootFetch, _, _ io.Writer) (runtime.BootAssets, error) {
	r.asked++
	r.atResolve = r.said.String()
	r.fetches = append(r.fetches, fetch)
	if fetch.Replace && r.replaced != nil {
		r.replaced()
	}
	return r.assets, r.err
}

func (r *resolvingRuntime) Run(spec runtime.RunSpec) error {
	r.booted = &spec
	return errors.New("stub runtime: not booting")
}

// givenAssets is a kernel and an initrd on disk whose digests the stand-in
// registry lists, so the digest check passes and the summary names them.
func givenAssets(t *testing.T) runtime.BootAssets {
	t.Helper()
	registryServes(t, published())
	return writeBootAssets(t, publishedKernel, publishedInitrd, false)
}

// resolvingConfig is a genericBoot run whose image and bundle both verify, so
// the summary has something to say and its place in the output is testable.
func resolvingConfig(t *testing.T, mode verify.Mode, rr *resolvingRuntime) *Config {
	t.Helper()
	c := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", mode, fakeCosign(t, testDigest, false), testDigest)
	rr.verifyRuntime = verifyRuntime{pins: true, local: testDigest}
	rr.said = c.Err.(*bytes.Buffer)
	c.Runtime = rr
	c.Profile.GenericBoot = true
	c.Workspace = t.TempDir()
	c.Cwd = c.Workspace
	return c
}

// The verify summary follows the resolve. It used to print before the adapter
// fetched the kernel and initrd, so "boot assets verified" was on screen
// before the files it spoke for were on disk. #234 needs them here to compare
// their digests before the summary.
func TestEnsureRunningResolvesBootAssetsBeforeTheSummary(t *testing.T) {
	rr := &resolvingRuntime{assets: givenAssets(t)}
	c := resolvingConfig(t, verify.Warn, rr)

	_ = c.EnsureRunning(creds.Set{})

	if rr.asked != 1 {
		t.Fatalf("the boot assets were resolved %d times before the boot, want once", rr.asked)
	}
	if strings.Contains(rr.atResolve, "verified") {
		t.Errorf("the summary printed before the boot assets were resolved:\n%s", rr.atResolve)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "image and boot assets verified") {
		t.Errorf("no summary after the resolve, so the ordering above proves nothing:\n%s", said)
	}
	if rr.booted == nil {
		t.Fatal("the run never reached the runtime")
	}
	if rr.booted.BootAssets != rr.assets {
		t.Errorf("Run got boot assets %+v, want the resolved %+v", rr.booted.BootAssets, rr.assets)
	}
}

// A resolve that fails stops the run before the summary and before Run. It is
// a failure to start, as it was when the adapter fetched, and not a
// verification refusal with its own exit code.
func TestEnsureRunningStopsWhenTheResolveFails(t *testing.T) {
	rr := &resolvingRuntime{err: errors.New("fetching them failed: no credentials")}
	c := resolvingConfig(t, verify.Warn, rr)

	err := c.EnsureRunning(creds.Set{})

	if err == nil {
		t.Fatal("a run with no kernel went ahead")
	}
	if !strings.Contains(err.Error(), "could not start the sandbox") || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("the error does not read as it did from Run: %v", err)
	}
	var vr *VerifyRefusedError
	if errors.As(err, &vr) {
		t.Errorf("a failed fetch reads as a verification refusal: %v", err)
	}
	if rr.booted != nil {
		t.Error("Run was called with no boot assets")
	}
	if said := c.Err.(*bytes.Buffer).String(); strings.Contains(said, "verified") {
		t.Errorf("the summary printed for boot assets that never arrived:\n%s", said)
	}
}

// BRIG_VERIFY=off still boots a kernel, so the resolve is keyed on the
// profile and not on verification.
func TestEnsureRunningResolvesBootAssetsWithVerifyOff(t *testing.T) {
	rr := &resolvingRuntime{assets: givenAssets(t)}
	c := resolvingConfig(t, verify.Off, rr)

	_ = c.EnsureRunning(creds.Set{})

	if rr.booted == nil || rr.booted.BootAssets != rr.assets {
		t.Errorf("with BRIG_VERIFY=off Run did not get the resolved boot assets: %+v", rr.booted)
	}
}

// A profile that boots its own image has no kernel to find, and nothing is
// fetched for it.
func TestEnsureRunningDoesNotResolveWithoutGenericBoot(t *testing.T) {
	rr := &resolvingRuntime{assets: givenAssets(t)}
	c := resolvingConfig(t, verify.Warn, rr)
	c.Profile.GenericBoot = false

	_ = c.EnsureRunning(creds.Set{})

	if rr.asked != 0 {
		t.Errorf("resolved boot assets for a profile that boots its own image")
	}
	if rr.booted != nil && rr.booted.BootAssets != (runtime.BootAssets{}) {
		t.Errorf("Run got boot assets for a profile that boots its own image: %+v", rr.booted.BootAssets)
	}
}
