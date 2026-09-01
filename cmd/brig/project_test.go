package main

import (
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
		// brig stops reading at the project, so a word after it is the
		// agent's -- the boundary has not moved, only what the word means.
		{[]string{"claude", "/tmp/p", "status"}, "/tmp/p", "status"},
		// An explicit -- declares the rest the agent's, project included: the
		// line has already said which reading it wants.
		{[]string{"claude", "--", "/tmp/p"}, "", "/tmp/p"},
		{[]string{"claude"}, "", ""},
		{[]string{"claude", "-p", "hi"}, "", "-p hi"},
	} {
		o, profileName, tail, err := parse(c.args)
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
// ref and nothing more it is still a mistake for rejectTail to name.
func TestOnlyRunKeepsTheSecondBareWord(t *testing.T) {
	project, tail := projectFor("run", "myproject", []string{"-p", "hi"})
	if project != "myproject" || strings.Join(tail, " ") != "-p hi" {
		t.Errorf("run: project %q, tail %q; want myproject and -p hi", project, tail)
	}
	for _, verb := range []string{"sh", "shell", "exec", "create", "stop", "rm", "info", "env"} {
		project, tail := projectFor(verb, "echo", []string{"hi"})
		if project != "" {
			t.Errorf("%s took %q as a project", verb, project)
		}
		if strings.Join(tail, " ") != "echo hi" {
			t.Errorf("%s: tail %q, want the word back at the head (echo hi)", verb, tail)
		}
	}
	// Nothing to decide when there is no second bare word.
	if project, tail := projectFor("run", "", []string{"-p", "hi"}); project != "" ||
		strings.Join(tail, " ") != "-p hi" {
		t.Errorf("no positional: project %q, tail %q", project, tail)
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
		notice := captureStderr(t, func() { o, _, _, err = parse(c.args) })
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
