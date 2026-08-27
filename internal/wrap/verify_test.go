package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/ttytest"
	"github.com/brig-sh/brig/internal/verify"
)

// verifyRuntime is the seam the verify decision reaches through: whether the
// runtime boots by digest, and what digest it already holds. Everything else
// panics through the embedded nil interface, which is louder than a stub that
// answers questions it was never asked.
type verifyRuntime struct {
	runtime.Runtime
	pins  bool
	local string
}

func (v verifyRuntime) Kind() string                       { return "hull" }
func (v verifyRuntime) PinsDigest() bool                   { return v.pins }
func (v verifyRuntime) LocalDigest(string) (string, error) { return v.local, nil }

// The little that EnsureRunning asks before it reaches the checks. Nothing is
// running and removing is a no-op, which is the state a first boot starts in.
// Run is deliberately absent: a test that got that far would be booting, and
// every case here is meant to refuse before then.
func (v verifyRuntime) Running(string) bool { return false }
func (v verifyRuntime) Remove(string) error { return nil }

// Stops the boot at the point a real runtime would start doing work, so a
// test can drive EnsureRunning through the checks without booting anything.
func (v verifyRuntime) Run(runtime.RunSpec) error { return errors.New("stub runtime: not booting") }

func verifyConfig(t *testing.T, image string, mode verify.Mode) *Config {
	t.Helper()
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Image = image
	c.Verify = mode
	c.VerifyPolicy = verify.DefaultPolicy()
	// No cosign on the machine running the tests, which is itself one of the
	// cases worth pinning.
	c.VerifyPolicy.Cosign = "cosign-that-does-not-exist"
	// These cases are about the tag path, which is the runtime that does not
	// pin a digest. The digest path has its own cases below.
	c.Runtime = verifyRuntime{pins: false}
	return c
}

// fakeCosign writes a cosign stand-in that resolves every reference to digest
// and either passes or fails verification, so the digest decision runs without
// a network. It mirrors the stub script script/smoke.sh installs.
func fakeCosign(t *testing.T, digest string, verifyFails bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cosign")
	script := "#!/bin/bash\n" +
		"case \"$1\" in\n" +
		"  triangulate) echo \"ghcr.io/example/image:$(echo " + digest + " | sed 's/:/-/').sig\" ;;\n"
	if verifyFails {
		script += "  verify) echo 'Error: no matching signatures' >&2; exit 1 ;;\n"
	} else {
		script += "  verify) exit 0 ;;\n"
	}
	script += "esac\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// digestConfig is verifyConfig for the runtime that boots by digest, with a
// working fake cosign and a chosen local-store digest.
func digestConfig(t *testing.T, image string, mode verify.Mode, cosign, local string) *Config {
	t.Helper()
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Image = image
	c.Verify = mode
	c.VerifyPolicy = verify.DefaultPolicy()
	c.VerifyPolicy.Cosign = cosign
	c.Runtime = verifyRuntime{pins: true, local: local}
	return c
}

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// The digest brig resolved and verified is the digest it records to boot, and
// the line names it.
func TestVerifyDigestPinsAndReportsTheDigest(t *testing.T) {
	cosign := fakeCosign(t, testDigest, false)
	c := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn, cosign, testDigest)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a verified image was refused: %v", err)
	}
	if c.BootDigest != testDigest {
		t.Errorf("BootDigest = %q, want the resolved digest so EnsureRunning boots it", c.BootDigest)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, testDigest) {
		t.Errorf("the message did not name the digest: %q", said)
	}
}

// Our own tag over a local copy that is not the verified digest stops, and on
// a boot it boots the verified digest rather than the copy on disk.
func TestVerifyDigestFirstPartyMismatchFailsClosed(t *testing.T) {
	other := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	cosign := fakeCosign(t, testDigest, false)
	c := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn, cosign, other)
	err := c.verifyImage()
	if err == nil {
		t.Fatal("a first-party mismatch booted without stopping (no terminal to say yes)")
	}
	if c.BootDigest != testDigest {
		t.Errorf("a yes would boot %q, not the verified digest", c.BootDigest)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "NOT the") {
		t.Errorf("the warning did not say the local copy is not the verified one: %q", said)
	}
}

