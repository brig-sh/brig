package wrap

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/ttytest"
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
//
// The assertion on the "not a terminal" line is what makes this test about
// that branch. Without it the test passed for the wrong reason: stdin under
// `go test` is /dev/null, the old IsTerminal called that a terminal, and the
// refusal came from the read hitting EOF rather than from there being nobody
// to ask -- so a confirm() mutated to fail open still passed. It goes red now.
func TestVerifyRefusesAFailedSignatureWithNoTerminal(t *testing.T) {
	// Said out loud rather than assumed: stdin under `go test` is not a
	// terminal, and this test is about what happens when it is not.
	if IsTerminal(os.Stdin) {
		t.Skip("this test needs a stdin that is not a terminal")
	}
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
	if !strings.Contains(said, "not a terminal") {
		t.Errorf("the refusal did not come from the no-terminal branch: %q", said)
	}
}

// The same refusal, from the other direction: /dev/null is a character device,
// which is what the old check tested and why it counted as somebody to ask.
func TestVerifyRefusesAFailedSignatureWithStdinOnDevNull(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	old := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = old }()

	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.VerifyPolicy.Cosign = "false"
	if err := c.verifyImage(); err == nil {
		t.Fatal("a failed signature was booted with stdin on /dev/null")
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "not a terminal") {
		t.Errorf("/dev/null was taken for somebody to ask: %q", said)
	}
}

// A caller with no terminal of its own is taken at its word, even sitting on a
// real one. This is brigd's case: its stdin may well be a terminal, because it
// was started from a shell, but that terminal belongs to whoever started the
// daemon and not to the client whose request raised the question -- so asking
// there puts a question in front of nobody while the client waits for an
// answer that cannot come.
func TestVerifyDoesNotAskWhenTheCallerHasNoTerminal(t *testing.T) {
	// A real terminal on stdin, with an answer already waiting on it: the
	// point is that neither is looked at.
	master := ttytest.AsStdin(t)
	if _, err := master.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
	c.VerifyPolicy.Cosign = "false" // present, and always exits non-zero
	c.NoTerminal = true

	err := c.verifyImage()
	if err == nil {
		t.Fatal("a failed signature was booted on a terminal the caller does not have")
	}
	if !strings.Contains(err.Error(), "BRIG_VERIFY=off") {
		t.Errorf("the refusal did not name the override: %v", err)
	}
	said := c.Err.(*bytes.Buffer).String()
	if strings.Contains(said, "Boot it anyway?") {
		t.Errorf("the question was asked anyway: %q", said)
	}
	if !strings.Contains(said, "nobody to ask") {
		t.Errorf("the refusal did not say why nobody was asked: %q", said)
	}
}

// And with a real terminal the question is actually asked and the answer
// actually read. This is the half the no-terminal tests cannot prove: a
// confirm() that always refused would satisfy every assertion above.
func TestVerifyAsksWhenThereIsSomebodyToAsk(t *testing.T) {
	for _, tc := range []struct {
		answer string
		booted bool
	}{{"y\n", true}, {"n\n", false}, {"\n", false}} {
		master := ttytest.AsStdin(t)
		if _, err := master.WriteString(tc.answer); err != nil {
			t.Fatal(err)
		}
		c := verifyConfig(t, "ghcr.io/brig-sh/claude-code:arm64", verify.Warn)
		c.VerifyPolicy.Cosign = "false"
		err := c.verifyImage()
		if booted := err == nil; booted != tc.booted {
			t.Errorf("answering %q: booted = %v, want %v (err %v)", tc.answer, booted, tc.booted, err)
		}
		if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "Boot it anyway?") {
			t.Errorf("answering %q: the question was never asked: %q", tc.answer, said)
		}
	}
}
