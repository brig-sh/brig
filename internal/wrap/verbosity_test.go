package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/verify"
)

// levelled builds a Config whose three writers a test can read back, at the
// level given. Nothing here boots anything: what is under test is which of the
// print helpers writes at which level.
func levelled(v Verbosity) (*Config, *bytes.Buffer, *bytes.Buffer) {
	out, progress := &bytes.Buffer{}, &bytes.Buffer{}
	return &Config{Out: out, Err: out, Progress: progress, Verbosity: v}, out, progress
}

// The rule the issue sets: by default, print what the user has to act on. A
// warning is an action, so it stays in the default output.
func TestWarningsStayInTheDefaultOutput(t *testing.T) {
	c, said, _ := levelled(Normal)
	c.warnf("the host's credential has expired")
	if !strings.Contains(said.String(), "expired") {
		t.Errorf("a warning was dropped from the default output: %q", said.String())
	}
}

// -q is identifiers and errors only, so a warning is not printed there: an
// error is returned rather than warned about, and everything between the two
// is what a script did not ask for.
func TestQuietDropsWarnings(t *testing.T) {
	c, said, _ := levelled(Quiet)
	c.warnf("the host's credential has expired")
	if said.Len() != 0 {
		t.Errorf("-q printed a warning: %q", said.String())
	}
}

// brig's own narration -- the line saying a boot has started -- is not an
// action, so it waits to be asked for.
func TestProgressNarrationWaitsForVerbose(t *testing.T) {
	c, _, progress := levelled(Normal)
	c.progressf("starting sandbox %s...", "brig-claude-code")
	if progress.Len() != 0 {
		t.Errorf("the default output narrated a boot: %q", progress.String())
	}

	c, _, progress = levelled(Verbose)
	c.progressf("starting sandbox %s...", "brig-claude-code")
	if !strings.Contains(progress.String(), "starting sandbox") {
		t.Errorf("--verbose did not print the narration: %q", progress.String())
	}
}

// The report `brig info` prints is the answer to the command rather than
// narration about one, so it is not levelled: asking for it by name is the
// whole of the request.
func TestTheReportIsNotLevelled(t *testing.T) {
	for _, v := range []Verbosity{Quiet, Normal, Verbose} {
		c, said, _ := levelled(v)
		c.sayf("image %s (pull %s)", "img", "missing")
		if !strings.Contains(said.String(), "image img") {
			t.Errorf("verbosity %d dropped a line of the report: %q", v, said.String())
		}
	}
}

// What the runtime says is handed a writer only when somebody asked to see it.
// Otherwise the runtime holds it and quotes it back if the boot fails, which is
// what a nil writer asks for.
func TestTheRuntimeGetsAWriterOnlyUnderVerbose(t *testing.T) {
	for _, tc := range []struct {
		level Verbosity
		want  bool
	}{{Quiet, false}, {Normal, false}, {Verbose, true}} {
		c, _, _ := levelled(tc.level)
		if got := c.runtimeOutput() != nil; got != tc.want {
			t.Errorf("verbosity %d: runtime writer %v, want %v", tc.level, got, tc.want)
		}
	}
}

// One long operation gets one line whether or not anyone asked for detail: a
// download that takes a minute has to say so, or the terminal looks hung. Only
// -q, which is reading a script's output, drops it.
func TestALongOperationAnnouncesItselfByDefault(t *testing.T) {
	for _, tc := range []struct {
		level Verbosity
		want  bool
	}{{Quiet, false}, {Normal, true}, {Verbose, true}} {
		c, _, _ := levelled(tc.level)
		if got := c.runtimeNotice() != nil; got != tc.want {
			t.Errorf("verbosity %d: notice writer %v, want %v", tc.level, got, tc.want)
		}
	}
}

// Normal is the zero value, so a Config built by hand -- every one in these
// tests, and brigd's -- is at the level a person reads rather than silent.
func TestNormalIsTheZeroValue(t *testing.T) {
	var v Verbosity
	if v != Normal {
		t.Errorf("the zero Verbosity is %d, want Normal", v)
	}
}

// The VERIFY row names the policy this run boots under, for every mode.
//
// It is the half of verification that is knowable when the envelope is
// printed: the block goes out before the sandbox boots, which is the point of
// it, and the checks happen afterwards inside EnsureRunning. See verifyLine.
func TestTheEnvelopeNamesTheVerifyMode(t *testing.T) {
	for _, mode := range []verify.Mode{verify.Require, verify.Warn, verify.Off} {
		c := testConfig(t, t.TempDir(), t.TempDir())
		c.Verify = mode
		block := &bytes.Buffer{}
		c.renderEnvelope(block, creds.Set{})

		got := block.String()
		if !strings.Contains(got, "VERIFY ") {
			t.Errorf("%s: the envelope has no VERIFY row:\n%s", mode, got)
			continue
		}
		if !strings.Contains(got, string(mode)) {
			t.Errorf("%s: the VERIFY row does not name the mode:\n%s", mode, got)
		}
	}
}