// A third party's tag over a differing local copy warns and boots.
func TestVerifyDigestThirdPartyBootsTheTagWithoutCosign(t *testing.T) {
	// A cosign that records every invocation. For an image brig did not publish
	// there is nothing to verify, so there must be nothing to run either: the
	// tag path never cost a registry round trip, and the digest path must not
	// start charging one.
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	cosign := filepath.Join(dir, "cosign")
	if err := os.WriteFile(cosign, []byte("#!/bin/bash\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := digestConfig(t, "docker.io/library/ubuntu:24.04", verify.Warn, cosign, "")
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a third-party image was refused: %v", err)
	}
	if c.BootDigest != "" {
		t.Errorf("a third-party image boots by tag, nothing should be pinned: %q", c.BootDigest)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("cosign was invoked for an image it has nothing to say about")
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "not published by brig-sh") {
		t.Errorf("the warning did not say whose image it is: %q", said)
	}
}

// A registry that cannot be reached resolves nothing, so the tag boots unpinned
// and only require refuses. cosign-that-does-not-exist stands in for a cosign
// that is present but whose resolve fails, by being absent -- which lands on
// NoTooling, the same warn-and-boot row -- so here a real failing resolve is
// used instead.
func TestVerifyDigestUnreachableRegistryRefusesWithoutATerminal(t *testing.T) {
	// A cosign whose triangulate always fails: a resolve that cannot reach the
	// registry. Before the digest check existed the same outage failed inside
	// `cosign verify`, which stopped the boot and asked; a network failure must
	// not be the thing that turns the default mode into "boot it unchecked".
	dir := t.TempDir()
	cosign := filepath.Join(dir, "cosign")
	if err := os.WriteFile(cosign, []byte("#!/bin/bash\ncase \"$1\" in triangulate) echo 'dial tcp: timeout' >&2; exit 1 ;; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn, cosign, "")
	if err := c.verifyImage(); err == nil {
		t.Fatal("an unreachable registry booted a first-party image unchecked, with no terminal to say yes")
	}
	if c.BootDigest != "" {
		t.Errorf("nothing resolved, so nothing should be pinned: %q", c.BootDigest)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "cannot reach") && !strings.Contains(said, "could not be verified") {
		t.Errorf("the refusal did not say the registry was the problem: %q", said)
	}
	c = digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Require, cosign, "")
	if err := c.verifyImage(); err == nil {
		t.Error("BRIG_VERIFY=require booted an image it could not resolve")
	}
}

// A bring-your-own image is reported and booted. Blocking it would make the
// feature useless, and using one is a supported way to run brig.
func TestVerifyWarnsButBootsAThirdPartyImage(t *testing.T) {
	c := verifyConfig(t, "docker.io/library/ubuntu:24.04", verify.Warn)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a third-party image was refused: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "not published by brig-sh") {
		t.Errorf("nothing was said about it: %q", said)
	}
}

// Without cosign nothing can be checked, which is a different thing from a
// bad signature and must not stop a boot by default.
func TestVerifyWarnsButBootsWithoutCosign(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a missing cosign blocked the boot: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "cosign is not installed") {
		t.Errorf("nothing was said about it: %q", said)
	}
}

// require is for the caller who would rather not boot than not know.
func TestVerifyRequireRefusesWhatItCannotCheck(t *testing.T) {
	for _, image := range []string{
		"docker.io/library/ubuntu:24.04",
		"ghcr.io/brig-sh/claude-code:arm64",
	} {
		c := verifyConfig(t, image, verify.Require)
		if err := c.verifyImage(); err == nil {
			t.Errorf("BRIG_VERIFY=require booted %s unchecked", image)
		}
	}
}

func TestVerifyOffChecksNothing(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("BRIG_VERIFY=off still checked: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); said != "" {
		t.Errorf("BRIG_VERIFY=off still said something: %q", said)
	}
}

