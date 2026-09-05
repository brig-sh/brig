package main

import (
	"strings"
	"testing"
)

// #123: a verb that forwards no tail must not warn that a token is the agent's,
// because it has no agent to hand one to. The old code warned inside split for
// every verb and then rejectTail refused the same line, so `brig stop claude
// extra -q` printed two messages that disagreed: one calling -q the agent's,
// one saying the verb has no agent. Now split says nothing and rejectTail gives
// the single correct message.
func TestVerbsThatForwardNoTailWarnOnlyViaRejectTail(t *testing.T) {
	for _, verb := range []string{"stop", "rm", "info", "env", "create"} {
		var (
			tail []string
			err  error
		)
		warning := captureStderr(t, func() {
			_, _, tail, err = parse(verb, []string{"claude", "extra", "-q"})
		})
		if err != nil {
			t.Fatalf("parse(%s): %v", verb, err)
		}
		// Not a word about -q, and nothing calling the tail the agent's: there
		// is no agent on these verbs to call it that.
		if strings.Contains(warning, "-q") || strings.Contains(warning, "one of brig's own flags") {
			t.Errorf("%s warned about a tail it does not forward:\n%s", verb, warning)
		}
		// The one correct message is rejectTail's, and it names the stray word
		// rather than the flag beside it.
		err = rejectTail(verb, tail)
		if err == nil {
			t.Fatalf("%s: rejectTail accepted %q", verb, tail)
		}
		if !strings.Contains(err.Error(), "extra") {
			t.Errorf("%s: rejectTail did not name the stray argument: %v", verb, err)
		}
		if strings.Contains(err.Error(), "-q") {
			t.Errorf("%s: rejectTail named -q, which is not the stray word: %v", verb, err)
		}
	}
}

// The warning still exists for the verbs that do forward a tail, which is the
// case it is for: `brig run claude foo bar -q` runs the agent with -q and
// leaves the envelope printed, so brig says which token it did not read. -q is
// read on run's line, so that is the position the advice names.
func TestRunWarnsAboutQuietInTailNamingTheRunLine(t *testing.T) {
	var tail []string
	warning := captureStderr(t, func() {
		_, _, tail, _ = parse("run", []string{"claude", "foo", "bar", "-q"})
	})
	// foo is the project, bar ends brig's reading, and everything from bar on
	// is forwarded untouched -- the warning changes nothing about what runs.
	if strings.Join(tail, " ") != "bar -q" {
		t.Fatalf("tail = %q, want it forwarded untouched (bar -q)", tail)
	}
	if !strings.Contains(warning, "-q") {
		t.Errorf("run said nothing about -q in the tail:\n%s", warning)
	}
	if !strings.Contains(warning, "before the profile") {
		t.Errorf("run did not name the run-line position for -q:\n%s", warning)
	}
}

// -q shapes nothing on sh -- it is documented for run and create -- so the
// run-line position it names on run would be refused on sh. Its home is the
// global position, which does drop sh's warnings, so that is where sh sends the
// reader.
func TestShSendsQuietToTheGlobalPosition(t *testing.T) {
	warning := captureStderr(t, func() {
		_, _, _, _ = parse("sh", []string{"claude", "ls", "-q"})
	})
	if !strings.Contains(warning, "-q") {
		t.Fatalf("sh said nothing about -q:\n%s", warning)
	}
	if !strings.Contains(warning, "before the command") {
		t.Errorf("sh advice for -q is not the global position:\n%s", warning)
	}
	if strings.Contains(warning, "before the profile") {
		t.Errorf("sh sent -q to a run-line position it does not read there:\n%s", warning)
	}
}

// A flag a verb reads in no position gets no position to move to. --no-project
// is run's alone, and it is not global, so on sh there is nowhere it would be
// read -- the honest message says so rather than sending the reader to a
// position that would refuse it too.
func TestShSaysAFlagItCannotReadHasNowhereToGo(t *testing.T) {
	warning := captureStderr(t, func() {
		_, _, _, _ = parse("sh", []string{"claude", "ls", "--no-project"})
	})
	if !strings.Contains(warning, "--no-project") {
		t.Fatalf("sh said nothing about --no-project:\n%s", warning)
	}
	if strings.Contains(warning, "before the profile") || strings.Contains(warning, "before the command") {
		t.Errorf("sh sent --no-project to a position it cannot read it in:\n%s", warning)
	}
}

// Which run-line flags sh reads is the table's to say, not a list beside it:
// every row marked runOnly is read in no position on sh, and every other
// run-line row is, so a flag added without a decision fails here rather than
// drifting.
func TestRunOnlyComesFromTheFlagTable(t *testing.T) {
	seen := 0
	for _, f := range brigFlags {
		if f.position != posRun {
			continue
		}
		seen++
		if got, want := honorsRunLine("sh", f.long), !f.runOnly; got != want {
			t.Errorf("honorsRunLine(sh, %s) = %v, but the row says runOnly=%v", f.long, got, f.runOnly)
		}
		if !honorsRunLine("run", f.long) {
			t.Errorf("honorsRunLine(run, %s) = false; run reads every run-line flag", f.long)
		}
	}
	if seen == 0 {
		t.Fatal("no run-line flags in the table")
	}
}
