package verify

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
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
func TestVerifyMismatchOnAThirdPartyImageWarns(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, sigTag("docker.io/library/ubuntu"), nil, nil)

	got := p.Verify("docker.io/library/ubuntu:24.04", "sha256:"+strings.Repeat("c", 64))
	if got.Outcome != Mismatch {
		t.Fatalf("outcome = %v, want Mismatch", got.Outcome)
	}
	if got.Ours {
		t.Error("a third-party mismatch must not be marked Ours, or it would stop")
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

// A third party's image resolves but carries no signature of ours to check, so
// with a matching (or absent) local copy it is NotOurs, and the digest still
// comes back so the caller can pin it.
func TestVerifyThirdPartyResolvesButIsNotOurs(t *testing.T) {
	p := DefaultPolicy()
	digestStub(t, sigTag("docker.io/library/ubuntu"), nil, nil)

	got := p.Verify("docker.io/library/ubuntu:24.04", "")
	if got.Outcome != NotOurs {
		t.Fatalf("outcome = %v, want NotOurs", got.Outcome)
	}
	if got.Digest != "sha256:"+digestHex {
		t.Errorf("a third-party digest was still worth resolving: %q", got.Digest)
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

// A typo in the setting must not silently disable the check.
func TestParseModeFallsBackToWarn(t *testing.T) {
	for in, want := range map[string]Mode{
		"off": Off, "none": Off, "0": Off,
		"require": Require, "strict": Require,
		"warn": Warn, "": Warn, "yes-please": Warn, "OFF": Off,
	} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessagesNameTheImageAndTheRemedy(t *testing.T) {
	cases := map[Outcome]string{
		NotOurs:   "not published by brig-sh",
		NoTooling: "brew install cosign",
		Failed:    "DID NOT VERIFY",
	}
	for outcome, want := range cases {
		msg := Result{Outcome: outcome, Image: "img", Detail: "d"}.Message()
		if !strings.Contains(msg, want) {
			t.Errorf("%v message = %q, want it to mention %q", outcome, msg, want)
		}
		if !strings.Contains(msg, "img") {
			t.Errorf("%v message does not name the image: %q", outcome, msg)
		}
	}
}