// A failing signature on an image claiming to be ours is the one case that
// stops. With no terminal there is nobody to ask, and assuming yes would turn
// the only check that stops into one that does not.
//
// The assertion on the "not a terminal" line is what makes this test about
// that branch. Without it the test passed for the wrong reason: stdin under
// `go test` is /dev/null, the old IsTerminal called that a terminal, and the
// refusal came from the read hitting EOF rather than from there being nobody
// to ask -- so a confirm() mutated to fail open still passed. It goes red now.
func TestVerifyRefusesAFailedSignatureWithNoTerminal(t *testing.T) {
	// Said out loud rather than assumed: stdin under `go test` is not a
	// terminal, and this test is about what happens when it is not.
	if IsTerminal(os.Stdin) {
		t.Skip("this test needs a stdin that is not a terminal")
	}
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.VerifyPolicy.Cosign = "false" // present, and always exits non-zero
	err := c.verifyImage()
	if err == nil {
		t.Fatal("a failed signature was booted without asking")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error = %v", err)
	}
	// Whatever the refusal was -- no terminal, or an answer of no -- it names
	// the setting that lets a caller proceed deliberately.
	if !strings.Contains(err.Error(), "BRIG_VERIFY=off") {
		t.Errorf("the refusal did not name the override: %v", err)
	}
	said := c.Err.(*bytes.Buffer).String()
	if !strings.Contains(said, "DID NOT VERIFY") {
		t.Errorf("the warning did not say what happened: %q", said)
	}
	if !strings.Contains(said, "not a terminal") {
		t.Errorf("the refusal did not come from the no-terminal branch: %q", said)
	}
}

// The same refusal, from the other direction: /dev/null is a character device,
// which is what the old check tested and why it counted as somebody to ask.
func TestVerifyRefusesAFailedSignatureWithStdinOnDevNull(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	old := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = old }()

	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.VerifyPolicy.Cosign = "false"
	if err := c.verifyImage(); err == nil {
		t.Fatal("a failed signature was booted with stdin on /dev/null")
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "not a terminal") {
		t.Errorf("/dev/null was taken for somebody to ask: %q", said)
	}
}

// A caller with no terminal of its own is taken at its word, even sitting on a
// real one. This is brigd's case: its stdin may well be a terminal, because it
// was started from a shell, but that terminal belongs to whoever started the
// daemon and not to the client whose request raised the question -- so asking
// there puts a question in front of nobody while the client waits for an
// answer that cannot come.
func TestVerifyDoesNotAskWhenTheCallerHasNoTerminal(t *testing.T) {
	// A real terminal on stdin, with an answer already waiting on it: the
	// point is that neither is looked at.
	master := ttytest.AsStdin(t)
	if _, err := master.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.VerifyPolicy.Cosign = "false" // present, and always exits non-zero
	c.NoTerminal = true

	err := c.verifyImage()
	if err == nil {
		t.Fatal("a failed signature was booted on a terminal the caller does not have")
	}
	if !strings.Contains(err.Error(), "BRIG_VERIFY=off") {
		t.Errorf("the refusal did not name the override: %v", err)
	}
	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "Boot it anyway?") {
		t.Errorf("the question was asked anyway: %q", said)
	}
	if !strings.Contains(said, "nobody to ask") {
		t.Errorf("the refusal did not say why nobody was asked: %q", said)
	}
}

// And with a real terminal the question is actually asked and the answer
// actually read. This is the half the no-terminal tests cannot prove: a
// confirm() that always refused would satisfy every assertion above.
func TestVerifyAsksWhenThereIsSomebodyToAsk(t *testing.T) {
	for _, tc := range []struct {
		answer string
		booted bool
	}{{"y\n", true}, {"n\n", false}, {"\n", false}} {
		master := ttytest.AsStdin(t)
		if _, err := master.WriteString(tc.answer); err != nil {
			t.Fatal(err)
		}
		c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
		c.VerifyPolicy.Cosign = "false"
		err := c.verifyImage()
		if booted := err == nil; booted != tc.booted {
			t.Errorf("answering %q: booted = %v, want %v (err %v)", tc.answer, booted, tc.booted, err)
		}
		if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "Boot it anyway?") {
			t.Errorf("answering %q: the question was never asked: %q", tc.answer, said)
		}
	}
}

