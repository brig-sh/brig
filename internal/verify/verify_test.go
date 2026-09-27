package verify

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func stub(t *testing.T, found bool, output string, err error) {
	t.Helper()
	origLook, origRun := lookPath, run
	t.Cleanup(func() { lookPath, run = origLook, origRun })
	lookPath = func(string) (string, error) {
		if !found {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/cosign", nil
	}
	run = func(string, ...string) (string, error) { return output, err }
}

func TestImageDecisionTable(t *testing.T) {
	p := DefaultPolicy()

	// An image we did not publish carries no signature of ours, which is the
	// expected state for a bring-your-own image rather than a problem.
	stub(t, true, "", nil)
	if got := p.Image("docker.io/library/ubuntu:24.04"); got.Outcome != NotOurs {
		t.Errorf("third-party image outcome = %v, want NotOurs", got.Outcome)
	}

	// Ours, and cosign agrees.
	stub(t, true, "ok", nil)
	if got := p.Image("ghcr.io/brig-sh/claude-code:arm64"); got.Outcome != Verified {
		t.Errorf("signed image outcome = %v, want Verified", got.Outcome)
	}

	// Ours, and cosign does not agree: the one case with no innocent reading.
	stub(t, true, "Error: no matching signatures\n", errors.New("exit 1"))
	got := p.Image("ghcr.io/brig-sh/claude-code:arm64")
	if got.Outcome != Failed {
		t.Fatalf("bad signature outcome = %v, want Failed", got.Outcome)
	}
	if !strings.Contains(got.Detail, "no matching signatures") {
		t.Errorf("detail = %q, want cosign's own reason", got.Detail)
	}

	// No cosign: nothing could be checked, which is not the same as a bad
	// signature and must not read like one.
	stub(t, false, "", nil)
	if got := p.Image("ghcr.io/brig-sh/claude-code:arm64"); got.Outcome != NoTooling {
		t.Errorf("missing cosign outcome = %v, want NoTooling", got.Outcome)
	}
}

// digestStub drives the digest path: triangulate resolves a reference to a
// digest, verify checks it. Either can be told to fail, which is how the
// unreachable-registry and bad-signature rows are reached without a network.
func digestStub(t *testing.T, triangulate string, triErr error, verifyErr error) {
	t.Helper()
	origLook, origRun := lookPath, run
	t.Cleanup(func() { lookPath, run = origLook, origRun })
	lookPath = func(string) (string, error) { return "/usr/bin/cosign", nil }
	run = func(_ string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "triangulate":
			return triangulate, triErr
		case "verify":
			if verifyErr != nil {
				return "Error: no matching signatures\n", verifyErr
			}
			return "", nil
		}
		return "", nil
	}
}

// digestHex is a stand-in for a real sha256, and sigTag spells the reference
// cosign triangulate prints for it -- <repo>:sha256-<hex>.sig -- so the tests
// resolve a digest the way the code parses one.
const digestHex = "1111111111111111111111111111111111111111111111111111111111111111"

func sigTag(repo string) string { return repo + ":sha256-" + digestHex + ".sig\n" }

// The whole point of the path: the digest resolved is the digest verified is
// the digest reported, and a local copy that matches raises nothing.
func TestVerifyResolvesVerifiesAndPinsAMatchingDigest(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, sigTag("ghcr.io/brig-sh/claude-code"), nil, nil)

	// The local store holds the very digest the registry resolved to.
	got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "sha256:"+digestHex)
	if got.Outcome != Verified {
		t.Fatalf("outcome = %v, want Verified", got.Outcome)
	}
	if got.Digest != "sha256:"+digestHex {
		t.Errorf("digest = %q, want the resolved one", got.Digest)
	}
	if !strings.Contains(got.Message(), got.Digest) {
		t.Errorf("the success message does not name the digest: %q", got.Message())
	}
	// No local copy at all is the clean first boot, and still verifies.
	if got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", ""); got.Outcome != Verified {
		t.Errorf("a first boot with nothing cached = %v, want Verified", got.Outcome)
	}
}

