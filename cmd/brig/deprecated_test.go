package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v0.5.0 removed the spellings that had printed a notice since v0.2, and each
// one is now a usage error that names its replacement, on every shape of the
// line. Exit 2 rather than the 1 an unknown command gets, because the word was
// brig's own, and nothing on stderr: the refusal is the whole of the answer.
//
// The replacement is asserted as a whole command line rather than as the noun,
// because that is the thing a reader can paste.
func TestRemovedSpellingsAreUsageErrorsNamingTheReplacement(t *testing.T) {
	for _, c := range []struct {
		args        []string
		replacement string
	}{
		// Removed in v0.3.0.
		{[]string{"shell"}, "brig sh <ref> [command...]"},
		{[]string{"shell", "claude"}, "brig sh <ref> [command...]"},
		{[]string{"shell", "claude", "echo", "hi"}, "brig sh <ref> [command...]"},
		{[]string{"shell", "claude", "-c", "script.sh"}, "brig sh <ref> [command...]"},
		{[]string{"--json", "shell", "claude"}, "brig sh <ref> [command...]"},
		// The plural nouns.
		{[]string{"profiles"}, "brig agent ls"},
		{[]string{"agents"}, "brig agent ls"},
		{[]string{"policies"}, "brig policy ls"},
		// The renamed group, with and without a verb.
		{[]string{"profile"}, "brig agent <verb>"},
		{[]string{"profile", "ls"}, "brig agent <verb>"},
		{[]string{"profile", "--help"}, "brig agent <verb>"},
		{[]string{"template", "ls"}, "brig agent <verb>"},
		{[]string{"template", "edit", "mine"}, "brig agent <verb>"},
		// The top-level spellings of a noun command.
		{[]string{"import", "-"}, "brig agent import"},
		{[]string{"export", "codex"}, "brig agent export"},
		// The lifecycle verbs. A runtime is never reached, so `brig reset` on a
		// machine with sandboxes on it removes nothing.
		{[]string{"create", "claude"}, "brig run -d <ref>"},
		{[]string{"env", "claude"}, "brig info <ref>"},
		{[]string{"--json", "env", "claude"}, "brig info <ref>"},
		{[]string{"reset"}, "brig rm --all"},
		{[]string{"reset", "-y"}, "brig rm --all"},
		// The undocumented second spellings inside a group.
		{[]string{"agent", "list"}, "brig agent ls"},
		{[]string{"agent", "save", "codex"}, "brig agent export"},
		{[]string{"agent", "load", "-"}, "brig agent import"},
		{[]string{"policy", "list"}, "brig policy ls"},
		{[]string{"secret", "list"}, "brig secret ls"},
		{[]string{"secret", "rm", "gh-token", "-y"}, "brig secret delete"},
	} {
		line := "brig " + strings.Join(c.args, " ")
		scratchHost(t)
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		f := newFake(t)
		f.seed("gh-token", "value")
		var err error
		stderr := captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(c.args) })
		})
		if code := exitCode(err); code != exitUsage {
			t.Errorf("%s exited %d, want %d: %v", line, code, exitUsage, err)
		}
		if err == nil || !strings.Contains(err.Error(), "was removed; use `"+c.replacement+"`") {
			t.Errorf("%s does not name %q as its replacement: %v", line, c.replacement, err)
		}
		if stderr != "" {
			t.Errorf("%s printed on stderr:\n%s", line, stderr)
		}
		if _, ok := f.items["gh-token"]; !ok {
			t.Errorf("%s removed a secret", line)
		}
	}
}

// The current spellings say nothing. A notice on a command that is not going
// anywhere is how a reader learns to ignore the ones that are.
func TestCurrentSpellingsPrintNoNotice(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "ls"}, {"agent", "show", "codex"}, {"agent", "export", "codex"},
		{"policy", "ls"}, {"telemetry", "--help"},
	} {
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		var err error
		notice := captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(args) })
		})
		if err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
		}
		if strings.Contains(notice, "is now") {
			t.Errorf("brig %s printed a deprecation notice:\n%s", strings.Join(args, " "), notice)
		}
	}
}

