package wrap

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// checkTag and checkDigest are the two checks behind verifyImage, as variables
// so a test can hand the decision a result that a real cosign reaches only
// after cosignTimeout, such as one that timed out.
var (
	checkTag    = verify.Policy.Image
	checkDigest = verify.Policy.Verify
)

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
		//
		// Unless the envelope has already said it. The VERIFY row carries the
		// mode, so on a run that printed the block this line is the same fact a
		// second time, four lines apart -- and a fact stated twice is one a
		// reader starts skipping. Said exactly once at every level: by the row
		// when there is one, by this line when there is not, which is every
		// default run, every -q run and every cold `brig sh`.
		//
		// A genericBoot run skips the kernel's checks too, and says so in the
		// same line, so the setting is still named once.
		if !c.envelopeShown {
			if c.Profile.GenericBoot {
				c.alertf("BRIG_VERIFY=off, so the signature and digest checks are skipped: " +
					"the guest image and the kernel it boots are not checked")
			} else {
				c.alertf("BRIG_VERIFY=off, so the guest image is not checked before it boots")
			}
		}
		return nil
	}
	if c.Runtime.PinsDigest() {
		return c.verifyDigest()
	}
	// An alert rather than a warning, on the same rule as "could not check": it
	// is a caveat on the claim, not a note beside it. What verified and what
	// boots are not provably the same bytes here -- under the default pull
	// policy they need not be, which is the limitation docs/security.md
	// records -- so a reader told nothing would believe a digest was pinned
	// when none was. The upgrade advice in it is incidental; the substance is
	// that brig's guarantee is weaker on this host than it otherwise is.
	c.alertf("%s", (&Rows{}).
		Note("brig verifies and boots the tag, not a pinned digest").
		Do("to boot by digest, upgrade to hull 0.1.0-rc23 or newer", "").
		Block("this "+c.Runtime.Kind()+" cannot boot by digest"))
	return c.verifyTag()
}

