package wrap

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// VerifyRefusedError marks a boot brig stopped because the guest image or the
// kernel it boots did not verify: a bad signature, a mismatch, or a "could not
// check" under BRIG_VERIFY=require, plus the interactive refusals of the same.
//
// It carries the underlying message unchanged and only adds a class a caller can
// match, so a run refused for this reason gets an exit code of its own rather
// than folding into the general failure. EnsureRunning wraps the verify step's
// error in it, which is the one place both the image and the boot-asset checks
// pass through.
type VerifyRefusedError struct{ Err error }

func (e *VerifyRefusedError) Error() string { return e.Err.Error() }
func (e *VerifyRefusedError) Unwrap() error { return e.Err }

// verifyImage checks the guest image before booting it, and decides what to
// do about the answer.
//
// The rule is deliberately asymmetric. An image nobody claimed to publish is
// reported and booted: bring-your-own images are a supported way to use brig,
// and blocking one would make the feature useless. An image that sits under
// our registry and fails verification is the one case that stops, because
// that combination has no innocent reading -- it means something is
// pretending to be us.
//
// There are two checks behind this one method, and the runtime decides which.
// A runtime whose store is addressable by digest (containerd on Linux, hull
// from 0.1.0-rc23) can boot the exact bytes cosign checked, so it takes
// verifyDigest: resolve the tag to a digest, verify that digest, compare it
// against the copy on disk, and hand the digest on to boot. One whose store is
// not stays on verifyTag, which checks the tag as brig always has -- because
// claiming a digest was pinned when it was not is worse than the gap the claim
// would paper over -- and says so, because a user who reads "verified" on that
// path is owed the difference. See runtime.Runtime.PinsDigest.
func (c *Config) verifyImage() error {
	if c.Verify == verify.Off {
		// Said out loud rather than passed over in silence. This was the only
		// state no command mentioned: the check returned here, before any
		// output, so a sandbox booted unchecked and nothing on screen said so.
		// The quietest path was the one that most needed a line.
		c.warnf("BRIG_VERIFY=off, so the guest image is not checked before it boots")
		return nil
	}
	if c.Runtime.PinsDigest() {
		return c.verifyDigest()
	}
	c.warnf("this %s cannot boot by digest (hull 0.1.0-rc23 or newer can), so the tag "+
		"is verified and booted rather than a pinned digest", c.Runtime.Kind())
	return c.verifyTag()
}

// verifyTag is the tag-level check, kept for a runtime that cannot boot by
// digest. It is brig's original behaviour, unchanged: the tag is what cosign
// sees and what the runtime boots, and under the default pull policy those need
// not be the same bytes -- the limitation documented in docs/security.md.
func (c *Config) verifyTag() error {
	res := c.VerifyPolicy.Image(c.Image)

	switch res.Outcome {
	case verify.Verified:
		c.warnf("%s", res.Message())
		return nil

	case verify.NotOurs, verify.NoTooling:
		if c.Verify == verify.Require {
			return fmt.Errorf("%s (BRIG_VERIFY=require)", res.Message())
		}
		c.warnf("%s", res.Message())
		return nil

	default:
		c.warnf("%s", res.Message())
		if c.Verify == verify.Require {
			return errors.New("refusing to boot an image that failed verification")
		}
		if !c.confirm("Boot it anyway?") {
			// Not "turn the check off". A signature that is present and does
			// not check out is the one outcome with no innocent reading, and
			// disabling the control that caught it is not a remedy. Pulling
			// again fixes a stale or truncated copy; naming a digest the user
			// has checked themselves is the deliberate way past it.
			return errors.New("aborted: the image failed verification. Pull it again " +
				"(BRIG_PULL=always), or set BRIG_IMAGE to a digest you have checked " +
				"yourself")
		}
		return nil
	}
}

