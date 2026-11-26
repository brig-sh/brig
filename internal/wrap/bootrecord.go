package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/verify"
)

// The boot record: what each sandbox was booted with, keyed by sandbox name.
//
// The run says what verification found and which policies apply, and then the
// terminal scrolls away. A person who comes back to a running sandbox, or a
// program that watches a host's sandboxes, had no way to ask which image
// digest is running in it, whether its signature checked out, under which
// trust policy, or which policies were bound when it booted. `brig --json ls`
// answers that from this record.
//
// Written once per boot and nowhere else, the way the network posture is: a
// command that finds the sandbox up did not boot it, and recording what that
// command would have checked would describe a boot that never happened. A
// policy edited since is not in force until the next boot, so the record keeps
// the digest of the document as it was bound, not the file as it is now.
//
// A file of its own, for the reason networks.json gives: an older release
// rewrites sessions.json with the fields it knows. Like sessions.json there is
// no lock, and a write lost to a race costs only the record, which the next
// boot of that sandbox writes again.

// bootRecordName is the file, inside stateDir.
const bootRecordName = "boots.json"

// BootRecord is what one sandbox was booted with.
type BootRecord struct {
	// BootedAt is when the runtime reported the boot started, in UTC.
	BootedAt time.Time `json:"bootedAt"`
	Image    BootImage `json:"image"`
	// BootAssets is the kernel and initrd check of a profile that boots a
	// downloaded bundle, and nil for one that boots its own image.
	BootAssets *BootAssets `json:"bootAssets,omitempty"`
	// Policies are the policies bound to the sandbox when it booted, in the
	// order they were merged. Empty, never null, when none was.
	Policies []BootPolicy `json:"policies"`
}

// BootImage is the guest image as it was checked and booted.
type BootImage struct {
	// Ref is the reference the run named.
	Ref string `json:"ref"`
	// Digest is the digest the runtime was told to boot. Omitted when the tag
	// booted: a runtime that cannot boot by digest, or a check that resolved
	// none. A digest here is one brig handed the runtime, never one it read
	// back.
	Digest       string           `json:"digest,omitempty"`
	Verification BootVerification `json:"verification"`
}

// BootVerification is the signature decision the boot went ahead under.
type BootVerification struct {
	// Mode is BRIG_VERIFY: off, warn or require.
	Mode string `json:"mode"`
	// Outcome is what the check found: verified, not-ours, no-tooling,
	// unresolved, mismatch or failed, and unchecked under off. Under require
	// only verified boots. Any other outcome under warn booted because warn
	// boots it, or because the person at the terminal said yes.
	Outcome string `json:"outcome"`
	// Trust is the policy the check ran under, omitted when nothing was
	// checked.
	Trust *BootTrust `json:"trust,omitempty"`
}

// BootTrust is the trust policy a check ran under: which registry prefix
// counts as the publisher's, and which signer identity and issuer it accepts.
type BootTrust struct {
	Registry string `json:"registry"`
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
	// Replaced is set when BRIG_VERIFY_REGISTRY, BRIG_VERIFY_IDENTITY or
	// BRIG_VERIFY_ISSUER replaced the policy brig ships.
	Replaced bool `json:"replaced"`
}

// BootAssets is the kernel and initrd check of a genericBoot profile.
type BootAssets struct {
	// Verified is set when the bundle's signature verified and the files that
	// booted matched it.
	Verified bool `json:"verified"`
	// Digest is the bundle digest whose signature verified, omitted when none
	// did.
	Digest string `json:"digest,omitempty"`
}

// BootPolicy is one policy bound to the sandbox when it booted.
type BootPolicy struct {
	Name string `json:"name"`
	// Digest is the sha256 of the document as brig parsed it, in its JSON
	// form, so two files that parse to the same policy share it and an edit
	// that changes a rule changes it.
	Digest string `json:"digest"`
}

// outcomeWords are the record's words for each outcome.
var outcomeWords = map[verify.Outcome]string{
	verify.Verified:   "verified",
	verify.NotOurs:    "not-ours",
	verify.NoTooling:  "no-tooling",
	verify.Failed:     "failed",
	verify.Mismatch:   "mismatch",
	verify.Unresolved: "unresolved",
}

// bootRecord is the record of the boot this run is about to make.
func (c *Config) bootRecord(now time.Time) BootRecord {
	rec := BootRecord{
		BootedAt: now.UTC(),
		Image: BootImage{
			Ref:          c.Image,
			Digest:       c.BootDigest,
			Verification: BootVerification{Mode: string(c.Verify), Outcome: "unchecked"},
		},
		Policies: policyDigests(c.policyDocs),
	}
	if res := c.imageCheck; res != nil {
		rec.Image.Verification.Outcome = outcomeWords[res.Outcome]
		p := res.Policy
		rec.Image.Verification.Trust = &BootTrust{Registry: p.Registry, Identity: p.Identity,
			Issuer: p.Issuer, Replaced: p.Replaced()}
	}
	if c.Profile.GenericBoot && c.Verify != verify.Off {
		rec.BootAssets = &BootAssets{Verified: slices.Contains(c.verified, "boot assets"),
			Digest: c.bundleDigest}
	}
	return rec
}

// policyDigests names each bound policy with the digest of its document.
func policyDigests(docs []policy.Policy) []BootPolicy {
	out := make([]BootPolicy, 0, len(docs))
	for _, d := range docs {
		digest := ""
		if blob, err := json.Marshal(d); err == nil {
			sum := sha256.Sum256(blob)
			digest = "sha256:" + hex.EncodeToString(sum[:])
		}
		out = append(out, BootPolicy{Name: d.Metadata.Name, Digest: digest})
	}
	return out
}

// recordBoot records what this run booted. A warning when the write fails:
// the sandbox is up, and only `brig ls` loses the record.
func (c *Config) recordBoot() {
	all := readIndex[BootRecord](bootRecordName)
	all[c.VMName] = c.bootRecord(time.Now())
	if err := writeIndex(bootRecordName, all); err != nil {
		c.warnf("could not record what %s was booted with: %v", c.VMName, err)
	}
}

// BootRecords is every sandbox's boot record, keyed by sandbox name. A
// sandbox booted by an older release has none.
func BootRecords() map[string]BootRecord {
	return readIndex[BootRecord](bootRecordName)
}

// ForgetBoot drops the record of a removed sandbox. Errors are dropped, as
// ForgetBootedNet drops its own.
func ForgetBoot(name string) {
	all := readIndex[BootRecord](bootRecordName)
	if _, ok := all[name]; !ok {
		return
	}
	delete(all, name)
	_ = writeIndex(bootRecordName, all)
}

// PruneBoots drops the record of every sandbox not in keep, so a sandbox
// removed outside brig does not hand its record to the next one of its name.
func PruneBoots(keep []string) {
	all := readIndex[BootRecord](bootRecordName)
	if len(all) == 0 {
		return
	}
	pruned := false
	for name := range all {
		if !slices.Contains(keep, name) {
			delete(all, name)
			pruned = true
		}
	}
	if pruned {
		_ = writeIndex(bootRecordName, all)
	}
}