// Our own tag over a local copy that is not the digest that verified is the
// signature-failure row: it stops. Ours is what says so.
func TestVerifyMismatchOnAFirstPartyImageFailsClosed(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, sigTag("ghcr.io/brig-sh/claude-code"), nil, nil)

	got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "sha256:"+strings.Repeat("b", 64))
	if got.Outcome != Mismatch {
		t.Fatalf("outcome = %v, want Mismatch", got.Outcome)
	}
	if !got.Ours {
		t.Error("a mismatch on our own image must be marked Ours so it stops")
	}
	if got.Digest != "sha256:"+digestHex || got.Local != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("the result names the wrong digests: verified %q local %q", got.Digest, got.Local)
	}
}

// A third party's tag over a differing local copy is a warning, not a stop:
// same fact, lighter weight, exactly as NotOurs is lighter than Failed.
// A third party's image carries no signature of ours to check, so there is
// nothing to resolve either: resolving costs a registry round trip on every
// boot, stalls a laptop that is off the network, and buys a pin brig cannot
// vouch for. The answer is NotOurs before cosign is so much as looked up,
// which is what the tag path always did.
func TestVerifyThirdPartyMakesNoCosignCall(t *testing.T) {
	p := DefaultPolicy()
	origLook, origRun := lookPath, run
	t.Cleanup(func() { lookPath, run = origLook, origRun })
	lookPath = func(string) (string, error) { return "/usr/bin/cosign", nil }
	calls := 0
	run = func(_ string, args ...string) (string, error) {
		calls++
		return "", nil
	}

	got := p.Verify("docker.io/library/ubuntu:24.04", "")
	if got.Outcome != NotOurs {
		t.Fatalf("outcome = %v, want NotOurs", got.Outcome)
	}
	if got.Digest != "" {
		t.Errorf("a third-party image is booted by tag, so no digest should be resolved: %q", got.Digest)
	}
	if calls != 0 {
		t.Errorf("cosign was invoked %d time(s) for an image it has nothing to say about", calls)
	}
}

// A registry that cannot be reached resolves no digest, which is "could not
// check", not "failed": it must not read like a bad signature.
func TestVerifyUnreachableRegistryIsUnresolvedNotFailed(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, "", errors.New("dial tcp: i/o timeout"), nil)

	got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "sha256:"+digestHex)
	if got.Outcome != Unresolved {
		t.Fatalf("outcome = %v, want Unresolved", got.Outcome)
	}
	if got.Digest != "" {
		t.Errorf("nothing was resolved, so there is no digest to report: %q", got.Digest)
	}
}

// Our own image whose signature does not check out on the resolved digest is
// Failed, carrying cosign's own reason.
func TestVerifyFailedSignatureOnTheResolvedDigest(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, sigTag("ghcr.io/brig-sh/claude-code"), nil, errors.New("exit 1"))

	got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "")
	if got.Outcome != Failed {
		t.Fatalf("outcome = %v, want Failed", got.Outcome)
	}
	if !strings.Contains(got.Detail, "no matching signatures") {
		t.Errorf("detail = %q, want cosign's own reason", got.Detail)
	}
}

// triangulateCredsFailure is what cosign 3.1.3 prints when triangulate cannot
// run a credential helper: the deprecation notice first, then the error twice.
const triangulateCredsFailure = `Command "triangulate" is deprecated, triangulate will be removed in v4.0.0 (see https://github.com/sigstore/cosign/issues/4696). Instead, please use ` + "`oras discover` or `cosign tree`" + ` to show referring artifacts
Error: error getting credentials - err: exec: "docker-credential-desktop": executable file not found in $PATH, out: ` + "``" + `
error during command execution: error getting credentials - err: exec: "docker-credential-desktop": executable file not found in $PATH, out: ` + "``" + `
`

// A resolve that fails reports cosign's error. The deprecation notice cosign
// prints first is a warning about brig's own call, and reporting it hid the
// credential helper that could not run.
func TestVerifyUnresolvedReportsCosignsErrorNotItsWarning(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, triangulateCredsFailure, errors.New("exit status 1"), nil)

	got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", "")
	if got.Outcome != Unresolved {
		t.Fatalf("outcome = %v, want Unresolved", got.Outcome)
	}
	want := `error getting credentials - err: exec: "docker-credential-desktop": ` +
		"executable file not found in $PATH, out: ``"
	if got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
	if strings.Contains(got.Message(), "deprecated") {
		t.Errorf("the message reports cosign's warning: %q", got.Message())
	}
}