// verifyDigest is the digest-level check, for a runtime that boots the object
// it is handed rather than a name. It resolves the reference to the digest the
// registry serves, verifies that digest, and -- when the resolve succeeds --
// records it in BootDigest so EnsureRunning boots that exact object.
//
// The decision table is the same shape as verifyTag's, with two rows the tag
// path cannot express. Unresolved (a registry that could not be reached) joins
// NoTooling: nothing could be checked, so it warns and boots the tag, and only
// Require refuses. Mismatch (the local store holds a different digest than the
// one verified) joins the failure row, and splits the way Failed and NotOurs
// do: our own image stops to ask, a third party's warns. Either way brig boots
// the digest it resolved, not the copy on disk, so a "yes" boots the verified
// object rather than the suspect one.
func (c *Config) verifyDigest() error {
	// What the store already holds for this reference, so the resolve can be
	// compared against it. A runtime that cannot say returns "", which reads as
	// "no local copy" and raises no mismatch.
	local, _ := c.Runtime.LocalDigest(c.Image)
	res := c.VerifyPolicy.Verify(c.Image, local)

	switch res.Outcome {
	case verify.Verified, verify.NotOurs:
		// Both boot the digest we resolved. A third party's carries no signature
		// of ours, but pinning the digest still boots the bytes the registry
		// serves rather than a stale local tag.
		c.BootDigest = res.Digest
		if res.Outcome == verify.NotOurs && c.Verify == verify.Require {
			return fmt.Errorf("%s (BRIG_VERIFY=require)", res.Message())
		}
		c.warnf("%s", res.Message())
		return nil

	case verify.NoTooling:
		// Nothing could be checked or pinned, so the tag boots as given. Leaving
		// BootDigest empty is what makes that happen. A missing cosign is a gap
		// in the machine's setup, said so on every boot, not something that
		// changes between one run and the next.
		if c.Verify == verify.Require {
			return fmt.Errorf("%s (BRIG_VERIFY=require)", res.Message())
		}
		c.warnf("%s", res.Message())
		return nil

	case verify.Unresolved:
		// The registry could not be reached, so one of our own images could not
		// be checked. That is the outage case, and it stops exactly as it did
		// before the digest check existed, when the same outage failed inside
		// `cosign verify` and asked. A boot that goes ahead here on its own
		// would let anyone who can make the registry unreachable, a captive
		// portal or a sinkhole, turn the default mode into "unchecked". Nothing
		// is pinned either way: a yes boots the cached tag.
		c.warnf("%s", res.Message())
		if c.Verify == verify.Require {
			return errors.New("refusing to boot an image that could not be verified " +
				"(BRIG_VERIFY=require)")
		}
		if !c.confirm("Boot the cached copy unverified?") {
			return errors.New("aborted: the registry could not be reached, so the image " +
				"could not be verified. Try again with the registry reachable, or set " +
				"BRIG_VERIFY=off to boot the cached copy unchecked")
		}
		return nil

	case verify.Mismatch:
		// Boot the resolved digest whatever we decide below: the object on disk
		// is the one we are refusing to trust, so a "yes" must not boot it.
		c.BootDigest = res.Digest
		c.warnf("%s", res.Message())
		if !res.Ours {
			// A third party's copy differing from the registry is a warning, the
			// same weight as NotOurs -- unless Require, which trusts nothing it
			// cannot positively verify.
			if c.Verify == verify.Require {
				return errors.New("refusing under BRIG_VERIFY=require: the local image " +
					"is not the digest the registry serves")
			}
			return nil
		}
		// Our own tag over a copy that is not the one that verified has no
		// innocent reading, so it stops exactly as a failed signature does.
		if c.Verify == verify.Require {
			return errors.New("refusing to boot: the local copy is not the verified digest")
		}
		if !c.confirm("The local copy is not the verified image. Boot the verified digest anyway?") {
			return errors.New("aborted: the local copy does not match the verified digest. " +
				"Set BRIG_PULL=always to replace it, or BRIG_VERIFY=off if you know why it differs")
		}
		return nil

	default: // verify.Failed
		c.BootDigest = res.Digest
		c.warnf("%s", res.Message())
		if c.Verify == verify.Require {
			return errors.New("refusing to boot an image that failed verification")
		}
		if !c.confirm("Boot it anyway?") {
			// Not "turn the check off". A signature that is present and does
			// not check out is the one outcome with no innocent reading, and
			// disabling the control that caught it is not a remedy. Pulling
			// again fixes a stale or truncated copy; naming a digest the user
			// has checked themselves is the deliberate way past it.
			return errors.New("aborted: the image failed verification. Pull it again " +
				"(BRIG_PULL=always), or set BRIG_IMAGE to a digest you have checked " +
				"yourself")
		}
		return nil
	}
}