// TestVerifyTagPathSaysWhyItIsNotPinned: a runtime that cannot boot by digest
// gets the tag check, and the user is told that is what happened and what
// would change it. Silence here would read as "verified", which on this path
// means less than it does on the other.
func TestVerifyTagPathSaysWhyItIsNotPinned(t *testing.T) {
	c := verifyConfig(t, "docker.io/library/ubuntu:24.04", verify.Warn)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("the tag path refused a third-party image: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "cannot boot by digest") {
		t.Errorf("nothing said why the digest was not pinned: %q", said)
	}
}

// Every refusal names a way past it. A client that surfaces the error and
// drops the warnings -- which is the ordinary client shape, and what brigd
// does -- otherwise leaves the user with an abort and nothing to act on. The
// unreachable-registry case said only to try again with the registry
// reachable, which is not something a user in a sinkhole or behind a captive
// portal can do.
func TestEveryVerifyRefusalNamesAWayForward(t *testing.T) {
	for _, tc := range []struct {
		what string
		pins bool
	}{
		// The digest path. A cosign that exits non-zero fails the resolve
		// before it ever reaches the signature, which is Unresolved: could not
		// check, rather than failed. That is the case that named nothing.
		{"the registry could not be reached", true},
		// And the tag path, so the case that always named its override keeps
		// doing so.
		{"the tag could not be verified", false},
	} {
		c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
		c.Runtime = verifyRuntime{pins: tc.pins}
		c.VerifyPolicy.Cosign = "false"
		err := c.verifyImage()
		if err == nil {
			t.Fatalf("%s: booted rather than refusing", tc.what)
		}
		if !strings.Contains(err.Error(), "BRIG_VERIFY=off") {
			t.Errorf("%s: the refusal names no way forward: %v", tc.what, err)
		}
	}
}

// A genericBoot profile boots a kernel and an initrd brig downloads, and the
// initrd carries the in-guest agent. brig verified the image and not the
// kernel that runs it, which is the more privileged of the two. The bundle is
// signed, so there was a check to make and brig was not making it.
func TestGenericBootVerifiesTheBundle(t *testing.T) {
	// Require refuses what it cannot check, which is the cheapest way to prove
	// the bundle is checked at all: with no cosign there is nothing to check,
	// so a profile that boots a bundle must refuse and a profile that does not
	// must proceed.
	boots := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Require)
	boots.Runtime = verifyRuntime{pins: false}
	boots.Profile.GenericBoot = true
	err := boots.verifyBootAssets()
	if err == nil {
		t.Error("a genericBoot profile booted a bundle that could not be checked")
	}

	// A profile that boots its own image has no bundle, so there is nothing to
	// check and nothing to refuse.
	plain := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Require)
	plain.Runtime = verifyRuntime{pins: false}
	plain.Profile.GenericBoot = false
	if err := plain.verifyBootAssets(); err != nil {
		t.Errorf("a profile with no bundle was refused: %v", err)
	}

	// Off means off, for the bundle as for the image.
	skipped := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	skipped.Runtime = verifyRuntime{pins: false}
	skipped.Profile.GenericBoot = true
	if err := skipped.verifyBootAssets(); err != nil {
		t.Errorf("BRIG_VERIFY=off still checked the bundle: %v", err)
	}
}

// And the check is actually reached on the way to a boot. Calling
// verifyBootAssets directly proves what it decides, not that anything asks it:
// with the call removed from EnsureRunning every test above still passed, which
// is the whole failure mode this pins.
func TestEnsureRunningVerifiesTheBundleBeforeBooting(t *testing.T) {
	// Warn rather than Require, because under Require the image check refuses
	// first and the run never reaches the bundle. What is being pinned is that
	// the bundle is looked at on the way to a boot, and under Warn that shows
	// up as a line about it rather than as a refusal.
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.Runtime = verifyRuntime{pins: false}
	c.Profile.GenericBoot = true
	c.Workspace = t.TempDir()
	c.Cwd = c.Workspace

	_ = c.EnsureRunning(creds.Set{})

	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "boot assets") {
		t.Errorf("the run never looked at the kernel it was about to boot:\n%s", said)
	}
}
