package main

import (
	"errors"
	"strings"
	"testing"
)

// `brig run <ref> [project] [agent args...]`. The second bare word is the
// project directory, and brig's parsing still ends there: everything after it
// is the agent's, exactly as it was when the word itself was.
func TestRunTakesTheProjectAsAPositional(t *testing.T) {
	for _, c := range []struct {
		args    []string
		project string
		tail    string
	}{
		{[]string{"claude", "/tmp/p"}, "/tmp/p", ""},
		{[]string{"claude", "/tmp/p", "-p", "hi"}, "/tmp/p", "-p hi"},
		{[]string{"claude", "-q", "/tmp/p"}, "/tmp/p", ""},
		// brig reads past the project, which is its own operand, and stops
		// at the next bare word: that is where the agent's argv starts.
		{[]string{"claude", "/tmp/p", "status"}, "/tmp/p", "status"},
		{[]string{"claude", "/tmp/p", "-q", "status"}, "/tmp/p", "status"},
		// An explicit -- declares the rest the agent's, project included: the
		// line has already said which reading it wants.
		{[]string{"claude", "--", "/tmp/p"}, "", "/tmp/p"},
		// A project named before the marker stands: -- speaks for what comes
		// after it, not for what came before.
		{[]string{"claude", "/tmp/p", "--", "--version"}, "/tmp/p", "--version"},
		{[]string{"claude"}, "", ""},
		{[]string{"claude", "-p", "hi"}, "", "-p hi"},
	} {
		o, profileName, tail, err := parse("run", c.args)
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if profileName != "claude" {
			t.Errorf("parse(%q) resolved the profile %q, want claude", c.args, profileName)
		}
		if o.load.Project != c.project || strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = project %q, tail %q; want %q and %q",
				c.args, o.load.Project, strings.Join(tail, " "), c.project, c.tail)
		}
	}
}

// Only run takes a positional. On sh -- and on the two spellings it replaces --
// a second bare word is already the guest command, and on the verbs that take a
// ref and nothing more it is still a mistake for rejectTail to name. Both get
// the word back at the head of their tail, and brig stops reading there: a
// flag after the guest command is the guest's.
func TestOnlyRunKeepsTheSecondBareWord(t *testing.T) {
	_, _, word, tail, err := split("run", []string{"claude", "myproject", "-p", "hi"})
	if err != nil {
		t.Fatalf("split(run): %v", err)
	}
	if word != "myproject" || strings.Join(tail, " ") != "-p hi" {
		t.Errorf("run: project %q, tail %q; want myproject and -p hi", word, tail)
	}
	for _, verb := range []string{"sh", "shell", "exec", "create", "stop", "rm", "info", "env"} {
		_, _, word, tail, err := split(verb, []string{"claude", "echo", "hi"})
		if err != nil {
			t.Errorf("split(%s): %v", verb, err)
			continue
		}
		if word != "" {
			t.Errorf("%s took %q as a project", verb, word)
		}
		if strings.Join(tail, " ") != "echo hi" {
			t.Errorf("%s: tail %q, want the word back at the head (echo hi)", verb, tail)
		}
	}
	// Nothing to decide when there is no second bare word.
	if _, _, word, tail, err := split("run", []string{"claude", "-p", "hi"}); err != nil ||
		word != "" || strings.Join(tail, " ") != "-p hi" {
		t.Errorf("no positional: project %q, tail %q, err %v", word, tail, err)
	}
}

// Brig's own flags keep working after the project positional.
//
// The positional is brig's operand, like the ref before it, so it does not end
// brig's reading of the line -- split's rule is that the first token brig does
// not own ends it, and this is a token brig owns. Before this, every brig flag
// after the path went to the agent: `brig run claude ~/proj --mem 4096 -d
// --offline` booted in the foreground with default memory and the network on,
// warned about but not acted on.
func TestBrigFlagsAfterTheProjectAreStillBrigs(t *testing.T) {
	o, _, tail, err := parse("run", []string{"claude", "/tmp/p", "--mem", "4096", "-d", "--offline"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.load.Project != "/tmp/p" {
		t.Errorf("project = %q, want /tmp/p", o.load.Project)
	}
	if o.load.Mem != 4096 || !o.detach || !o.offline {
		t.Errorf("mem=%d detach=%v offline=%v, want 4096 true true", o.load.Mem, o.detach, o.offline)
	}
	if len(tail) != 0 {
		t.Errorf("tail = %q, want empty", tail)
	}

	// A flag brig does not own still ends the reading, and a third bare word
	// still does: those are the agent's, and the boundary rule is unchanged.
	for _, c := range []struct {
		args []string
		tail string
	}{
		{[]string{"claude", "/tmp/p", "--resume"}, "--resume"},
		{[]string{"claude", "/tmp/p", "npm", "test"}, "npm test"},
		{[]string{"claude", "/tmp/p", "-d", "npm", "test"}, "npm test"},
	} {
		o, _, tail, err := parse("run", c.args)
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if o.load.Project != "/tmp/p" || strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = project %q, tail %q; want /tmp/p and %q",
				c.args, o.load.Project, strings.Join(tail, " "), c.tail)
		}
	}
}

