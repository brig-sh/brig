package main

import (
	"errors"
	"strings"
	"testing"
)

// An unknown flag standing where brig's own flags go, before the profile is
// named, is refused rather than forwarded. Forwarding it consumed the profile
// as the flag's value and left the line reporting that the profile was missing,
// blaming the one word on it that was right.
func TestParseRefusesUnknownFlagBeforeProfile(t *testing.T) {
	for _, args := range [][]string{
		{"--nope", "claude"},
		{"--dry-run", "claude"},
		{"-x", "claude"},
	} {
		_, _, _, err := parse(args)
		if err == nil {
			t.Errorf("parse(%q) was accepted", args)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("parse(%q): %v, want a usageError", args, err)
		}
		if !strings.Contains(err.Error(), args[0]) {
			t.Errorf("parse(%q): %v, want it to name %q", args, err, args[0])
		}
	}
}

// The same flag once the profile is named is the agent's, and still passes
// through. This is the line the refusal above must not break.
func TestParseKeepsUnknownFlagAfterProfileForTheAgent(t *testing.T) {
	_, name, tail, err := parse([]string{"claude", "--nope"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if name != "claude" || strings.Join(tail, " ") != "--nope" {
		t.Errorf("parse = (%q, %q), want (claude, --nope)", name, tail)
	}
}

// The verbs that take a profile and nothing more refuse a stray argument
// instead of discarding it. The ones that consume a tail keep it.
func TestRejectTail(t *testing.T) {
	for _, verb := range []string{"create", "stop", "rm", "env"} {
		err := rejectTail(verb, []string{"extra"})
		if err == nil {
			t.Errorf("rejectTail(%q, extra) was accepted", verb)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("rejectTail(%q): %v, want a usageError", verb, err)
		}
		if !strings.Contains(err.Error(), "extra") {
			t.Errorf("rejectTail(%q): %v, want it to name the token", verb, err)
		}
	}
	for _, verb := range []string{"run", "shell", "exec"} {
		if err := rejectTail(verb, []string{"-p", "hi"}); err != nil {
			t.Errorf("rejectTail(%q) refused an agent tail: %v", verb, err)
		}
	}
	// No tail is fine for every verb.
	for _, verb := range []string{"create", "stop", "rm", "env", "run", "shell", "exec"} {
		if err := rejectTail(verb, nil); err != nil {
			t.Errorf("rejectTail(%q, nil): %v", verb, err)
		}
	}
}

// ls and reset name no profile and take no flags, so an argument is a token
// they would read and drop. reset is the sharp one: `brig reset --dry-run`
// reads like a preview and removes everything, so it must refuse before it
// reaches the runtime.
func TestLsAndResetRefuseArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]string) error
		args []string
	}{
		{"ls", listSandboxes, []string{"claude"}},
		{"reset", reset, []string{"--dry-run"}},
	} {
		err := tc.fn(tc.args)
		if err == nil {
			t.Errorf("%s(%q) was accepted", tc.name, tc.args)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("%s(%q): %v, want a usageError", tc.name, tc.args, err)
		}
		if !strings.Contains(err.Error(), tc.args[0]) {
			t.Errorf("%s(%q): %v, want it to name %q", tc.name, tc.args, err, tc.args[0])
		}
	}
}
