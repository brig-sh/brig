package main

import (
	"slices"
	"strings"
	"testing"
)

// One word with a space or a shell operator in it is almost always a script
// typed the way ssh takes one. The guest looks it up as a command name and
// fails with a message about a missing file, so brig names the fix first.
// Several words, a plain word, or a line that already says -c get nothing.
func TestShellHint(t *testing.T) {
	for _, tc := range []struct {
		tail []string
		hint bool
	}{
		{[]string{"ls /work | wc -l"}, true},
		{[]string{"make;make test"}, true},
		{[]string{"a&&b"}, true},
		{[]string{"cat ~/.bashrc"}, true},
		{[]string{"ls>out"}, true},
		{[]string{"cat<f"}, true},
		{[]string{"echo$HOME"}, true},
		{[]string{"$(id)"}, true},
		{[]string{"echo `id`"}, true},
		// A program path with a space in it is a real command, and -c
		// would split it.
		{[]string{"/work/My Tools/run"}, false},
		{[]string{"ls"}, false},
		{[]string{"ls", "/work dir"}, false},
		{[]string{"-c", "ls | wc -l"}, false},
		{nil, false},
	} {
		got := shellHint(tc.tail)
		if (got != "") != tc.hint {
			t.Errorf("shellHint(%q) = %q, want a hint: %v", tc.tail, got, tc.hint)
		}
		if tc.hint && !strings.Contains(got, "-c") {
			t.Errorf("shellHint(%q) = %q, does not name -c", tc.tail, got)
		}
	}
}

// -c with no script after it is refused before anything boots, on sh and on a
// shell profile's run line, rather than handed to bash to complain about.
func TestShScriptFlagNeedsAScript(t *testing.T) {
	for _, args := range [][]string{
		{"sh", "claude", "-c"},
		{"sh", "claude", "-c", ""},
		{"sh", "claude", "-ec"},
		{"run", "ubuntu", "--", "-c"},
	} {
		scratchHost(t)
		_, err := captureStdout(t, func() error { return run(args) })
		if took(err) {
			t.Errorf("brig %s was accepted: %v", strings.Join(args, " "), err)
		}
	}
}

// -c is not one of brig's flags, so it reaches the guest command as its first
// word, where shellArgv reads it, and brig says nothing about it on the way.
// Owning it in the flag table would warn on every agent that has a -c of its
// own, `brig run claude -c` included.
func TestShScriptFlagReachesTheTail(t *testing.T) {
	var tail []string
	var err error
	stderr := captureStderr(t, func() {
		_, _, tail, err = parse("sh", []string{"claude", "-c", "ls | wc -l"})
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []string{"-c", "ls | wc -l"}; !slices.Equal(tail, want) {
		t.Errorf("tail = %q, want %q", tail, want)
	}
	if stderr != "" {
		t.Errorf("parse printed %q", stderr)
	}
}

// The hint is printed before anything boots, so it shows even when the run
// goes no further, and -q silences it like any other notice.
func TestShScriptHintPrintsBeforeBoot(t *testing.T) {
	scratchHost(t)
	stderr := captureStderr(t, func() { _ = run([]string{"sh", "claude", "ls /work | wc -l"}) })
	if !strings.Contains(stderr, "put -c in front of it") {
		t.Errorf("no hint on stderr: %q", stderr)
	}

	scratchHost(t)
	stderr = captureStderr(t, func() { _ = run([]string{"-q", "sh", "claude", "ls /work | wc -l"}) })
	if strings.Contains(stderr, "put -c in front of it") {
		t.Errorf("-q printed the hint: %q", stderr)
	}
}

// brig run -d starts the sandbox and runs nothing, so a hint about what the
// guest will do with the command is wrong there.
func TestShScriptHintNotOnDetach(t *testing.T) {
	scratchHost(t)
	stderr := captureStderr(t, func() { _ = run([]string{"run", "-d", "ubuntu", "--", "ls /work | wc -l"}) })
	if strings.Contains(stderr, "put -c in front of it") {
		t.Errorf("run -d printed the hint: %q", stderr)
	}
}