// A replaced trust policy is the state the row most has to carry: the check
// still runs and still reports success, and it is no longer brig's check.
func TestTheVerifyRowSaysWhenThePolicyIsNotBrigsOwn(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Verify = verify.Warn
	c.VerifyPolicy = verify.Policy{Registry: "ghcr.io/someone-else"}
	if !c.VerifyPolicy.Replaced() {
		t.Fatal("the fixture policy does not read as replaced; the test proves nothing")
	}

	if got := c.verifyLine(); !strings.Contains(got, "replaced trust policy") {
		t.Errorf("the VERIFY row does not say the policy is not brig's: %q", got)
	}
}

// The outcome, which the row cannot carry. A default run states that
// verification held, in one line, so that a quiet run is not asking the reader
// to infer it from an absence.
func TestADefaultRunSaysVerificationHeld(t *testing.T) {
	c, said, _ := levelled(Normal)
	c.verified = []string{"image"}
	c.sayVerified()
	if !strings.Contains(said.String(), "image verified") {
		t.Errorf("a default run said nothing about verification holding: %q", said.String())
	}
}

// One line for the whole step, however many checks reached it: a reader wants
// to know that what boots verified, not to audit the checks.
func TestTheOutcomeIsOneLineForEveryCheck(t *testing.T) {
	c, said, _ := levelled(Normal)
	c.verified = []string{"image", "boot assets"}
	c.sayVerified()
	if got := strings.Count(said.String(), "\n"); got != 1 {
		t.Errorf("the outcome took %d lines, want 1: %q", got, said.String())
	}
	if !strings.Contains(said.String(), "image and boot assets verified") {
		t.Errorf("the outcome does not name both checks: %q", said.String())
	}
}

// And nothing is said when nothing was positively checked. BRIG_VERIFY=off, an
// image nobody claimed to publish and a machine with no cosign each say so
// themselves, in the default output; "verified" beside any of them is false.
func TestNothingIsSaidWhenNothingVerified(t *testing.T) {
	c, said, _ := levelled(Normal)
	c.sayVerified()
	if said.Len() != 0 {
		t.Errorf("a run that verified nothing claimed it had: %q", said.String())
	}
}

// -q takes the outcome with everything else between an identifier and an
// error; --verbose keeps it and adds the per-check detail it summarises.
func TestTheOutcomeFollowsTheLevel(t *testing.T) {
	c, said, _ := levelled(Quiet)
	c.verified = []string{"image"}
	c.sayVerified()
	if said.Len() != 0 {
		t.Errorf("-q printed the verification outcome: %q", said.String())
	}

	c, said, progress := levelled(Verbose)
	c.verified = []string{"image"}
	c.progressf("image ghcr.io/x: signature verified")
	c.sayVerified()
	if !strings.Contains(said.String(), "image verified") {
		t.Errorf("--verbose dropped the outcome: %q", said.String())
	}
	if !strings.Contains(progress.String(), "signature verified") {
		t.Errorf("--verbose dropped the per-check detail: %q", progress.String())
	}
}

// `brig info` does not boot, so it has no outcome to report: the row appears
// and the line does not. That asymmetry is the report being honest about what
// it knows rather than a gap in it -- the command answers what a run WOULD
// trust, and whether a signature holds is not established until something
// checks it.
func TestInfoShowsTheModeAndNoOutcome(t *testing.T) {
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Verify = verify.Require
	report, said := &bytes.Buffer{}, &bytes.Buffer{}
	c.Out, c.Err = report, said

	c.Info(creds.Set{})

	if !strings.Contains(report.String(), "VERIFY ") {
		t.Errorf("brig info has no VERIFY row:\n%s", report.String())
	}
	if !strings.Contains(report.String(), "image verification: require") {
		t.Errorf("brig info dropped the verification line from the report:\n%s", report.String())
	}
	for _, w := range []*bytes.Buffer{report, said} {
		if strings.Contains(w.String(), "image verified") {
			t.Errorf("brig info claimed an outcome it never established:\n%s", w.String())
		}
	}
}

// The envelope is `brig info`'s output whatever level a run asks for. It is the
// answer to a command asked by name, which is the same reason the report it
// heads is not levelled -- see sayf. Only a RUN decides whether to print it,
// and cmd/brig makes that decision; the block itself never goes quiet.
func TestInfoPrintsTheEnvelopeAtEveryLevel(t *testing.T) {
	for _, level := range []Verbosity{Quiet, Normal, Verbose} {
		c := testConfig(t, t.TempDir(), t.TempDir())
		c.Verbosity = level
		report := &bytes.Buffer{}
		c.Out = report

		c.Info(creds.Set{})

		if !strings.Contains(report.String(), "SANDBOX ") {
			t.Errorf("level %d: brig info dropped the envelope:\n%s", level, report.String())
		}
	}
}