// `brig secret delete` is what `brig secret rm` became, and it still removes
// the secret and says nothing about it.
func TestSecretDeleteIsTheSpelling(t *testing.T) {
	f := newFake(t)
	f.seed("gh-token", "value")
	var err error
	notice := captureStderr(t, func() {
		err = secretCmd(&bytes.Buffer{}, []string{"delete", "gh-token", "-y"})
	})
	if err != nil {
		t.Errorf("brig secret delete: %v", err)
	}
	if _, ok := f.items["gh-token"]; ok {
		t.Error("brig secret delete did not remove the secret")
	}
	if strings.Contains(notice, "is now") {
		t.Errorf("the documented spelling printed a deprecation notice:\n%s", notice)
	}
}

// The run-line flags v0.5.0 removed went back to the agents, which is why #47
// retired them: after the ref each is the agent's word, forwarded untouched.
// Before the ref there is no agent to own one, so it is a usage error naming
// its replacement, the inline form included. -q names the position it moved
// to, because its spelling is still current left of the verb.
func TestRemovedFlagsAreRefusedBeforeTheRefAndTheAgentsAfterIt(t *testing.T) {
	for _, c := range []struct {
		args       []string
		was, using string
	}{
		{[]string{"-t", "img:1", "claude"}, "-t", "--image"},
		{[]string{"-t=img:1", "claude"}, "-t", "--image"},
		{[]string{"-m", "2048", "claude"}, "-m", "--mem"},
		{[]string{"-n", "refactor", "claude"}, "-n", "<agent>@<label>"},
		{[]string{"--name", "refactor", "claude"}, "--name", "<agent>@<label>"},
		{[]string{"--name=refactor", "claude"}, "--name", "<agent>@<label>"},
		{[]string{"-w", "/tmp/h", "claude"}, "-w", "--home"},
		{[]string{"--workspace", "/tmp/h", "claude"}, "--workspace", "--home"},
		{[]string{"-q", "claude"}, "brig <verb> <ref> -q", "brig -q <verb> <ref>"},
		{[]string{"--quiet", "claude"}, "brig <verb> <ref> --quiet", "brig --quiet <verb> <ref>"},
	} {
		var err error
		warning := captureStderr(t, func() { _, _, _, err = parse("run", c.args) })
		if code := exitCode(err); code != exitUsage {
			t.Errorf("parse(%q) exited %d, want %d: %v", c.args, code, exitUsage, err)
			continue
		}
		want := "`" + c.was + "` was removed; use `" + c.using + "`"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("parse(%q) = %v, want it to say %s", c.args, err, want)
		}
		if warning != "" {
			t.Errorf("parse(%q) printed on stderr:\n%s", c.args, warning)
		}
	}
	// After the ref, brig reads none of them: they are the agent's, and so
	// is everything after them.
	for _, args := range [][]string{
		{"claude", "-t", "img:1"},
		{"claude", "-m", "2048"},
		{"claude", "-n", "refactor"},
		{"claude", "--name", "refactor"},
		{"claude", "--name=refactor"},
		{"claude", "-w", "/tmp/h"},
		{"claude", "--workspace", "/tmp/h"},
	} {
		var (
			o    options
			tail []string
			err  error
		)
		warning := captureStderr(t, func() { o, _, tail, err = parse("run", args) })
		if err != nil {
			t.Errorf("parse(%q): %v", args, err)
			continue
		}
		if strings.Join(tail, " ") != strings.Join(args[1:], " ") {
			t.Errorf("parse(%q) tail = %q, want the agent to get it all", args, tail)
		}
		if o.load.Image != "" || o.load.Mem != 0 || o.load.Name != "" || o.load.Workspace != "" {
			t.Errorf("parse(%q) read the agent's flag: %+v", args, o.load)
		}
		if warning != "" {
			t.Errorf("parse(%q) printed on stderr:\n%s", args, warning)
		}
	}
	// The long spellings are current, so they say nothing.
	warning := captureStderr(t, func() {
		o, _, _, err := parse("run", []string{"claude", "--image", "img:1", "--mem", "8", "--memory", "9", "--home", "/h"})
		if err != nil {
			t.Errorf("parse: %v", err)
		}
		if o.load.Image != "img:1" || o.load.Mem != 9 || o.load.Workspace != "/h" {
			t.Errorf("the current spellings did not land: %+v", o.load)
		}
	})
	if warning != "" {
		t.Errorf("the current spellings printed a notice:\n%s", warning)
	}
	// A value spelled like a removed flag is a value.
	if o, _, _, err := parse("run", []string{"--image", "-t", "claude"}); err != nil || o.load.Image != "-t" {
		t.Errorf("--image -t: image %q, err %v", o.load.Image, err)
	}
}

