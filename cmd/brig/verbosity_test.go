package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/wrap"
)

// atLevel pins the package's verbosity for one test and puts it back, so a
// test that reads a notice does not decide whether the next one sees any.
func atLevel(t *testing.T, v wrap.Verbosity) {
	t.Helper()
	old := verbosity
	t.Cleanup(func() { verbosity = old })
	verbosity = v
}

// --verbose and -q are read left of the verb. Both are facts about the whole
// invocation rather than about one run line.
func TestGlobalPositionReadsVerboseAndQuiet(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want wrap.Verbosity
		rest string
	}{
		{[]string{"--verbose", "run", "claude"}, wrap.Verbose, "run claude"},
		{[]string{"--quiet", "run", "claude"}, wrap.Quiet, "run claude"},
		{[]string{"-q", "ls"}, wrap.Quiet, "ls"},
		{[]string{"run", "claude"}, wrap.Normal, "run claude"},
	} {
		g, rest, err := parseGlobal(tc.args)
		if err != nil {
			t.Errorf("parseGlobal(%q): %v", tc.args, err)
			continue
		}
		if g.verbosity() != tc.want {
			t.Errorf("parseGlobal(%q) = level %d, want %d", tc.args, g.verbosity(), tc.want)
		}
		if strings.Join(rest, " ") != tc.rest {
			t.Errorf("parseGlobal(%q) left %q, want %q", tc.args, rest, tc.rest)
		}
	}
}

// -v is not brig's. It is Claude Code's version flag, codex's verbose flag and
// Docker's volume flag, so brig owns none of those readings and does not claim
// the letter -- left of the verb it is an unknown token, and right of the ref
// it is the agent's word, untouched.
func TestVerboseHasNoShortForm(t *testing.T) {
	_, _, err := parseGlobal([]string{"-v", "run", "claude"})
	if err == nil {
		t.Fatal("-v was accepted as a brig flag")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("parseGlobal(-v): %v, want a usageError", err)
	}

	// And after the ref it reaches the agent rather than being read.
	_, _, tail, parseErr := parse("run", []string{"claude", "-v"})
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}
	if strings.Join(tail, " ") != "-v" {
		t.Errorf("tail = %q, want -v handed to the agent", tail)
	}
}

// Asking to be told more and less at once is a mistake worth naming: either
// winner would leave the line reading like it asked for the opposite.
func TestVerboseAndQuietTogetherAreRefused(t *testing.T) {
	if _, _, err := parseGlobal([]string{"--quiet", "--verbose", "run", "claude"}); err == nil {
		t.Error("--quiet with --verbose was accepted")
	}
}

// -q moved to the global position, and moving it must not break the line it
// works on today: `brig run claude -q` still means quiet, and says where the
// flag lives now. The run-line spelling goes in v0.3.
func TestQuietOnTheRunLineStillWorksAndSaysWhereItMoved(t *testing.T) {
	atLevel(t, wrap.Normal)
	for _, args := range [][]string{{"claude", "-q"}, {"claude", "--quiet"}} {
		var o options
		said := captureStderr(t, func() {
			var err error
			o, _, _, err = parse("run", args)
			if err != nil {
				t.Fatalf("parse(%q): %v", args, err)
			}
		})
		if !o.quiet {
			t.Errorf("parse(%q) did not read the flag", args)
		}
		if !strings.Contains(said, "brig -q") {
			t.Errorf("parse(%q) said %q, want a notice naming the global position", args, said)
		}
	}
}

// The global spelling is where the flag lives now, so it says nothing.
func TestQuietInTheGlobalPositionIsNotDeprecated(t *testing.T) {
	atLevel(t, wrap.Normal)
	said := captureStderr(t, func() {
		if _, _, err := parseGlobal([]string{"-q", "run", "claude"}); err != nil {
			t.Fatal(err)
		}
	})
	if said != "" {
		t.Errorf("the global -q printed a notice: %q", said)
	}
}

// -q is identifiers and errors only, so brig's own notices go with it.
func TestQuietSuppressesBrigsOwnNotices(t *testing.T) {
	atLevel(t, wrap.Quiet)
	said := captureStderr(t, func() { warnf("something worth saying") })
	if said != "" {
		t.Errorf("-q printed a notice: %q", said)
	}

	atLevel(t, wrap.Normal)
	said = captureStderr(t, func() { warnf("something worth saying") })
	if !strings.Contains(said, "something worth saying") {
		t.Errorf("the default output dropped a notice: %q", said)
	}
}

// The envelope is behind --verbose. It is not gone: `brig info` is the command
// whose whole output it is, and it answers without booting anything.
//
// Both of showsEnvelope's rules are here, because a change to either is silent:
// a verb that starts printing the block interrupts a script, and a level that
// starts printing it undoes this issue.
func TestTheEnvelopeIsBehindVerbose(t *testing.T) {
	for _, tc := range []struct {
		verb  string
		level wrap.Verbosity
		want  bool
	}{
		{"run", wrap.Quiet, false},
		{"run", wrap.Normal, false},
		{"run", wrap.Verbose, true},
		{"create", wrap.Verbose, true},
		// sh continues a session whose boundary the reader has already met, and
		// a scripted one on a cold sandbox must not be interrupted by a block.
		{"sh", wrap.Verbose, false},
		{"info", wrap.Verbose, false},
		{"stop", wrap.Verbose, false},
	} {
		if got := showsEnvelope(tc.verb, tc.level); got != tc.want {
			t.Errorf("showsEnvelope(%q, %d) = %v, want %v", tc.verb, tc.level, got, tc.want)
		}
	}
}
