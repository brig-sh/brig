package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/verify"
)

func verifyConfig(t *testing.T, image string, mode verify.Mode) *Config {
	t.Helper()
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Image = image
	c.Verify = mode
	c.VerifyPolicy = verify.DefaultPolicy()
	// No cosign on the machine running the tests, which is itself one of the
	// cases worth pinning.
	c.VerifyPolicy.Cosign = "cosign-that-does-not-exist"
	return c
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
func TestVerifyRefusesAFailedSignatureWithNoTerminal(t *testing.T) {
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
}
