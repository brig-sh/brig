package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
)

// `brig env` is the only window a user has into what a sandbox is about to
// receive, and it used to print the profile's deny list unconditionally: with
// BRIG_ALLOW_DENIED=1 the same report said "forwarding to guest:
// ANTHROPIC_API_KEY" and, four lines later, "never forwarded for x:
// ANTHROPIC_API_KEY". One of those was a lie, and the report gave no way to
// tell which.
func TestStatusDoesNotContradictItselfWhenTheDenylistIsOverridden(t *testing.T) {
	c := bindingConfig(t, "deny: [ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN]\n")
	c.AllowDenied = true
	c.Runtime = fakeRuntime{}
	out := &bytes.Buffer{}
	c.Out = out

	var set creds.Set
	set.Add("ANTHROPIC_API_KEY", "sk-x", "ANTHROPIC_API_KEY")
	c.Status(set)

	got := out.String()
	if strings.Contains(got, "never forwarded") && strings.Contains(got, "ANTHROPIC_API_KEY") {
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "never forwarded") &&
				strings.Contains(line, "ANTHROPIC_API_KEY") {
				t.Errorf("the report says it is forwarding ANTHROPIC_API_KEY and also that it "+
					"never forwards it:\n%s", got)
			}
		}
	}
	if !strings.Contains(got, "OVERRIDDEN") {
		t.Errorf("the report did not say the denylist was overridden:\n%s", got)
	}
	// The entry that was not forwarded is still worth reporting, and still
	// unguarded: the switch is off for the whole list, not for one name.
	if !strings.Contains(got, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the report lost the entry nothing tripped:\n%s", got)
	}
	if strings.Contains(got, "sk-x") {
		t.Errorf("the report printed a value:\n%s", got)
	}
}

// The override message quotes the value the user set, not a hardcoded =1. With
// the strict reading BRIG_ALLOW_DENIED=true is what turns the guard off, and a
// report answering "why is my sandbox on metered billing" with a =1 the user
// never wrote sends them looking for a variable that is not there.
func TestStatusOverrideMessageReportsTheValueTheUserSet(t *testing.T) {
	c := bindingConfig(t, "deny: [ANTHROPIC_API_KEY]\n")
	c.env = NewEnv(c.Profile.Name, oneVar("BRIG_ALLOW_DENIED", "true"))
	c.AllowDenied = true
	c.Runtime = fakeRuntime{}
	out := &bytes.Buffer{}
	c.Out = out

	var set creds.Set
	set.Add("ANTHROPIC_API_KEY", "sk-x", "ANTHROPIC_API_KEY")
	c.Status(set)

	got := out.String()
	if !strings.Contains(got, "BRIG_ALLOW_DENIED=true") {
		t.Errorf("the override message does not quote what the user set:\n%s", got)
	}
	if strings.Contains(got, "BRIG_ALLOW_DENIED=1") {
		t.Errorf("the override message still quotes a hardcoded =1:\n%s", got)
	}
}

// With the guard on, the report reads as it always did.
func TestStatusStillNamesTheDenylistWhenItIsOn(t *testing.T) {
	c := bindingConfig(t, "deny: [ANTHROPIC_API_KEY]\n")
	c.Runtime = fakeRuntime{}
	out := &bytes.Buffer{}
	c.Out = out
	c.Status(creds.Set{})
	if got := out.String(); !strings.Contains(got, "never forwarded for x: ANTHROPIC_API_KEY") {
		t.Errorf("the denylist was not reported:\n%s", got)
	}
}

// A file in the profile directory may override a built-in by name -- that is
// how you pin your own image for a profile brig already knows about -- and a
// file that omits deny: silently takes the billing guard with it. Nothing said
// so: the report printed the profile's own deny list, which in that case is
// empty, so it printed nothing at all.
func TestStatusSurfacesAFileThatOverridesABuiltInAndDropsItsDenylist(t *testing.T) {
	dir := t.TempDir()
	// The built-in claude-code, minus its deny list: the file a user writes
	// when they only meant to pin an image.
	spec := "name: claude-code\nimage: ghcr.io/me/mine:latest\nguestHome: /home/claude\n" +
		"binary: claude\nmem: 1\ncpus: 1\n"
	path := filepath.Join(dir, "claude-code.yaml")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(dir); err != nil {
		t.Fatal(err)
	}
	// Put the registry back for every other test in the package.
	t.Cleanup(func() {
		if err := profile.Load(); err != nil {
			t.Fatal(err)
		}
	})

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("the override was not loaded")
	}
	c := testConfig(t, t.TempDir(), t.TempDir(), p)
	c.Runtime = fakeRuntime{}
	out := &bytes.Buffer{}
	c.Out = out
	c.Status(creds.Set{})

	got := out.String()
	if !strings.Contains(got, path) {
		t.Errorf("the report does not name the file that decides what claude-code runs:\n%s", got)
	}
	for _, dropped := range profile.BuiltInDeny("claude-code") {
		if !strings.Contains(got, dropped) {
			t.Errorf("the report does not say %s is no longer denied:\n%s", dropped, got)
		}
	}
}

// The built-in itself must not be reported as an override of anything, or the
// warning means nothing when it matters.
func TestStatusSaysNothingAboutABuiltInProfile(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code is not registered")
	}
	c := testConfig(t, t.TempDir(), t.TempDir(), p)
	c.Runtime = fakeRuntime{}
	out := &bytes.Buffer{}
	c.Out = out
	c.Status(creds.Set{})
	if got := out.String(); strings.Contains(got, "overriding the one brig ships") {
		t.Errorf("a built-in profile was reported as an override:\n%s", got)
	}
}
