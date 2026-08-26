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

// --network takes a value and --offline is its shorthand for one posture. The
// pair exists because "offline" is the word the request is usually phrased in,
// and a flag that reads like the sentence is worth two lines of plumbing.
func TestParseNetworkAndOffline(t *testing.T) {
	o, profileName, _, err := parse([]string{"--network", "offline", "claude"})
	if err != nil {
		t.Fatalf("--network offline: %v", err)
	}
	if profileName != "claude" {
		t.Errorf("profile = %q, want claude", profileName)
	}
	if o.load.Network != "offline" {
		t.Errorf("network = %q, want offline", o.load.Network)
	}

	if o, _, _, err = parse([]string{"--offline", "claude"}); err != nil {
		t.Fatalf("--offline: %v", err)
	}
	if o.load.Network != "offline" {
		t.Errorf("--offline gave network %q, want offline", o.load.Network)
	}

	// Saying both, and disagreeing, is a mistake worth naming rather than a
	// silent precedence puzzle.
	if _, _, _, err = parse([]string{"--offline", "--network", "shared", "claude"}); err == nil {
		t.Error("--offline with --network shared was accepted")
	}
}