// verifyTag is the tag-level check, kept for a runtime that cannot boot by
// digest. It is brig's original behaviour, unchanged: the tag is what cosign
// sees and what the runtime boots, and under the default pull policy those need
// not be the same bytes -- the limitation documented in docs/security.md.
func (c *Config) verifyTag() error {
	res := checkTag(c.VerifyPolicy, c.Image)

	switch res.Outcome {
	case verify.Verified:
		// The per-check detail narrates rather than warns: a signature that
		// checked out is nothing to act on, and one line per check on every
		// boot is noise. What the default run gets instead is
		// one summary line for the whole step, which is what recording it here
		// is for. See sayVerified.
		c.verified = append(c.verified, "image")
		c.progressf("%s", res.Message())
		return nil

	case verify.NotOurs, verify.NoTooling:
		if c.Verify == verify.Require {
			return errors.New(res.Refusal())
		}
		c.alertf("%s", res.Notice())
		return nil

	default:
		c.alertf("%s", res.Notice())
		if res.TimedOut {
			return c.cosignHung(res, "Boot it unverified?")
		}
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
	res := checkDigest(c.VerifyPolicy, c.Image, local)

	switch res.Outcome {
	case verify.Verified, verify.NotOurs:
		// Both boot the digest we resolved. A third party's carries no signature
		// of ours, but pinning the digest still boots the bytes the registry
		// serves rather than a stale local tag.
		c.BootDigest = res.Digest
		if res.Outcome == verify.NotOurs && c.Verify == verify.Require {
			return errors.New(res.Refusal())
		}
		// The two outcomes part company here. A signature that checked out is
		// nothing to act on and narrates; an image nobody claimed to publish is
		// a gap in what was checked, and that stays on screen.
		if res.Outcome == verify.Verified {
			c.verified = append(c.verified, "image")
			c.progressf("%s", res.Message())
			return nil
		}
		c.alertf("%s", res.Notice())
		return nil

	case verify.NoTooling:
		// Nothing could be checked or pinned, so the tag boots as given. Leaving
		// BootDigest empty is what makes that happen. A missing cosign is a gap
		// in the machine's setup, said so on every boot, not something that
		// changes between one run and the next.
		if c.Verify == verify.Require {
			return errors.New(res.Refusal())
		}
		c.alertf("%s", res.Notice())
		return nil

	case verify.Unresolved:
		// The registry could not be reached, so one of our own images could not
		// be checked. That is the outage case, and it stops exactly as it did
		// before the digest check existed, when the same outage failed inside
		// `cosign verify` and asked. A boot that goes ahead here on its own
		// would let anyone who can make the registry unreachable, a captive
		// portal or a sinkhole, turn the default mode into "unchecked". Nothing
		// is pinned either way: a yes boots the cached tag.
		c.alertf("%s", res.Notice())
		if res.TimedOut {
			return c.cosignHung(res, "Boot the cached copy unverified?")
		}
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
		c.alertf("%s", res.Notice())
		if !res.Ours {
			// A third party's copy differing from the registry is said and
			// booted, the same weight as NotOurs -- unless Require, which
			// trusts nothing it cannot positively verify.
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
		c.alertf("%s", res.Notice())
		if res.TimedOut {
			return c.cosignHung(res, "Boot it unverified?")
		}
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

// cosignHung decides an image check that cosign did not answer in time, on
// either path and at either step. It stops as the row it came from does: under
// require it refuses, otherwise it asks, and with nobody to ask it refuses.
//
// Only the words differ. No signature was read, so it does not say the image
// failed verification. A hung credential helper is a local cause, so it does
// not blame the registry. The detail goes in the error itself, so a caller that
// shows only the error still sees the credential helper cosign waited on when
// one is set.
func (c *Config) cosignHung(res verify.Result, question string) error {
	if c.Verify == verify.Require {
		return fmt.Errorf("refusing to boot an image that was not verified: %s "+
			"(BRIG_VERIFY=require)", res.Detail)
	}
	if !c.confirm(question) {
		return fmt.Errorf("aborted: the image was not verified: %s. Try again once "+
			"cosign answers, or set BRIG_VERIFY=off to boot it unchecked", res.Detail)
	}
	return nil
}

// sayVerified is the one line a default run prints about verification that
// held.
//
// It is the other half of the VERIFY row. The row names the policy before the
// sandbox boots, because that is all that is knowable then; this names the
// result after the checks have run. Without it a quiet run says nothing about
// verification at all, and "nothing was said" would have to be read as "it
// verified" -- an inference on an absence, about the one subject where a reader
// must never have to make one.
//
// A warning rather than narration, unlike the per-check lines it summarises.
// Those are detail about how the answer was reached and wait for --verbose;
// this is the answer, and the default run is where it belongs. -q drops it with
// everything else between an identifier and an error.
//
// One line for the whole step, not one per check. The image and the kernel are
// two checks under one policy, and a reader wants to know that what boots
// verified rather than to audit the checks -- which is also why nothing is said
// when nothing was positively checked. BRIG_VERIFY=off, an image nobody
// claimed to publish and a machine with no cosign have each already said so
// themselves, in the default output, and a run that added "verified" beside
// them would be false.
func (c *Config) sayVerified() {
	if len(c.verified) == 0 {
		return
	}
	c.warnf("%s verified", strings.Join(c.verified, " and "))
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
		c.alertf("%s", (&Rows{}).
			Do("to boot it regardless", "BRIG_VERIFY=off").
			Block("not a terminal, so there is nobody to ask: refusing"))
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
// than two. The signature covers the bundle's manifest, not the files on
// disk, so a signature that verified records the digest and checkBootDigests
// compares the files with it once they are resolved. The summary names the
// boot assets only after that comparison.
func (c *Config) verifyBootAssets() error {
	if c.Verify == verify.Off || !c.Profile.GenericBoot {
		return nil
	}
	// The Linux runtime bundle's kernel and initrd are its own, and the boot
	// bundle in the registry lists neither. Its release's signed record does,
	// and checkBootDigests checks that once the files are resolved, so a
	// signature on the boot bundle has nothing to say here.
	// The bundle runs on Linux alone. On another runtime, a SHA256SUMS in a
	// named directory is somebody's build and gets the boot bundle's check.
	if dir := os.Getenv("BRIG_BOOT_ASSETS"); dir != "" && c.Runtime.Kind() == "nerdctl" {
		// The runtime resolves the files from the absolute path, so the
		// record's directory is kept in the same form.
		if abs, err := filepath.Abs(dir); err == nil && verify.HasBundleRecord(abs) {
			c.bundleRecord = abs
			return nil
		}
	}
	ref := runtime.BootAssetsRef()
	// Its own identity and registry, but the same cosign binary: which tool to
	// run is a fact about the machine, set once by BRIG_COSIGN_BIN, and not
	// something each trust root should answer differently. Hardcoding it here
	// also made this unstubbable, so the check reached the real registry from a
	// unit test.
	policy := verify.BootAssetsPolicy()
	policy.Cosign = c.VerifyPolicy.Cosign
	res := checkDigest(policy, ref, "")

	switch res.Outcome {
	case verify.Verified:
		// Narration for the same reason the image's success line is: the
		// signature verified, and there is nothing here for anybody to do about
		// it. The summary waits for checkBootDigests, because the signature
		// vouches for a manifest and not yet for the files that boot.
		c.bundleRef, c.bundleDigest = ref, res.Digest
		c.progressf("boot assets %s: signature verified", ref)
		return nil

	case verify.NotOurs:
		// The bundle was pointed somewhere else, with BRIG_BOOT_ASSETS_REF or a
		// mirror. brig has nothing to check there and says so rather than
		// implying it checked.
		if c.Verify == verify.Require {
			return fmt.Errorf("refusing to boot: the boot assets at %s are not published "+
				"by brig, so their signature cannot be checked (BRIG_VERIFY=require). "+
				"Set BRIG_VERIFY=warn to boot them unchecked", ref)
		}
		c.alertf("%s", (&Rows{}).
			Note("nothing was checked about the kernel this sandbox boots").
			Block("boot assets "+ref+" are not published by brig"))
		return nil

	case verify.NoTooling, verify.Unresolved:
		// Could not check, rather than failed. It follows the image's rule: said
		// out loud at every level by default, a refusal under require.
		//
		// The cause is named here. res.Message() is written about an image and
		// about booting it anyway; the subject here is the kernel, and the warn
		// line says it boots, in the words the NotOurs case above uses.
		cause := c.VerifyPolicy.CosignMissing()
		if res.Outcome == verify.Unresolved {
			// Unresolved covers every reference the resolve did not answer: a
			// registry out of reach, a tag that is not there, one that needs
			// credentials. Detail carries which of them it was.
			cause = fmt.Sprintf("the reference could not be resolved: %s", res.Detail)
		}
		if c.Verify == verify.Require {
			return fmt.Errorf("refusing to boot: the boot assets at %s could not be "+
				"verified: %s (BRIG_VERIFY=require). Set BRIG_VERIFY=warn to boot "+
				"them unchecked", ref, cause)
		}
		c.alertf("%s", (&Rows{}).
			Note("nothing was checked about the kernel this sandbox boots").
			Block(fmt.Sprintf("the boot assets at %s could not be verified: %s", ref, cause)))
		return nil

	default:
		// A signature that is present and wrong on the kernel brig is about to
		// boot. This one stops whatever the mode, short of off: there is no
		// reading of a bad signature here that is worth a prompt.
		//
		// A check cosign did not answer stops the same way, in words that do not
		// call it a bad signature.
		if res.TimedOut {
			return fmt.Errorf("refusing to boot: the boot assets at %s were not verified (%s). "+
				"Set BRIG_VERIFY=off to boot them regardless", ref, res.Detail)
		}
		return fmt.Errorf("refusing to boot: the boot assets at %s failed verification (%s). "+
			"Set BRIG_VERIFY=off to boot them regardless", ref, res.Detail)
	}
}

// registryDigests reads a bundle's per-file digests from its registry
// manifest. A variable so a unit test reads a bundle without a registry.
var registryDigests = verify.BundleDigests

// checkBootDigests compares the kernel and initrd brig hands the runtime with
// the digests the verified bundle lists for them, before the boot (#234).
//
// The signature verifyBootAssets checked covers the bundle's manifest, and
// the manifest lists each file's sha256. The files on disk are what boots,
// and nothing tied them to that manifest: a kernel swapped in the asset
// directory, or left there from an older bundle, booted under a line saying
// the boot assets verified.
//
// Who chose the directory decides what a difference means. brig chose it and
// fetched into it when BRIG_BOOT_ASSETS is unset, so a file that differs has
// no innocent reading and refuses in every mode but off. A directory named in
// BRIG_BOOT_ASSETS is somebody's build, so warn states the difference and
// boots, and require refuses. Digests brig cannot read are "cannot check",
// as elsewhere: said under warn, a refusal under require.
//
// Only a signature that verified gets here. A bundle that is not brig's, or
// one brig cannot check, has already said so, and there is no digest to
// bind.
func (c *Config) checkBootDigests(assets runtime.BootAssets) error {
	if c.Verify == verify.Off || !c.Profile.GenericBoot {
		return nil
	}
	if c.bundleRecord != "" {
		return c.checkBundleRecord(assets)
	}
	if c.bundleDigest == "" {
		return nil
	}
	bundle := fmt.Sprintf("%s (%s)", c.bundleRef, c.bundleDigest)
	if assets.Kernel == "" || assets.Initrd == "" {
		return c.bootDigestsUnread(assets, bundle,
			fmt.Errorf("this %s finds the kernel and initrd inside its own run", c.Runtime.Kind()))
	}

	differ, source, fallback, err := compareBootDigests(c.bundleRef, c.bundleDigest, assets)
	// Files an earlier fetch left, of a bundle the tag named before it moved,
	// are fetched again: brig chose the directory and fetches into it, so
	// replacing them is its own business. A run after every publish refused
	// until somebody deleted them by hand.
	if differ != "" && c.staleBundle(assets) {
		if rerr := c.refetchBootAssets(assets); rerr != nil {
			return fmt.Errorf("refusing to boot: the boot assets in %s are an older bundle than the "+
				"one that verified, %s, and fetching that one failed: %v", filepath.Dir(assets.Kernel), bundle, rerr)
		}
		differ, source, fallback, err = compareBootDigests(c.bundleRef, c.bundleDigest, assets)
	}
	switch {
	case err != nil:
		return c.bootDigestsUnread(assets, bundle, err)
	case differ != "":
		return c.bootAssetsDiffer(assets, bundle, differ)
	}
	if fallback != nil {
		return c.bootDigestsUnread(assets, bundle, recordOnly(fallback))
	}
	c.verified = append(c.verified, "boot assets")
	c.progressf("boot assets: %s and %s match the digests %s lists for %s",
		filepath.Base(assets.Kernel), filepath.Base(assets.Initrd), source, c.bundleDigest)
	return nil
}

// verifiedBootRef is the boot bundle at the digest whose signature verified,
// or "" when none did.
func (c *Config) verifiedBootRef() string {
	if c.bundleDigest == "" {
		return ""
	}
	return verify.RefWithDigest(c.bundleRef, c.bundleDigest)
}

// compareBootDigests compares the kernel and initrd with what the verified
// bundle lists. differ says how they differ, empty when they match, and
// source names where the list came from. fallback is the registry's error
// when that list is hull's record, as expectedBootDigests returns it. err is
// a list brig could not read.
func compareBootDigests(ref, digest string, assets runtime.BootAssets) (differ, source string, fallback, err error) {
	expected, source, fallback, err := expectedBootDigests(ref, digest, filepath.Dir(assets.Kernel))
	var other *verify.OtherBundleError
	switch {
	case errors.As(err, &other):
		return other.Error(), "", nil, nil
	case err != nil:
		return "", "", nil, err
	}
	var lines []string
	for _, path := range []string{assets.Kernel, assets.Initrd} {
		name := filepath.Base(path)
		want := expected[name]
		if want == "" {
			return "", "", nil, fmt.Errorf("%s lists no digest for %s", source, name)
		}
		got, err := verify.FileDigest(path)
		if err != nil {
			return "", "", nil, err
		}
		if got != want {
			lines = append(lines, fmt.Sprintf("%s is %s, not the %s it lists", name, got, want))
		}
	}
	return strings.Join(lines, ". "), source, fallback, nil
}

// staleBundle reports whether the files are an older bundle's, in a directory
// brig chose: the record beside them names a bundle other than the one that
// verified, and they are the files it lists. Files that match no record are
// somebody's change, and stay a refusal.
func (c *Config) staleBundle(assets runtime.BootAssets) bool {
	if assets.Named {
		return false
	}
	recorded, files, err := verify.RecordedBundle(filepath.Dir(assets.Kernel))
	if err != nil || recorded == c.bundleDigest {
		return false
	}
	for _, path := range []string{assets.Kernel, assets.Initrd} {
		want := files[filepath.Base(path)]
		got, err := verify.FileDigest(path)
		if want == "" || err != nil || got != want {
			return false
		}
	}
	return true
}

// refetchBootAssets fetches the bundle that verified, by its digest, over an
// older bundle's files.
func (c *Config) refetchBootAssets(assets runtime.BootAssets) error {
	r, ok := c.Runtime.(runtime.BootResolver)
	if !ok {
		return fmt.Errorf("this %s cannot fetch them", c.Runtime.Kind())
	}
	c.progressf("boot assets in %s are an older bundle; fetching %s",
		filepath.Dir(assets.Kernel), c.verifiedBootRef())
	fresh, err := r.ResolveBootAssets(runtime.BootFetch{Ref: c.verifiedBootRef(), Replace: true},
		c.runtimeNotice(), c.runtimeOutput())
	if err != nil {
		return err
	}
	// The run boots the paths resolved before. A fetch that put the files
	// anywhere else leaves those paths holding the older bundle.
	if fresh.Kernel != assets.Kernel || fresh.Initrd != assets.Initrd {
		return fmt.Errorf("the fetch resolved %s and %s, not the files compared", fresh.Kernel, fresh.Initrd)
	}
	return nil
}

// expectedBootDigests reads what the verified bundle lists for its files: the
// registry manifest at that digest first, and hull's provenance record in dir
// when the manifest cannot be read. fallback is the registry's error when the
// list comes from the record, and nil when it comes from the registry. A
// record of another bundle comes back as verify.OtherBundleError, whatever the
// registry said.
//
// A registry that answered with a manifest brig refused gets no fallback. A
// record that agrees with the files would say nothing about the registry
// answering the signed digest with other bytes.
func expectedBootDigests(ref, digest, dir string) (files verify.BootDigests, source string, fallback, err error) {
	files, err = registryDigests(ref, digest)
	if err == nil {
		return files, "the registry manifest", nil, nil
	}
	var refused *verify.ManifestRefusedError
	if errors.As(err, &refused) {
		return nil, "", nil, fmt.Errorf("brig refused the registry's answer for it: %w", err)
	}
	recorded, perr := verify.ProvenanceDigests(dir, digest)
	if perr == nil {
		return recorded, "hull's provenance record", err, nil
	}
	var other *verify.OtherBundleError
	if errors.As(perr, &other) {
		return nil, "", nil, perr
	}
	return nil, "", nil, fmt.Errorf("the registry manifest could not be read (%v), and there is no provenance record for it in %s", err, dir)
}

// recordOnly is the "cannot check" cause for files that match hull's
// provenance record while the registry manifest could not be read. hull writes
// the record into the directory it describes, so anything that can rewrite the
// kernel there can rewrite the record to match, and a match is not a
// verification.
func recordOnly(registryErr error) error {
	return fmt.Errorf("the registry manifest could not be read (%v), and only hull's provenance "+
		"record lists them. That record sits beside the files, so it does not vouch for them", registryErr)
}

// bootAssetsDiffer decides a kernel or initrd that is not the file the
// verified bundle lists.
func (c *Config) bootAssetsDiffer(assets runtime.BootAssets, bundle, detail string) error {
	dir := filepath.Dir(assets.Kernel)
	if !assets.Named {
		// Deleting the files is the remedy on both runtimes: the resolve then
		// fetches the reference brig verified, hull pinned to it as well.
		return fmt.Errorf("refusing to boot: the boot assets in %s are not the bundle that "+
			"verified, %s: %s. Delete both files there and run again to fetch the bundle, or "+
			"set BRIG_BOOT_ASSETS to that directory if they are your own build", dir, bundle, detail)
	}
	if c.Verify == verify.Require {
		return fmt.Errorf("refusing to boot: BRIG_BOOT_ASSETS names %s, and its boot assets "+
			"are not the bundle that verified, %s: %s (BRIG_VERIFY=require).%s Set "+
			"BRIG_VERIFY=warn to boot them", dir, bundle, detail, c.linuxBundleNote(assets))
	}
	rows := (&Rows{}).Note("booting them as your own build, so nothing vouches for the kernel " +
		"this sandbox boots")
	if note := strings.TrimSpace(c.linuxBundleNote(assets)); note != "" {
		rows.Note("%s", strings.ToLower(note[:1])+note[1:])
	}
	c.alertf("%s", rows.Block(fmt.Sprintf("BRIG_BOOT_ASSETS names %s, and its boot assets "+
		"are not the bundle that verified, %s: %s", dir, bundle, detail)))
	return nil
}

// bootDigestsUnread decides digests brig cannot read, or files it cannot
// hash: "cannot check", said under warn and refused under require.
func (c *Config) bootDigestsUnread(assets runtime.BootAssets, bundle string, cause error) error {
	if c.Verify == verify.Require {
		return fmt.Errorf("refusing to boot: cannot read the digests of the boot bundle %s: %v, "+
			"so the kernel and initrd were not compared with it (BRIG_VERIFY=require).%s Set "+
			"BRIG_VERIFY=warn to boot them without the comparison", bundle, cause, c.linuxBundleNote(assets))
	}
	c.alertf("%s", (&Rows{}).
		Note("the kernel and initrd boot without being compared with it").
		Block(fmt.Sprintf("cannot read the digests of the boot bundle %s: %v", bundle, cause)))
	return nil
}

// linuxBundleNote says why a directory from an older Linux runtime bundle
// gets here. Its launcher sets BRIG_BOOT_ASSETS to the kernel and initrd it
// carries, and a bundle that keeps no signed record of them reaches the boot
// bundle's check, which lists neither.
//
// brig cannot tell that launcher's directory from anyone else's once it keeps
// no record, so the note is about the launcher, and only a named directory on
// nerdctl gets it. On hull the bundle plays no part, and a note about it sent
// a Mac user who named a build of their own looking in the wrong place.
func (c *Config) linuxBundleNote(assets runtime.BootAssets) string {
	if !assets.Named || c.Runtime.Kind() != "nerdctl" {
		return ""
	}
	return " The Linux runtime bundle's launcher sets BRIG_BOOT_ASSETS to the kernel and " +
		"initrd it carries, and a bundle this old keeps no signed record of them. Re-run " +
		"brig's install.sh to install one that does."
}

// checkBundleRecord checks the Linux runtime bundle's kernel and initrd
// against the record its release signed (#234).
//
// The bundle chose these files, not whoever runs brig, so a file the record
// does not list refuses in every mode but off, as a file in a directory brig
// chose does. So does a record the release's checksums.txt does not list, and
// a checksums.txt whose signature does not verify. The files are compared with
// the record first, since that needs no network: a record brig cannot check
// -- no cosign, no signature files beside it, no answer from Sigstore -- is
// "cannot check" only for files that match it. That is said under warn, and a
// refusal under require.
func (c *Config) checkBundleRecord(assets runtime.BootAssets) error {
	dir := c.bundleRecord
	if assets.Kernel == "" || assets.Initrd == "" {
		return c.bundleRecordUnread(dir,
			fmt.Errorf("this %s finds the kernel and initrd inside its own run", c.Runtime.Kind()))
	}
	for _, path := range []string{assets.Kernel, assets.Initrd} {
		if filepath.Dir(path) != filepath.Clean(dir) {
			return c.bundleRecordUnread(dir, fmt.Errorf("the runtime resolved %s, which is not beside the record", path))
		}
	}

	files, err := verify.RecordDigests(dir)
	if err != nil {
		return c.bundleRecordUnread(dir, err)
	}
	var differ []string
	for _, path := range []string{assets.Kernel, assets.Initrd} {
		name := filepath.Base(path)
		want := files[name]
		if want == "" {
			return c.bundleRecordDiffers(dir, fmt.Sprintf("the record lists no %s", name))
		}
		got, err := verify.FileDigest(path)
		if err != nil {
			return c.bundleRecordUnread(dir, err)
		}
		if got != want {
			differ = append(differ, fmt.Sprintf("%s is %s, not the %s it lists", name, got, want))
		}
	}
	if len(differ) > 0 {
		return c.bundleRecordDiffers(dir, strings.Join(differ, ". "))
	}

	rec, err := verify.ReadBundleRecord(dir)
	var unlisted *verify.UnlistedRecordError
	switch {
	case errors.As(err, &unlisted):
		return c.bundleRecordDiffers(dir, err.Error())
	case err != nil:
		return c.bundleRecordUnread(dir, err)
	}

	// Its own identity, and the same cosign binary, for the reason
	// verifyBootAssets gives.
	policy := c.RuntimePolicy
	if policy.Identity == "" || policy.Issuer == "" {
		policy = verify.RuntimeBundlePolicy()
	}
	policy.Cosign = c.VerifyPolicy.Cosign
	switch res := policy.Blob(rec.Checksums, rec.Signature, rec.Cert); res.Outcome {
	case verify.Verified:
	case verify.NoTooling:
		return c.bundleRecordUnread(dir, errors.New(c.VerifyPolicy.CosignMissing()))
	case verify.Unresolved:
		return c.bundleRecordUnread(dir,
			fmt.Errorf("cosign could not reach Sigstore to check the release's signature: %s", res.Detail))
	default:
		return c.bundleRecordDiffers(dir,
			fmt.Sprintf("the signature on the release's checksums.txt did not verify (%s); a bundle "+
				"released from a fork needs BRIG_VERIFY_RUNTIME_IDENTITY set to its release workflow", res.Detail))
	}
	c.verified = append(c.verified, "boot assets")
	c.progressf("boot assets: %s and %s match the runtime bundle's signed record, %s",
		filepath.Base(assets.Kernel), filepath.Base(assets.Initrd), rec.Release)
	return nil
}

// bundleRecordDiffers refuses a runtime bundle's kernel or initrd that its
// signed record does not vouch for.
func (c *Config) bundleRecordDiffers(dir, detail string) error {
	return fmt.Errorf("refusing to boot: the kernel and initrd in %s are not the ones the "+
		"Linux runtime bundle's signed record lists: %s. Re-run brig's install.sh to "+
		"reinstall the bundle, or point BRIG_BOOT_ASSETS at a directory of your own build",
		dir, detail)
}

// bundleRecordUnread decides a runtime bundle's record that brig cannot
// check: said under warn, refused under require.
func (c *Config) bundleRecordUnread(dir string, cause error) error {
	if c.Verify == verify.Require {
		return fmt.Errorf("refusing to boot: cannot check the Linux runtime bundle's kernel "+
			"and initrd in %s against its signed record: %v (BRIG_VERIFY=require). Set "+
			"BRIG_VERIFY=warn to boot them without the check", dir, cause)
	}
	c.alertf("%s", (&Rows{}).
		Note("they boot without the check").
		Block(fmt.Sprintf("cannot check the Linux runtime bundle's kernel and initrd in %s "+
			"against its signed record: %v", dir, cause)))
	return nil
}