// The contradiction is refused in the spelling people type, not only in the
// one where the flag comes first. Reaching it is what the positional no longer
// ending brig's reading buys.
func TestNoProjectAfterThePositionalIsRefused(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{
		{"run", "--no-project", "claude", dir},
		{"run", "claude", dir, "--no-project"},
	} {
		err := run(args)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("brig %s: error is %T (%v), want a usage error",
				strings.Join(args, " "), err, err)
		}
	}
}

// The one-release warning. A bare word after the ref used to end brig's
// parsing and reach the AGENT, so giving it a new meaning is a breaking change
// for anyone who passed one through. Both readings are named, so a user can
// pick one before the notice goes.
func TestASecondBareWordWarnsAndNamesBothReadings(t *testing.T) {
	scratchHost(t)
	notice := captureStderr(t, func() { _, _ = captureStdout(t, func() error { return run([]string{"run", "claude", "."}) }) })
	for _, want := range []string{"project", "--", "`.`"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not name %s:\n%s", want, notice)
		}
	}

	// After an explicit -- there is nothing to point out: the line said what
	// the word is, and the word is not a project.
	scratchHost(t)
	notice = captureStderr(t, func() {
		_, _ = captureStdout(t, func() error { return run([]string{"run", "claude", "--", "."}) })
	})
	if strings.Contains(notice, "project directory") {
		t.Errorf("a tail after -- was warned about:\n%s", notice)
	}

	// And a run that names no positional at all hears nothing.
	scratchHost(t)
	notice = captureStderr(t, func() {
		_, _ = captureStdout(t, func() error { return run([]string{"run", "claude", "-p", "hi"}) })
	})
	if strings.Contains(notice, "project directory") {
		t.Errorf("a run with no positional was warned about:\n%s", notice)
	}
}

// --home is what sets the guest home now. Both older spellings keep working --
// a line that works today has to keep working -- and each says the one word
// that replaces it.
func TestHomeReplacesWorkspace(t *testing.T) {
	for _, c := range []struct {
		args   []string
		notice string
	}{
		{[]string{"claude", "--home", "/h"}, ""},
		{[]string{"claude", "--home=/h"}, ""},
		{[]string{"claude", "-w", "/h"}, "`-w` is now `--home`"},
		{[]string{"claude", "--workspace", "/h"}, "`--workspace` is now `--home`"},
		{[]string{"claude", "--workspace=/h"}, "`--workspace` is now `--home`"},
	} {
		var o options
		var err error
		notice := captureStderr(t, func() { o, _, _, err = parse("run", c.args) })
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if o.load.Workspace != "/h" {
			t.Errorf("parse(%q) set the home to %q, want /h", c.args, o.load.Workspace)
		}
		if c.notice == "" {
			if strings.Contains(notice, "is now") {
				t.Errorf("parse(%q) printed a notice:\n%s", c.args, notice)
			}
			continue
		}
		if !strings.Contains(notice, c.notice) {
			t.Errorf("parse(%q) does not say %q:\n%s", c.args, c.notice, notice)
		}
	}
}

// --no-project is a run-line flag like the positional it answers, so it parses
// between the verb and the ref and reaches wrap.Options.
func TestNoProjectParses(t *testing.T) {
	o, _, tail, err := parse("run", []string{"--no-project", "claude"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.load.NoProject {
		t.Error("--no-project did not reach the options")
	}
	if len(tail) != 0 {
		t.Errorf("--no-project left a tail: %q", tail)
	}
}

// Both at once ask for two contradictory things, and either winner would be
// silent about the other: the directory mounted with --no-project ignored, or
// dropped with the word the line names ignored. Refuse and name both.
func TestNoProjectWithAPositionalIsRefused(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	dir := t.TempDir()
	err := run([]string{"run", "--no-project", "claude", dir})
	if err == nil {
		t.Fatal("--no-project alongside a project was accepted")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("error is %T, want a usage error: %v", err, err)
	}
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "--no-project") {
		t.Errorf("the error does not name both: %v", err)
	}
}

// Elsewhere the flag would do nothing, which is worse than being refused: a
// user would think the session had been detached and find it still mounted.
func TestNoProjectOnAnotherVerbIsRefused(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, verb := range []string{"sh", "info", "stop"} {
		err := run([]string{verb, "--no-project", "claude"})
		if err == nil {
			t.Errorf("brig %s --no-project was accepted", verb)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("brig %s --no-project: error is %T, want a usage error", verb, err)
		}
	}
}
