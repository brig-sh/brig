package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// The old spellings keep working for one release. Anyone who scripted against
// brig template should get a working command and a note, not a failure.
func TestDeprecatedVerbsStillWork(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{{"agents"}, {"template", "ls"}} {
		if err := run(args); err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
		}
	}
}

// There is no brig template edit: the deprecated group carries only the verbs
// it already had. A command that never existed under the old name does not
// need a deprecated spelling, and adding one would extend the vocabulary this
// change is retiring.
func TestNoDeprecatedEditVerb(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := run([]string{"template", "edit", "mine"}); err == nil {
		t.Error("brig template edit exists")
	}
}

// hostCredential: keeps working for one release and says it is going, naming
// what replaces it. Following the agents->profiles and template->profile
// pattern above.
//
// The warning is scoped to file-backed profiles, so this fixture is a file.
// The two assertions are the deprecation contract in full: it still parses and
// still carries the block, AND brig says the block is going.
func TestHostCredentialWarnsAndStillWorks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n" +
		"hostCredential:\n  keychainService: My Tool-credentials\n" +
		"  tokenField: accessToken\n  targetVar: MYTOOL_TOKEN\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	var err error
	warning := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"profiles"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "hostCredential:") ||
		!strings.Contains(warning, "deprecated") {
		t.Errorf("nothing said hostCredential: is going:\n%s", warning)
	}
	if !strings.Contains(warning, "brig secret import mine") {
		t.Errorf("the warning does not name what replaces it:\n%s", warning)
	}
	p, ok := profile.Lookup("mine")
	if !ok {
		t.Fatal("the profile stopped loading")
	}
	if p.HostCredential == nil || p.HostCredential.TargetVar != "MYTOOL_TOKEN" {
		t.Errorf("hostCredential: stopped working: %+v", p.HostCredential)
	}
}

// A built-in must never warn about itself. After the switchover no shipped
// spec carries the key, so an unscoped check would fire on every brig command
// for a file the user does not own and cannot edit -- which is how a reader
// learns to ignore the warnings that are theirs to act on.
func TestBuiltInProfilesDoNotWarnAboutHostCredential(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	var err error
	warning := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"profiles"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(warning, "hostCredential:") {
		t.Errorf("brig warned about its own built-in spec:\n%s", warning)
	}
	for _, p := range profile.All() {
		if p.HostCredential != nil {
			t.Errorf("built-in %s still carries hostCredential:, so brig reads the "+
				"%s keychain item on every run", p.Name, p.HostCredential.KeychainService)
		}
	}
}

// -t and -m keep working for one release and say what replaces them. They are
// two of the four short flags #47 removes in v0.3, and the notice is the same
// one the retiring verbs print: a working command and a line about it, not a
// failure.
func TestDeprecatedShortFlagsStillWork(t *testing.T) {
	for _, c := range []struct {
		args        []string
		replacement string
		check       func(options) bool
	}{
		{[]string{"claude", "-t", "img:1"}, "--image", func(o options) bool { return o.load.Image == "img:1" }},
		{[]string{"claude", "-m", "2048"}, "--mem", func(o options) bool { return o.load.Mem == 2048 }},
		// The inline form is the same flag, so it hears the same notice.
		{[]string{"claude", "-t=img:1"}, "--image", func(o options) bool { return o.load.Image == "img:1" }},
	} {
		var (
			o   options
			err error
		)
		warning := captureStderr(t, func() { o, _, _, err = parse(c.args) })
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if !c.check(o) {
			t.Errorf("parse(%q) stopped working: %+v", c.args, o.load)
		}
		// The notice names the spelling, not the token: the inline form's
		// value is no part of what is being deprecated.
		spelling, _, _ := strings.Cut(c.args[1], "=")
		if !strings.Contains(warning, spelling) || !strings.Contains(warning, c.replacement) {
			t.Errorf("parse(%q) did not point %s at %s:\n%s",
				c.args, spelling, c.replacement, warning)
		}
	}
	// The long spellings are current, so they say nothing.
	warning := captureStderr(t, func() {
		if _, _, _, err := parse([]string{"claude", "--image", "img:1", "--mem", "8"}); err != nil {
			t.Errorf("parse: %v", err)
		}
	})
	if warning != "" {
		t.Errorf("the current spellings printed a deprecation notice:\n%s", warning)
	}
	// A value that happens to be spelled like a deprecated flag is a value.
	// Warning on it would send the reader looking for a flag they did not type.
	warning = captureStderr(t, func() {
		if _, _, _, err := parse([]string{"claude", "--name", "-t"}); err != nil {
			t.Errorf("parse: %v", err)
		}
	})
	if strings.Contains(warning, "--image") {
		t.Errorf("a --name value read as a deprecated flag:\n%s", warning)
	}
}
