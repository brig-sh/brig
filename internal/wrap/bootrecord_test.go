package wrap

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/verify"
)

// The record of a verified boot names the digest the runtime was told to
// boot and the trust policy the signature checked out under.
func TestBootRecordNamesTheVerifiedDigestAndItsTrust(t *testing.T) {
	cosign := fakeCosign(t, testDigest, false)
	c := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Require, cosign, testDigest)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a verified image was refused: %v", err)
	}
	rec := c.bootRecord(time.Now())
	if rec.Image.Ref != "ghcr.io/brig-sh/claude-code:arm64" || rec.Image.Digest != testDigest {
		t.Errorf("image = %+v, want the ref and the digest that booted", rec.Image)
	}
	v := rec.Image.Verification
	if v.Mode != "require" || v.Outcome != "verified" {
		t.Errorf("verification = %+v, want require and verified", v)
	}
	shipped := verify.DefaultPolicy()
	if v.Trust == nil || v.Trust.Registry != shipped.Registry || v.Trust.Identity != shipped.Identity ||
		v.Trust.Issuer != shipped.Issuer || v.Trust.Replaced {
		t.Errorf("trust = %+v, want the shipped policy, not replaced", v.Trust)
	}
}

// An image nobody claimed to publish boots under warn, and its record must
// not read as a verification: no digest brig vouched for on the tag path, and
// an outcome that says what the check found.
func TestBootRecordDoesNotCallAThirdPartyImageVerified(t *testing.T) {
	c := verifyConfig(t, "docker.io/library/ubuntu:24.04", verify.Warn)
	if err := c.verifyImage(); err != nil {
		t.Fatalf("a third-party image was refused under warn: %v", err)
	}
	rec := c.bootRecord(time.Now())
	if got := rec.Image.Verification.Outcome; got == "verified" || got == "" {
		t.Errorf("outcome = %q, want what the check found and never verified", got)
	}
	if rec.Image.Digest != "" {
		t.Errorf("digest = %q on the tag path, want none", rec.Image.Digest)
	}
}

// A failed signature boots only when the person at the terminal says yes,
// and then the record keeps the failure rather than the boot's success.
func TestBootRecordKeepsTheOutcomeOfAFailedSignature(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	stubCheck(t, verify.Result{Outcome: verify.Failed, Ours: true, Digest: testDigest})
	_ = c.verifyImage()
	if got := c.bootRecord(time.Now()).Image.Verification.Outcome; got != "failed" {
		t.Errorf("outcome = %q, want failed", got)
	}
}

// A policy the user replaced is recorded as replaced, so a reader can tell a
// check that accepts one signer from one that accepts another.
func TestBootRecordSaysWhenTheTrustPolicyWasReplaced(t *testing.T) {
	cosign := fakeCosign(t, testDigest, false)
	c := digestConfig(t, "registry.example.com/team/agent:1", verify.Require, cosign, testDigest)
	c.VerifyPolicy.Registry = "registry.example.com/team/"
	c.VerifyPolicy.Identity = `^https://ci\.example\.com/team/agent$`
	if err := c.verifyImage(); err != nil {
		t.Fatalf("an image under the replaced policy was refused: %v", err)
	}
	trust := c.bootRecord(time.Now()).Image.Verification.Trust
	if trust == nil || !trust.Replaced || trust.Registry != "registry.example.com/team/" {
		t.Errorf("trust = %+v, want the replaced policy marked replaced", trust)
	}
}

// BRIG_VERIFY=off checked nothing, and the record says so rather than naming
// a trust policy nothing was held to.
func TestBootRecordUnderOffClaimsNoCheck(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	if err := c.verifyImage(); err != nil {
		t.Fatal(err)
	}
	v := c.bootRecord(time.Now()).Image.Verification
	if v.Mode != "off" || v.Outcome != "unchecked" || v.Trust != nil {
		t.Errorf("verification = %+v, want off, unchecked and no trust", v)
	}
}

func egressPolicy(name string, hosts ...string) policy.Policy {
	var p policy.Policy
	p.APIVersion, p.Kind = policy.APIVersion, "Policy"
	p.Metadata.Name = name
	e := &policy.Egress{Default: "deny"}
	for _, h := range hosts {
		e.Allow = append(e.Allow, policy.Rule{Host: h})
	}
	p.Spec.Egress = e
	return p
}

// Each bound policy is named with a digest of its rules, in the order bound,
// and a rule changed since gives another digest.
func TestBootRecordDigestsTheBoundPolicies(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	c.policyDocs = []policy.Policy{egressPolicy("github", "github.com"), egressPolicy("npm", "registry.npmjs.org")}
	got := c.bootRecord(time.Now()).Policies
	if len(got) != 2 || got[0].Name != "github" || got[1].Name != "npm" {
		t.Fatalf("policies = %+v, want github then npm", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p.Digest, "sha256:") || len(p.Digest) != len("sha256:")+64 {
			t.Errorf("policy %s digest = %q, want a sha256", p.Name, p.Digest)
		}
	}
	c.policyDocs[0] = egressPolicy("github", "github.com", "example.com")
	if again := c.bootRecord(time.Now()).Policies; again[0].Digest == got[0].Digest {
		t.Error("a policy with another rule kept its digest")
	}
}

// No policy is an empty list, so a reader never has to tell null from none.
func TestBootRecordWithNoPolicyListsNone(t *testing.T) {
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	blob, err := json.Marshal(c.bootRecord(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"policies":[]`) {
		t.Errorf("record = %s, want an empty policies list", blob)
	}
}

// The record is kept per sandbox, read back by ls, and goes with the sandbox.
func TestBootRecordsAreKeptPrunedAndForgotten(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	a := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Off)
	a.VMName = "brig-a"
	b := verifyConfig(t, "ghcr.io/brig-sh/codex:arm64", verify.Off)
	b.VMName = "brig-b"
	a.recordBoot()
	b.recordBoot()
	all := BootRecords()
	if all["brig-a"].Image.Ref != a.Image || all["brig-b"].Image.Ref != b.Image {
		t.Fatalf("records = %+v, want one per sandbox", all)
	}
	PruneBoots([]string{"brig-a"})
	if _, ok := BootRecords()["brig-b"]; ok {
		t.Error("the record of a sandbox the runtime no longer lists was kept")
	}
	ForgetBoot("brig-a")
	if len(BootRecords()) != 0 {
		t.Errorf("records = %+v after the last sandbox was forgotten", BootRecords())
	}
}

// A boot refused by verification, or one the runtime failed, records
// nothing: the record says what booted, and nothing did.
func TestARefusedOrFailedBootRecordsNothing(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	refused := verifyConfig(t, "docker.io/library/ubuntu:24.04", verify.Require)
	refused.Workspace = t.TempDir()
	refused.Cwd = refused.Workspace
	if err := refused.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("require booted a third-party image")
	}
	cosign := fakeCosign(t, testDigest, false)
	failed := digestConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Require, cosign, testDigest)
	failed.Workspace = t.TempDir()
	// An image that boots its own kernel, so the run reaches the runtime.
	failed.Profile.GenericBoot = false
	failed.Cwd = failed.Workspace
	if err := failed.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "not booting") {
		t.Fatalf("err = %v, want the stub runtime's failure to boot", err)
	}
	if got := BootRecords(); len(got) != 0 {
		t.Errorf("records = %+v, want none for boots that did not happen", got)
	}
}
