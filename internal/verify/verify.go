// Package verify checks that a guest image is the one we published.
//
// The images brig boots come from a registry, and the question worth asking
// is not "was this signed?" but "was this built by that workflow, in that
// repo?". Everything under brig-sh is signed with keyless cosign, so the
// signature is bound to the workflow that produced it and recorded in
// Sigstore's public transparency log; there is no key to distribute and none
// for us to lose.
//
// The check never hard-fails by default. Bring-your-own images are a
// first-class way to use brig, and refusing to boot one would make the
// feature useless -- so an image we did not publish is reported, not blocked.
// An image that claims to be ours and fails verification is a different
// matter, and stops to ask.
package verify

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Mode is how strict the check is.
type Mode string

const (
	// Off skips verification entirely.
	Off Mode = "off"
	// Warn reports an unverifiable image and stops to ask about one that
	// claims to be ours and is not. The default.
	Warn Mode = "warn"
	// Require refuses to boot anything that cannot be positively verified,
	// including a third-party image and a host with no cosign.
	Require Mode = "require"
)

// ParseMode reads a mode, falling back to Warn for anything unrecognised: a
// typo in this setting must not silently disable the check.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "0":
		return Off
	case "require", "strict":
		return Require
	default:
		return Warn
	}
}

// Policy is what counts as ours.
type Policy struct {
	// Registry is the image-reference prefix we publish under.
	Registry string
	// Identity is the certificate identity regexp: the workflow, in the repo,
	// that is allowed to have signed it.
	Identity string
	// Issuer is the OIDC issuer that vouched for that identity.
	Issuer string
	// Cosign is the binary to run.
	Cosign string
}

// DefaultPolicy matches what brig-sh/community-images publishes and
// documents. The identity is anchored on the repository AND the workflow
// file, so a signature from any other workflow -- including another one in
// the same repository -- fails it.
//
// Anchored at BOTH ends, and with every dot escaped, because this is a regexp
// and not a prefix match. Ending it at "@refs/" accepted a certificate minted
// by the same workflow running on any branch or on any pull request --
// refs/heads/attacker-branch, refs/pull/7/merge -- which is a signature
// anybody who can open a PR against community-images can obtain. And an
// unescaped dot matches any character, so "build-images0yml" and
// "githubXcom" passed too. Only the workflow as it runs on main publishes
// what brig boots, so that is what the identity says.
func DefaultPolicy() Policy {
	return Policy{
		Registry: "ghcr.io/brig-sh/",
		Identity: `^https://github\.com/brig-sh/community-images/\.github/workflows/build-images\.yml@refs/heads/main$`,
		Issuer:   "https://token.actions.githubusercontent.com",
		Cosign:   "cosign",
	}
}

// Outcome is what the check concluded.
type Outcome int

const (
	// Verified: ours, and the signature checks out.
	Verified Outcome = iota
	// NotOurs: published by somebody else, so there is no signature of ours
	// to check. Expected for a bring-your-own image.
	NotOurs
	// NoTooling: cosign is not installed, so nothing could be checked.
	NoTooling
	// Failed: it claims to be ours and the signature does not check out.
	Failed
)

// Result carries the outcome and the detail worth printing.
type Result struct {
	Outcome Outcome
	Image   string
	// Detail is cosign's own output when it failed, trimmed.
	Detail string
}

// Message is the line to show the user, phrased for the outcome.
func (r Result) Message() string {
	switch r.Outcome {
	case Verified:
		return fmt.Sprintf("image %s: signature verified", r.Image)
	case NotOurs:
		return fmt.Sprintf("image %s is not published by brig-sh, so there is no "+
			"signature of ours to check. That is expected for your own image -- "+
			"just be sure you trust where it came from", r.Image)
	case NoTooling:
		return fmt.Sprintf("cannot verify image %s: cosign is not installed "+
			"(`brew install cosign`). Booting it unchecked", r.Image)
	default:
		return fmt.Sprintf("image %s claims to be published by brig-sh, but its "+
			"signature DID NOT VERIFY: %s", r.Image, r.Detail)
	}
}

// Image checks one image reference against the policy.
func (p Policy) Image(ref string) Result {
	if !strings.HasPrefix(ref, p.Registry) {
		return Result{Outcome: NotOurs, Image: ref}
	}
	if _, err := lookPath(p.Cosign); err != nil {
		return Result{Outcome: NoTooling, Image: ref}
	}
	out, err := run(p.Cosign,
		"verify",
		"--certificate-identity-regexp", p.Identity,
		"--certificate-oidc-issuer", p.Issuer,
		ref,
	)
	if err != nil {
		return Result{Outcome: Failed, Image: ref, Detail: firstLine(out, err)}
	}
	return Result{Outcome: Verified, Image: ref}
}

// lookPath and run are variables so tests can drive the decision table
// without a cosign on PATH.
var lookPath = exec.LookPath

var run = func(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func firstLine(out string, err error) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// cosign prints a banner about the transparency log before the real
		// reason; the reason is the first line that is not one of those.
		if line == "" || strings.HasPrefix(line, "Verification for ") ||
			strings.HasPrefix(line, "The following checks") ||
			strings.HasPrefix(line, "  - ") {
			continue
		}
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "no detail"
}
