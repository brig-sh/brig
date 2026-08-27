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
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// ParseModeStrict reads a mode and refuses anything it does not recognise.
// BRIG_VERIFY is the switch the whole image-verification path turns on, so a
// typo in it must stop the run, not quietly fall back to Warn and weaken the
// check. The empty string is the unset case and keeps the Warn default.
func ParseModeStrict(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return Warn, nil
	case "warn":
		return Warn, nil
	case "off", "none", "0":
		return Off, nil
	case "require", "strict":
		return Require, nil
	default:
		return Warn, fmt.Errorf("BRIG_VERIFY=%q is not a mode: use off, warn, or require", s)
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

// BootAssetsPolicy matches the boot bundle: the kernel and the initrd a
// genericBoot profile starts, which carry the in-guest agent.
//
// A separate policy rather than a second registry on the image one, because
// the two are separate trust roots. The bundle is published by another
// repository, under another registry prefix, by another workflow, and running
// it through DefaultPolicy would land on NotOurs -- a check that inspects
// nothing and reports success, which is worse than no check because it reads
// like one.
//
// The identity here was read off the signature the published bundle carries
// rather than written from the repository layout, because a regexp that names
// a workflow which does not sign anything fails every boot, and one that is
// too loose passes signatures nobody meant to trust. Anchored at both ends and
// with the dots escaped, for the reason DefaultPolicy gives.
func BootAssetsPolicy() Policy {
	return Policy{
		Registry: "ghcr.io/nofireai/",
		Identity: `^https://github\.com/NOFireAI/hull-assets/\.github/workflows/build-assets\.yml@refs/heads/main$`,
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
	// Mismatch: the reference resolved to one digest in the registry, and the
	// copy already in the local store is a different one. Treated as the
	// signature-failure row of the table -- fail-closed for our own images,
	// warn for a third party's -- because a tag that names one object in the
	// registry and another on disk is exactly the gap booting by digest closes.
	Mismatch
	// Unresolved: the reference could not be resolved to a registry digest, so
	// there was nothing to pin or verify. A registry that cannot be reached
	// lands here, and that is "could not check", not "failed": like NoTooling
	// it warns and boots the tag, and only Require refuses it.
	Unresolved
)

// Result carries the outcome and the detail worth printing.
type Result struct {
	Outcome Outcome
	Image   string
	// Digest is the registry digest the reference resolved to, and -- for a
	// Verified result -- the digest whose signature checked out. Empty when the
	// reference could not be resolved (NoTooling, Unresolved).
	Digest string
	// Local is the digest the local store already held for the reference, set
	// only on a Mismatch so the message can name both sides.
	Local string
	// Ours reports whether the image sits under the policy's registry prefix.
	// It is what turns a Mismatch into a stop rather than a warning, the same
	// way it separates Failed from NotOurs.
	Ours bool
	// Detail is cosign's own output when it failed, trimmed.
	Detail string
}

// Message is the line to show the user, phrased for the outcome.
func (r Result) Message() string {
	switch r.Outcome {
	case Verified:
		// Naming the digest is the point of this whole path: the line vouches
		// for the bytes that boot, not for a tag that can move under them.
		return fmt.Sprintf("image %s: signature verified, booting %s", r.Image, r.Digest)
	case NotOurs:
		return fmt.Sprintf("image %s is not published by brig-sh, so there is no "+
			"signature of ours to check. That is expected for your own image -- "+
			"just be sure you trust where it came from", r.Image)
	case NoTooling:
		return fmt.Sprintf("cannot verify image %s: cosign is not installed "+
			"(`brew install cosign`). Booting it unchecked", r.Image)
	case Unresolved:
		return fmt.Sprintf("cannot reach the registry to verify image %s: %s. The copy "+
			"on disk could not be checked against what the registry serves",
			r.Image, r.Detail)
	case Mismatch:
		if r.Ours {
			return fmt.Sprintf("image %s claims to be published by brig-sh, but the copy "+
				"in your local store is %s, NOT the %s that verified. Booting the verified "+
				"digest", r.Image, r.Local, r.Digest)
		}
		return fmt.Sprintf("image %s in your local store is %s, not the %s the registry "+
			"now serves. Booting the registry digest", r.Image, r.Local, r.Digest)
	default:
		return fmt.Sprintf("image %s claims to be published by brig-sh, but its "+
			"signature DID NOT VERIFY: %s", r.Image, r.Detail)
	}
}

// normalizeRef puts a reference into the one spelling the prefix test can be
// applied to.
//
// The test is a byte prefix, and a registry reference has several spellings
// that name the same image: ghcr.io:443/... is the same host and the same
// image as ghcr.io/..., and hosts are case-insensitive. Each of those read as
// "somebody else's image", and NotOurs does not refuse -- it warns and boots
// unverified. So the spellings were not an inconvenience, they were a way to
// ask for the check to be skipped, and an imported profile is free to write
// the image either way.
//
// Deliberately small: brig has one direct dependency and a reference parser is
// not worth a second. Scheme, host case and the default port are the whole
// difference between the equivalent spellings.
func normalizeRef(ref string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(ref, "https://"), "http://")
	host, rest, found := strings.Cut(s, "/")
	if !found {
		return strings.ToLower(s)
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ":443")
	return host + "/" + rest
}

// Image checks one image reference against the policy.
func (p Policy) Image(ref string) Result {
	subject := normalizeRef(ref)
	if !strings.HasPrefix(subject, p.Registry) {
		return Result{Outcome: NotOurs, Image: ref}
	}
	if _, err := lookPath(p.Cosign); err != nil {
		return Result{Outcome: NoTooling, Image: ref}
	}
	out, err := run(p.Cosign,
		"verify",
		"--certificate-identity-regexp", p.Identity,
		"--certificate-oidc-issuer", p.Issuer,
		subject,
	)
	if err != nil {
		return Result{Outcome: Failed, Image: ref, Detail: firstLine(out, err)}
	}
	return Result{Outcome: Verified, Image: ref}
}

// Verify resolves the reference to a registry digest, verifies that digest,
// and reports whether the copy already in the local store is the same object.
//
// This is the stronger sibling of Image. Image checks the tag, and under the
// default pull policy the tag cosign checks and the copy the runtime boots are
// not required to be the same bytes. Verify closes that gap: it resolves the
// tag to the digest the registry serves right now, checks the signature on
// THAT digest, and hands the digest back so the caller can boot it rather than
// the tag. The digest resolved is the digest verified is the digest booted.
//
// localDigest is what brig's runtime already holds for the reference in its
// local store, or "" when the runtime holds nothing or cannot say. When it is
// present and differs from the digest we resolved, the local copy is not the
// object we verified, and that is a Mismatch -- fail-closed for our own images,
// a warning for a third party's.
//
// Only the runtimes whose store is addressable by digest use this path; see
// Runtime.PinsDigest. hull's is not, so on macOS brig stays on Image and the
// tag it names, rather than claim a guarantee that runtime cannot deliver.
func (p Policy) Verify(ref, localDigest string) Result {
	subject := normalizeRef(ref)
	ours := strings.HasPrefix(subject, p.Registry)

	// A third party's image carries no signature of ours to check, and that
	// settles it before cosign is so much as looked up. Resolving its digest
	// anyway was tried and withdrawn: it cost a registry round trip on every
	// boot, stalled a laptop that was off the network for the full dial
	// timeout, and bought a pin brig could not vouch for. The tag path never
	// charged for an image it had nothing to say about, and neither does this.
	if !ours {
		return Result{Outcome: NotOurs, Image: ref}
	}

	// cosign resolves the digest as well as the signature here, so its absence
	// stops both halves at once, exactly as it does for Image.
	if _, err := lookPath(p.Cosign); err != nil {
		return Result{Outcome: NoTooling, Image: ref, Ours: ours}
	}

	// Resolve the reference to a digest BEFORE the check. A registry that cannot
	// be reached fails here, and that is "could not check" rather than "failed":
	// it must not read like a bad signature, so it lands on Unresolved.
	digest, err := p.resolveDigest(ref)
	if err != nil {
		return Result{Outcome: Unresolved, Image: ref, Ours: ours, Detail: err.Error()}
	}

	// The signature check, on the digest rather than the tag.
	out, verr := run(p.Cosign,
		"verify",
		"--certificate-identity-regexp", p.Identity,
		"--certificate-oidc-issuer", p.Issuer,
		refWithDigest(ref, digest),
	)
	if verr != nil {
		return Result{Outcome: Failed, Image: ref, Digest: digest, Ours: ours, Detail: firstLine(out, verr)}
	}

	// The copy on disk must be the object we just resolved -- and, for our own
	// images, verified. A local digest that differs is the tag pointing at one
	// thing in the registry and another in the store, which is the case this
	// path exists to catch.
	if localDigest != "" && localDigest != digest {
		return Result{Outcome: Mismatch, Image: ref, Digest: digest, Local: localDigest, Ours: ours}
	}
	return Result{Outcome: Verified, Image: ref, Digest: digest, Ours: ours}
}

// resolveDigest asks cosign for the digest the registry serves for ref.
//
// cosign is already the one tool brig links its trust to, and it can name the
// digest without a second registry client or a new dependency. `cosign
// triangulate ref` prints where cosign would look for the signature, which is
// <repo>:sha256-<the image's own digest>.sig -- so the digest is right there in
// the tag it prints. A cosign new enough to take `--type=digest` prints
// <repo>@sha256:<digest> instead; digestFromOutput reads either spelling, so
// brig does not have to know which cosign it is driving.
//
// Resolving through cosign, not the runtime, is deliberate: the runtime's store
// is on disk and is the very thing we are checking the registry against, so
// asking it to resolve the tag would compare the local copy with itself.
func (p Policy) resolveDigest(ref string) (string, error) {
	out, err := run(p.Cosign, "triangulate", ref)
	if err != nil {
		return "", errors.New(firstLine(out, err))
	}
	if d := digestFromOutput(out); d != "" {
		return d, nil
	}
	return "", fmt.Errorf("cosign named no digest for %s", ref)
}

// digestFromOutput pulls a sha256 digest out of cosign's triangulate output.
//
// Both spellings cosign uses -- the "sha256-<hex>.sig" signature tag and the
// "sha256:<hex>" of --type=digest -- carry the same 64 hex characters after a
// single separator, so the first "sha256" followed by ":" or "-" and 64 hex
// characters is the digest in either. Scanning for that rather than splitting
// on a fixed position steps over cosign's own warning lines, which mention no
// such run of hex.
func digestFromOutput(out string) string {
	const marker = "sha256"
	for i := strings.Index(out, marker); i >= 0; {
		rest := out[i+len(marker):]
		if len(rest) >= 1+64 && (rest[0] == ':' || rest[0] == '-') && isHex(rest[1:1+64]) {
			return marker + ":" + rest[1:1+64]
		}
		next := strings.Index(out[i+len(marker):], marker)
		if next < 0 {
			return ""
		}
		i += len(marker) + next
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// refWithDigest rewrites ref to name a digest in place of its tag, so cosign
// verifies the exact object the tag resolved to. A digest and a tag cannot both
// sit on a reference (repo:tag@sha256:... is not a thing), so the tag is
// dropped first: an existing @digest is replaced, and a trailing :tag is cut --
// but only when the colon is a tag separator and not the ":port" of a registry
// host, which is what the slash test distinguishes.
func refWithDigest(ref, digest string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	} else if i := strings.LastIndexByte(ref, ':'); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		ref = ref[:i]
	}
	return ref + "@" + digest
}

// lookPath and run are variables so tests can drive the decision table
// without a cosign on PATH.
var lookPath = exec.LookPath

// cosignTimeout bounds every cosign invocation. The registry is on the other
// end of each one, and a dial that never completes is the ordinary shape of an
// outage; without a bound that outage would sit on the boot path for as long
// as the network stack cares to wait. A cosign that is cut off here reads as
// "could not be verified", the same as any other failure to reach the registry.
var cosignTimeout = 30 * time.Second

var run = func(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cosignTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// Killing the process is not the same as being done with it. Anything it
	// spawned inherits the pipes below, and Run waits for those to close, so a
	// helper left behind by a killed cosign would hold the boot for as long as
	// it lived. WaitDelay is the bound on that wait: once the deadline has
	// killed the process, a second is all the grandchildren get before Run
	// returns without them.
	cmd.WaitDelay = time.Second
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
