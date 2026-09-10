package main

import (
	"testing"
)

// One class, one code. A mistake in what was typed -- an unknown verb, a
// missing operand, a stray word, a flag the verb has no place for -- is a
// usage error, and docs/cli.md promises 2 for it. A script branches on that
// number to tell "you typed it wrong" from "brig ran and failed", which is 1.
//
// The tables are written out rather than generated: nothing in the CLI
// publishes its verbs as data, so a verb added later has to be added here.
// #145 is the one that would make that automatic.
func TestUnknownVerbsAndSubcommandsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"nosuchverb"},
		// A group named with no subcommand at all.
		{"agent"}, {"policy"}, {"secret"},
		// A group named with one it does not have.
		{"agent", "bogus"}, {"policy", "bogus"},
		{"secret", "bogus"}, {"telemetry", "bogus"},
		{"completion", "bogus"},
		// A retired group still answers, but not with a verb it never had.
		{"template", "edit", "mine"},
	} {
		assertUsageExit(t, args)
	}
}

// A verb that needs an operand and was given none. Every verb in the four
// groups that takes one is here; the ones that take none -- the listings,
// telemetry status -- are in TestAStrayWordOrUnknownFlagExitsTwo.
func TestAMissingOperandExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "show"}, {"agent", "new"}, {"agent", "edit"},
		{"agent", "rm"}, {"agent", "import"}, {"agent", "export"},
		{"policy", "show"}, {"policy", "create"}, {"policy", "edit"},
		{"policy", "rm"}, {"policy", "attach"}, {"policy", "detach"},
		{"policy", "check"},
		{"secret", "create"}, {"secret", "read"}, {"secret", "update"},
		{"secret", "delete"}, {"secret", "import"},
		{"exec"},
		// A run-line verb with no ref.
		{"info"}, {"run"}, {"sh"},
	} {
		assertUsageExit(t, args)
	}
}

// A word the verb has no place for, and a flag it does not define. Both are
// named in docs/cli.md's row for 2.
func TestAStrayWordOrUnknownFlagExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"ls", "extra"},
		{"agent", "ls", "extra"},
		{"doctor", "claude-code", "extra"},
		{"policy", "ls", "extra"},
		{"policy", "check", "claude-code", "extra"},
		{"policy", "show", "a", "b"},
		{"policy", "attach", "a", "b", "c"},
		{"secret", "read", "a", "b"},
		{"secret", "update", "a", "b"},
		{"secret", "ls", "extra"},
		{"telemetry", "status", "extra"},
		{"secret", "ls", "--bogus"},
		{"secret", "read", "--bogus"},
		{"secret", "import", "--bogus", "claude-code"},
		{"agent", "show", "--bogus", "claude-code"},
		{"agent", "edit", "--bogus"},
		{"agent", "import", "--bogus"},
		{"policy", "show", "--bogus", "no-net"},
		// A flag given no value is the same class: a value in the wrong place.
		{"policy", "check", "-n"},
		{"run", "claude-code", "--image"},
	} {
		assertUsageExit(t, args)
	}
}

// A flag given a value it cannot take, or two flags that contradict each
// other. The line is malformed before brig looks at anything outside it.
func TestABadFlagValueExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--mem", "lots", "claude-code"},
		{"run", "--offline=maybe", "claude-code"},
		{"--json=maybe", "ls"},
		{"run", "--offline", "--network", "open", "claude-code"},
		{"secret", "create", "gh-token", "-f", ""},
		{"secret", "create", "gh-token", "--stdin", "-f", "key"},
		{"secret", "import", "claude-code", "--from-command", ""},
	} {
		assertUsageExit(t, args)
	}
}

// The line the class stops at. Naming something that is not there is a
// well-formed command about an absent thing -- 3, not 2 -- and conflating the
// two would make 3 unreachable for these verbs.
func TestAWellFormedCommandIsNotAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"info", "nosuchagent"},
		{"agent", "show", "nosuchagent"},
	} {
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		t.Setenv("BRIG_POLICY_DIR", t.TempDir())
		var err error
		captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(args) })
		})
		if err == nil {
			t.Errorf("brig %v was accepted", args)
			continue
		}
		if got := exitCode(err); got != exitNotFound {
			t.Errorf("brig %v exits %d, want %d (no such thing): %v", args, got, exitNotFound, err)
		}
	}
}

func assertUsageExit(t *testing.T, args []string) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	var err error
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run(args) })
	})
	if err == nil {
		t.Errorf("brig %v was accepted", args)
		return
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("brig %v exits %d, want %d (the usage class): %v", args, got, exitUsage, err)
	}
}
