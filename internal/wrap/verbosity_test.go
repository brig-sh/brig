package wrap

import (
	"bytes"
	"strings"
	"testing"
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