// confirm asks a yes/no question, defaulting to no.
//
// Without a terminal there is nobody to ask, and assuming yes would turn the
// one check that stops into one that does not. So it answers no and says
// which setting overrides it -- a scripted run that genuinely wants to
// proceed can say so in advance.
//
// A caller that has no terminal of its own says so with NoTerminal rather than
// leaving it to be guessed from this process's stdin. The two are not the same
// question for a daemon: brigd's stdin may well be a terminal, it is simply
// not the one the request came from, and asking there puts the question in
// front of nobody while the client waits.
func (c *Config) confirm(question string) bool {
	if c.NoTerminal || !IsTerminal(os.Stdin) {
		c.warnf("not a terminal, so there is nobody to ask: refusing. " +
			"Set BRIG_VERIFY=off to boot it regardless.")
		return false
	}
	fmt.Fprintf(c.Err, "brig: %s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// verifyBootAssets checks the kernel and initrd a genericBoot profile starts.
//
// brig verified the guest image and not the kernel that runs it, which is the
// more privileged of the two: the initrd carries the in-guest agent, and six
// of the eight shipped profiles boot a bundle rather than their own image, so
// this is the ordinary path rather than an edge case. The bundle is signed;
// the check was simply never made.
//
// It is a trust root of its own, so it uses its own policy: another
// repository, another registry prefix, another workflow. Running it through
// the image policy would land on NotOurs, which inspects nothing and reports
// success -- worse than no check, because it reads like one.
//
// The modes are the image's modes, so a reader has one rule to learn rather
// than two. Nothing is pinned from the result: the bundle is fetched by oras
// or by hull, neither of which brig hands a digest to, so what this buys is a
// refusal before the boot rather than a pinned download. Naming the digest in
// the report is the next step, and it needs the fetchers to take one.
func (c *Config) verifyBootAssets() error {
	if c.Verify == verify.Off || !c.Profile.GenericBoot {
		return nil
	}
	ref := runtime.BootAssetsRef()
	// Its own identity and registry, but the same cosign binary: which tool to
	// run is a fact about the machine, set once by BRIG_COSIGN_BIN, and not
	// something each trust root should answer differently. Hardcoding it here
	// also made this unstubbable, so the check reached the real registry from a
	// unit test.
	policy := verify.BootAssetsPolicy()
	policy.Cosign = c.VerifyPolicy.Cosign
	res := policy.Verify(ref, "")

	switch res.Outcome {
	case verify.Verified:
		c.warnf("boot assets %s: signature verified", ref)
		return nil

	case verify.NotOurs:
		// The bundle was pointed somewhere else, with BRIG_BOOT_ASSETS_REF or a
		// mirror. brig has nothing to check there and says so rather than
		// implying it checked.
		if c.Verify == verify.Require {
			return fmt.Errorf("refusing to boot: the boot assets at %s are not published "+
				"by brig, so their signature cannot be checked (BRIG_VERIFY=require)", ref)
		}
		c.warnf("boot assets %s are not published by brig, so nothing was checked "+
			"about the kernel this sandbox boots", ref)
		return nil

	case verify.NoTooling, verify.Unresolved:
		// Could not check, rather than failed. It follows the image's rule: a
		// warning by default, a refusal under require.
		if c.Verify == verify.Require {
			return fmt.Errorf("refusing to boot: the boot assets at %s could not be "+
				"verified (%s) (BRIG_VERIFY=require)", ref, res.Message())
		}
		c.warnf("the boot assets at %s could not be verified: %s", ref, res.Message())
		return nil

	default:
		// A signature that is present and wrong on the kernel brig is about to
		// boot. This one stops whatever the mode, short of off: there is no
		// reading of a bad signature here that is worth a prompt.
		return fmt.Errorf("refusing to boot: the boot assets at %s failed verification (%s). "+
			"Set BRIG_VERIFY=off to boot them regardless", ref, res.Detail)
	}
}