// brig exec is the one retired spelling left. It still works, names brig sh,
// and names no release: it stays until brig sh can pipe a command's output
// (#335), and a date it cannot keep is the promise v0.3 broke.
func TestExecStillWorksAndNamesNoRelease(t *testing.T) {
	scratchHost(t)
	var err error
	notice := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"exec", "claude", "--", "true"}) })
	})
	if !took(err) {
		t.Errorf("brig exec stopped working: %v", err)
	}
	if !strings.Contains(notice, "brig: `brig exec` is now `brig sh`") {
		t.Errorf("brig exec does not name brig sh:\n%s", notice)
	}
	if !strings.Contains(notice, "stays until `brig sh` can pipe") {
		t.Errorf("brig exec does not say what it waits for:\n%s", notice)
	}
	if strings.Contains(notice, "removed in") {
		t.Errorf("brig exec names a release:\n%s", notice)
	}
}

// The current lifecycle spellings say nothing. This is the other half of the
// contract above: a notice on a command that is not going anywhere is how a
// reader learns to ignore the ones that are.
//
// The verbless ref is here too. It is accepted and taught nowhere, which is not
// the same as retired -- there is no older spelling of it to stop typing, so
// there is nothing for brig to say.
func TestCurrentLifecycleSpellingsPrintNoNotice(t *testing.T) {
	for _, args := range [][]string{
		{"run", "claude", "-d"}, {"sh", "claude"}, {"sh", "claude", "echo", "hi"},
		{"stop", "claude"}, {"rm", "claude"}, {"rm", "--all"}, {"info", "claude"},
		{"claude"}, {"claude@refactor"},
	} {
		scratchHost(t)
		var err error
		notice := captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(args) })
		})
		if !took(err) {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
		if strings.Contains(notice, "is now") {
			t.Errorf("brig %s printed a deprecation notice:\n%s",
				strings.Join(args, " "), notice)
		}
	}
}

// The label is the session, and it lands where everything downstream reads
// the session name, with nothing said about it.
func TestTheLabelNamesTheSession(t *testing.T) {
	notice := captureStderr(t, func() {
		o, _, _, err := parse("run", []string{"claude@refactor"})
		if err != nil {
			t.Errorf("parse: %v", err)
		}
		if o.load.Name != "refactor" || !o.nameGiven {
			t.Errorf("the label did not land as the session name: %+v", o.load)
		}
	})
	if notice != "" {
		t.Errorf("the label form printed a notice:\n%s", notice)
	}
}

// Every removed spelling has a row in docs/migration.md, which is where the
// refusal sends a reader who wants more than the one line, and where
// script/check-retired-spellings.sh points.
func TestMigrationListsEveryRemovedSpelling(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "..", "docs", "migration.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(blob)
	for verb := range removedVerbs {
		if !strings.Contains(doc, "`brig "+verb) {
			t.Errorf("docs/migration.md does not list `brig %s`", verb)
		}
	}
	for spelling := range removedFlags {
		if !strings.Contains(doc, "`"+spelling) && !strings.Contains(doc, spelling+"`") {
			t.Errorf("docs/migration.md does not list %s", spelling)
		}
	}
}

// BRIG_TEMPLATE_DIR went in v0.5.0. Set without BRIG_PROFILE_DIR it was the
// setting that said where profiles live, so it is refused before any profile
// is read, naming the replacement, rather than ignored in favour of the
// built-ins. Beside BRIG_PROFILE_DIR it never decided anything and is let
// through.
func TestTemplateDirIsRefusedWhereItWouldHaveDecided(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", "")
	t.Setenv("BRIG_TEMPLATE_DIR", t.TempDir())
	var err error
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"agent", "ls"}) })
	})
	if err == nil || !strings.Contains(err.Error(), "BRIG_PROFILE_DIR") {
		t.Errorf("brig agent ls with BRIG_TEMPLATE_DIR = %v, want a refusal naming BRIG_PROFILE_DIR", err)
	}
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"agent", "ls"}) })
	})
	if err != nil {
		t.Errorf("brig agent ls with both set: %v", err)
	}
}
