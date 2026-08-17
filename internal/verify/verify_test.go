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

// The identity is anchored on the repository AND the workflow file, so a
// signature from any other workflow -- including another one in the same
// repository -- fails the check.
func TestDefaultPolicyPinsRepoAndWorkflow(t *testing.T) {
	p := DefaultPolicy()
	for _, want := range []string{
		"brig-sh/community-images",
		"build-images.yml",
		"token.actions.githubusercontent.com",
	} {
		if !strings.Contains(p.Identity+p.Issuer, want) {
			t.Errorf("policy does not pin %q: %s %s", want, p.Identity, p.Issuer)
		}
	}
	if !strings.HasPrefix(p.Identity, "^") {
		t.Errorf("identity regexp is not anchored: %s", p.Identity)
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