func TestCosignReason(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range []struct {
		name, out, want string
	}{
		{"the Error line", "Error: no matching signatures\n", "no matching signatures"},
		{"after a deprecation notice", triangulateCredsFailure,
			`error getting credentials - err: exec: "docker-credential-desktop": ` +
				"executable file not found in $PATH, out: ``"},
		{"after a WARNING line", "WARNING: skipping tlog verification\nError: no signatures found\n",
			"no signatures found"},
		{"only the execution line", "Warning: x\nerror during command execution: dial tcp: i/o timeout\n",
			"dial tcp: i/o timeout"},
		{"a line cosign did not frame", "\nsomething went wrong\n", "something went wrong"},
		{"only a notice", `Command "triangulate" is deprecated, use oras` + "\n", "exit status 1"},
		{"no output", "", "exit status 1"},
	} {
		if got := cosignReason(tc.out, exit); got != tc.want {
			t.Errorf("%s: cosignReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Without cosign neither the resolve nor the check can happen, which is a
// different thing from a bad signature.
func TestVerifyDigestWithoutCosignIsNoTooling(t *testing.T) {
	p := DefaultPolicy()
	origLook := lookPath
	t.Cleanup(func() { lookPath = origLook })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if got := p.Verify("ghcr.io/brig-sh/claude-code:arm64", ""); got.Outcome != NoTooling {
		t.Errorf("outcome = %v, want NoTooling", got.Outcome)
	}
}

func TestDigestFromOutput(t *testing.T) {
	want := "sha256:" + digestHex
	for _, in := range []string{
		// The default triangulate spelling, with a cosign warning ahead of it.
		"WARNING: the ref uses a tag, not a digest\nghcr.io/brig-sh/x:sha256-" + digestHex + ".sig\n",
		// The --type=digest spelling.
		"ghcr.io/brig-sh/x@sha256:" + digestHex + "\n",
	} {
		if got := digestFromOutput(in); got != want {
			t.Errorf("digestFromOutput(%q) = %q, want %q", in, got, want)
		}
	}
	if got := digestFromOutput("no digest here\n"); got != "" {
		t.Errorf("a line with no digest returned %q", got)
	}
}

func TestRefWithDigest(t *testing.T) {
	d := "sha256:" + digestHex
	cases := map[string]string{
		"ghcr.io/brig-sh/x:arm64":         "ghcr.io/brig-sh/x@" + d,
		"ghcr.io/brig-sh/x":               "ghcr.io/brig-sh/x@" + d,
		"ghcr.io:443/brig-sh/x:arm64":     "ghcr.io:443/brig-sh/x@" + d,
		"ghcr.io/brig-sh/x@sha256:oldold": "ghcr.io/brig-sh/x@" + d,
	}
	for in, want := range cases {
		if got := refWithDigest(in, d); got != want {
			t.Errorf("refWithDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

// The identity is anchored on the repository AND the workflow file, so a
// signature from any other workflow -- including another one in the same
// repository -- fails the check.
func TestDefaultPolicyPinsRepoAndWorkflow(t *testing.T) {
	p := DefaultPolicy()
	// The dots are escaped, so the strings pinned here are the regexp's own
	// spelling of them: an unescaped dot matches any character, which is how
	// "build-images0yml" passed a check meant to name one workflow file.
	for _, want := range []string{
		"brig-sh/community-images",
		`build-images\.yml`,
		"token.actions.githubusercontent.com",
	} {
		if !strings.Contains(p.Identity+p.Issuer, want) {
			t.Errorf("policy does not pin %q: %s %s", want, p.Identity, p.Issuer)
		}
	}
	if !strings.HasPrefix(p.Identity, "^") || !strings.HasSuffix(p.Identity, "$") {
		t.Errorf("identity regexp is not anchored at both ends: %s", p.Identity)
	}
}

// A cosign the lookup cannot find is two situations: a host without the tool,
// and a BRIG_COSIGN_BIN naming a path that holds nothing. The install hint
// applies to the first, and the second is a setting the reader can correct.
func TestCosignMissingSeparatesAnAbsentToolFromAWrongPath(t *testing.T) {
	shipped := DefaultPolicy().CosignMissing()
	if !strings.Contains(shipped, "not installed") || !strings.Contains(shipped, "brew install cosign") {
		t.Errorf("the default name loses the install hint: %q", shipped)
	}

	pointed := Policy{Cosign: "/opt/nowhere/cosign"}.CosignMissing()
	if !strings.Contains(pointed, "/opt/nowhere/cosign") || !strings.Contains(pointed, "BRIG_COSIGN_BIN") {
		t.Errorf("a pointed cosign does not name the path or the setting: %q", pointed)
	}
	if strings.Contains(pointed, "brew") {
		t.Errorf("a pointed cosign is told to install one: %q", pointed)
	}
}

func TestMessagesNameTheImageAndTheRemedy(t *testing.T) {
	cases := map[Outcome]string{
		NotOurs:   "not published by brig-sh",
		NoTooling: "brew install cosign",
		Failed:    "DID NOT VERIFY",
	}
	for outcome, want := range cases {
		msg := Result{Policy: DefaultPolicy(), Outcome: outcome, Image: "img",
			Detail: "d"}.Message()
		if !strings.Contains(msg, want) {
			t.Errorf("%v message = %q, want it to mention %q", outcome, msg, want)
		}
		if !strings.Contains(msg, "img") {
			t.Errorf("%v message does not name the image: %q", outcome, msg)
		}
	}
}

// A cosign that hangs must not hang the boot. The registry is on the other end
// of every call it makes, and a dial that never completes is the ordinary shape
// of an outage, so `run` gives up after cosignTimeout and the caller sees the
// same "could not be verified" it would see for any other failure to reach the
// registry.
func TestRunGivesUpOnAHangingCosign(t *testing.T) {
	orig := cosignTimeout
	t.Cleanup(func() { cosignTimeout = orig })
	cosignTimeout = 200 * time.Millisecond

	// The sleep runs as the shell's child, not in its place, so killing the
	// shell at the deadline leaves a process holding the output pipe. That is
	// the shape a real hang takes, and the one a naive kill-and-wait misses:
	// on a shell that execs a lone command the sleep would die with the shell
	// and the test would pass without proving anything.
	start := time.Now()
	_, err := run("/bin/sh", "-c", "sleep 5 & wait")
	if err == nil {
		t.Fatal("a hanging cosign returned no error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("run waited %v for a cosign that never answers", took)
	}
}

// TestParseModeStrictRefusesATypo pins that an unrecognised verify mode is an
// error, not a silent fall back to Warn. BRIG_VERIFY is the one setting the
// whole image-verification path leans on, so a typo must stop the run rather
// than quietly weaken the check to a warning.
func TestParseModeStrictRefusesATypo(t *testing.T) {
	for _, ok := range []string{"warn", "require", "strict", "off", "none", "0", "WARN", " require "} {
		if _, err := ParseModeStrict(ok); err != nil {
			t.Errorf("ParseModeStrict(%q) errored: %v", ok, err)
		}
	}
	if _, err := ParseModeStrict("requrie"); err == nil {
		t.Error("ParseModeStrict(requrie) did not refuse a typo")
	}
}

// The boot assets are a separate trust root from the guest images. They are
// published by another repository, under another registry prefix, by another
// workflow, so the image policy would refuse them as NotOurs and check
// nothing. The identity below was read off the signature the published bundle
// actually carries, not written from the repository layout.
func TestBootAssetsPolicyPinsItsOwnRepoAndWorkflow(t *testing.T) {
	p := BootAssetsPolicy()
	for _, want := range []string{
		"NOFireAI/hull-assets",
		`build-assets\.yml`,
		"token.actions.githubusercontent.com",
	} {
		if !strings.Contains(p.Identity+p.Issuer, want) {
			t.Errorf("policy does not pin %q: %s %s", want, p.Identity, p.Issuer)
		}
	}
	if !strings.HasPrefix(p.Identity, "^") || !strings.HasSuffix(p.Identity, "$") {
		t.Errorf("identity regexp is not anchored at both ends: %s", p.Identity)
	}
	// The bundle lives under a different prefix from the images, so a policy
	// carrying the image prefix would call the bundle NotOurs and check
	// nothing at all -- a check that always passes.
	if p.Registry == DefaultPolicy().Registry {
		t.Error("the boot assets share the image registry prefix, so one policy would check both")
	}
	if !strings.HasPrefix("ghcr.io/nofireai/hull-assets:linux-amd64", p.Registry) {
		t.Errorf("the published bundle is not under the policy's prefix %q", p.Registry)
	}
}

// A policy the user replaced cannot report itself in the same words as the one
// brig ships. The settings that replace it accept anything, including an
// identity expression that matches every certificate, and with that set brig
// printed the identical "signature verified" line it prints for a real check.
// The sentence a reader acts on has to differ when the thing it describes does.
func TestAReplacedPolicyDoesNotSayTheSameThing(t *testing.T) {
	shipped := DefaultPolicy()
	if shipped.Replaced() {
		t.Error("the shipped policy reports itself as replaced")
	}
	// brig ships more than one: the boot assets are their own trust root, and
	// a policy that is shipped must not read as one the user swapped in.
	if BootAssetsPolicy().Replaced() {
		t.Error("the shipped boot-assets policy reports itself as replaced")
	}

	for what, p := range map[string]Policy{
		"identity": {Registry: shipped.Registry, Identity: `.*`, Issuer: shipped.Issuer},
		"registry": {Registry: "ghcr.io/someone/", Identity: shipped.Identity, Issuer: shipped.Issuer},
		"issuer":   {Registry: shipped.Registry, Identity: shipped.Identity, Issuer: "https://example.com"},
	} {
		if !p.Replaced() {
			t.Errorf("a policy with a replaced %s does not report itself as replaced", what)
		}
		res := Result{Outcome: Verified, Image: "ghcr.io/brig-sh/x:1", Digest: "sha256:abc", Policy: p}
		if got := res.Message(); strings.Contains(got, "signature verified, booting") {
			t.Errorf("a replaced %s printed the unqualified success line: %s", what, got)
		}
		if got := res.Message(); !strings.Contains(got, "replaced") {
			t.Errorf("a replaced %s does not say the policy was replaced: %s", what, got)
		}
	}

	// And the shipped policy still says exactly what it always said.
	res := Result{Outcome: Verified, Image: "ghcr.io/brig-sh/x:1", Digest: "sha256:abc", Policy: shipped}
	if got := res.Message(); !strings.Contains(got, "signature verified, booting") {
		t.Errorf("the shipped policy stopped saying it verified: %s", got)
	}
}

// Image returns Verified with an empty Digest, and the line it produces is the
// success line on every runtime that cannot boot by digest. Both spellings are
// covered: the replaced-policy one formats the same field.
func TestVerifiedWithoutADigestNamesTheTag(t *testing.T) {
	shipped := DefaultPolicy()
	replaced := Policy{Registry: "ghcr.io/someone/", Identity: shipped.Identity,
		Issuer: shipped.Issuer, Cosign: shipped.Cosign}

	for what, p := range map[string]Policy{"shipped": shipped, "replaced": replaced} {
		got := Result{Policy: p, Outcome: Verified, Image: "ghcr.io/brig-sh/x:1"}.Message()
		if !strings.Contains(got, "booting the tag") {
			t.Errorf("the %s policy does not say what booted: %q", what, got)
		}
		if strings.Contains(got, "booting  ") || strings.HasSuffix(got, "booting ") {
			t.Errorf("the %s policy left the digest slot empty: %q", what, got)
		}
	}

	// And a digest, when there is one, is still what the line names.
	got := Result{Policy: shipped, Outcome: Verified, Image: "ghcr.io/brig-sh/x:1",
		Digest: "sha256:abc"}.Message()
	if !strings.Contains(got, "sha256:abc") {
		t.Errorf("the digest path stopped naming the digest: %q", got)
	}
}

// Under require these two outcomes refuse the boot, so their line says that and
// names what the user can do instead.
func TestRefusalSaysItRefusedAndNamesTheWayOut(t *testing.T) {
	for outcome, remedy := range map[Outcome]string{
		NotOurs:   "BRIG_VERIFY_REGISTRY",
		NoTooling: "brew install cosign",
	} {
		got := Result{Policy: DefaultPolicy(), Outcome: outcome, Image: "img"}.Refusal()
		if !strings.Contains(got, "refusing to boot") {
			t.Errorf("%v refusal does not say it refused: %q", outcome, got)
		}
		if !strings.Contains(got, "BRIG_VERIFY=warn") || !strings.Contains(got, remedy) {
			t.Errorf("%v refusal does not name the way out: %q", outcome, got)
		}
		if !strings.Contains(got, "img") {
			t.Errorf("%v refusal does not name the image: %q", outcome, got)
		}
		if strings.Contains(strings.ToLower(got), "booting") {
			t.Errorf("%v refusal still says something booted: %q", outcome, got)
		}
	}
}

// Refusal owns the wording of two rows. The rest carry Message, and the
// sentence that refuses them is the caller's own.
func TestRefusalCarriesMessageForTheRowsItDoesNotOwn(t *testing.T) {
	for _, outcome := range []Outcome{Verified, Failed, Mismatch, Unresolved} {
		r := Result{Policy: DefaultPolicy(), Outcome: outcome, Image: "img",
			Digest: "sha256:abc", Local: "sha256:def", Detail: "d"}
		if got, want := r.Refusal(), r.Message(); got != want {
			t.Errorf("%v: refusal is %q, message is %q", outcome, got, want)
		}
	}

	for _, outcome := range []Outcome{NotOurs, NoTooling} {
		r := Result{Policy: DefaultPolicy(), Outcome: outcome, Image: "img"}
		if r.Refusal() == r.Message() {
			t.Errorf("%v: the refusal reads as the warning: %q", outcome, r.Refusal())
		}
	}
}

// Under a replaced policy the registry that put an image in the NotOurs row is
// the user's own, so the line names neither brig-sh nor settings already set.
func TestARefusedThirdPartyImageUnderAReplacedPolicyDoesNotNameBrigSh(t *testing.T) {
	shipped := DefaultPolicy()
	replaced := Policy{Registry: "ghcr.io/acme/", Identity: shipped.Identity,
		Issuer: shipped.Issuer, Cosign: shipped.Cosign}

	got := Result{Policy: replaced, Outcome: NotOurs, Image: "ghcr.io/other/x:1"}.Refusal()
	if strings.Contains(got, "brig-sh") {
		t.Errorf("a replaced policy blamed brig-sh: %q", got)
	}
	if !strings.Contains(got, "ghcr.io/acme/") {
		t.Errorf("the refusal does not name the registry that decided: %q", got)
	}
	if !strings.Contains(got, "refusing to boot") || !strings.Contains(got, "BRIG_VERIFY=warn") {
		t.Errorf("the replaced refusal lost its shape: %q", got)
	}
	// Warn is the only way out offered. A wider registry lands the image on the
	// signature check, which is a harder row to leave.
	if strings.Contains(got, "widen") {
		t.Errorf("the refusal sends the reader to widen the registry: %q", got)
	}

	// Replaced is true when any one of the three settings differs, and each is
	// set on its own. With only the identity replaced the prefix that rejected
	// the image is still brig's, and the line names that registry.
	identityOnly := Policy{Registry: shipped.Registry, Identity: `.*`,
		Issuer: shipped.Issuer, Cosign: shipped.Cosign}
	if !identityOnly.Replaced() {
		t.Fatal("a replaced identity no longer reports the policy as replaced")
	}
	got = Result{Policy: identityOnly, Outcome: NotOurs,
		Image: "docker.io/library/ubuntu:24.04"}.Refusal()
	if strings.Contains(got, "you set") || strings.Contains(got, "policy you") {
		t.Errorf("the refusal credits the reader with a registry they did not set: %q", got)
	}
	if !strings.Contains(got, shipped.Registry) {
		t.Errorf("the refusal does not name the registry that decided: %q", got)
	}

	// And the shipped policy still says whose image it is not.
	got = Result{Policy: shipped, Outcome: NotOurs, Image: "docker.io/library/ubuntu:24.04"}.Refusal()
	if !strings.Contains(got, "not published by brig-sh") {
		t.Errorf("the shipped policy stopped naming the publisher: %q", got)
	}
}
