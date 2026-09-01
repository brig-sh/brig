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
	for _, verb := range []string{"run", "sh", "shell", "exec"} {
		if err := rejectTail(verb, []string{"-p", "hi"}); err != nil {
			t.Errorf("rejectTail(%q) refused an agent tail: %v", verb, err)
		}
	}
	// No tail is fine for every verb.
	for _, verb := range []string{"create", "stop", "rm", "env", "run", "sh", "shell", "exec"} {
		if err := rejectTail(verb, nil); err != nil {
			t.Errorf("rejectTail(%q, nil): %v", verb, err)
		}
	}
}

// ls names no session and takes only -q, and `rm --all` takes nothing at all,
// so any other argument is a token they would read and drop. `rm --all` is the
// sharp one: `brig rm --all --dry-run` reads like a preview and removes
// everything, so it must refuse before it reaches the runtime. reset is the
// retired spelling of the same command and refuses on the same terms.
func TestLsAndRemoveAllRefuseArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]string) error
		args []string
	}{
		{"ls", listSandboxes, []string{"claude"}},
		{"rm --all", func(args []string) error { return removeAll("brig rm --all", args) },
			[]string{"--dry-run"}},
		{"reset", func(args []string) error { return removeAll("brig reset", args) },
			[]string{"--dry-run"}},
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

// Every entry in brigFlags says where it is legal. The zero position is not a
// position, so a flag added to the table without one fails here rather than
// landing in whichever set the zero value happens to be: a flag that is
// silently global is refused everywhere it used to work, and a flag that is
// silently on the run line is quietly forwarded to the agent instead.
func TestBrigFlagsDeclareAPosition(t *testing.T) {
	for _, f := range brigFlags {
		switch f.position {
		case posGlobal, posRun:
		default:
			t.Errorf("brig flag --%s has no position", f.long)
		}
	}
}

// The positions brig owns are closed sets, and a token in one of them that
// brig does not own is a mistake rather than the agent's word. That only holds
// while brig's own spellings are not also the agent's -- where the two collide,
// brig reads the flag and the agent never sees it, and `--` is the only way
// through.
//
// The overlaps below are what that costs today. They are the reason #47
// retires -n, -t, -w and -m in v0.3, and the map is expected to shrink as it
// does; a spelling that starts colliding fails here first.
//
// claudeCodeFlags is hand-maintained. No profile spec declares its agent's
// flags -- internal/profile carries the image, the binary and the credentials
// and nothing about the CLI's grammar -- so there is nothing to derive this
// from. It was captured from `claude --help` (Claude Code 2.1.252) and covers
// that one agent: the other shipped profiles' CLIs are not on this machine and
// are not asserted here, so this test is exactly as strong as the list, which
// is to say it catches a collision with Claude Code and no more.
func TestBrigFlagsOverlapWithClaudeCodeOnlyWhereKnown(t *testing.T) {
	claudeCodeFlags := []string{
		"--add-dir", "--agent", "--agents", "--allow-dangerously-skip-permissions",
		"--allowed-tools", "--allowedTools", "--append-system-prompt", "--autocompact",
		"--ax-screen-reader", "--background", "--bare", "--betas", "--bg", "--brief",
		"--chrome", "--cloud", "--continue", "--dangerously-skip-permissions", "--debug",
		"--debug-file", "--disable-slash-commands", "--disallowed-tools", "--disallowedTools",
		"--effort", "--environment", "--exclude-dynamic-system-prompt-sections",
		"--fallback-model", "--file", "--fork-session", "--forward-subagent-text",
		"--from-pr", "--help", "--ide", "--include-hook-events",
		"--include-partial-messages", "--input-format", "--json-schema",
		"--max-budget-usd", "--mcp-config", "--model", "--name", "--no-chrome",
		"--no-session-persistence", "--output-format", "--permission-mode",
		"--plugin-dir", "--plugin-url", "--print", "--prompt-suggestions",
		"--remote-control", "--remote-control-session-name-prefix",
		"--replay-user-messages", "--restricted", "--resume", "--safe-mode",
		"--session-id", "--setting-sources", "--settings", "--strict-mcp-config",
		"--system-prompt", "--teleport", "--tmux", "--tools", "--verbose", "--version",
		"--worktree", "-c", "-d", "-h", "-n", "-p", "-r", "-v", "-w",
	}
	// The brig spelling -> why it still collides.
	knownOverlaps := map[string]string{
		"--name": "claude-code's own --name; brig's retires in #5",
		"-n":     "claude-code -n; retires in #5",
		"-w":     "claude-code -w/--worktree; brig's --workspace retires in #6",
		"-d":     "claude-code -d/--debug; brig's --detach keeps -d, so this one stays",
	}

	agent := make(map[string]bool, len(claudeCodeFlags))
	for _, f := range claudeCodeFlags {
		agent[f] = true
	}
	found := map[string]bool{}
	for _, f := range brigFlags {
		spellings := []string{"--" + f.long}
		if f.short != "" {
			spellings = append(spellings, "-"+f.short)
		}
		for _, s := range spellings {
			if !agent[s] {
				continue
			}
			found[s] = true
			if _, known := knownOverlaps[s]; !known {
				t.Errorf("brig's %s is also claude-code's, and brig reads it first, so "+
					"`brig run claude %s` never reaches the agent", s, s)
			}
		}
	}
	// And the other way, so the map shrinks when a spelling retires rather
	// than outliving the collision it documents.
	for s := range knownOverlaps {
		if !found[s] {
			t.Errorf("knownOverlaps still lists %s, which no longer collides; drop the entry", s)
		}
	}
}

// The global position -- left of the verb -- is closed. It holds no flags yet:
// every flag brig has is on the run line, and #24, #11 and #30 are what fill
// this in. Closed and empty is the point, so that a flag written there is
// named rather than read as a command.
func TestGlobalPositionRefusesAnUnknownToken(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "run", "claude"},
		{"--nope", "ls"},
		{"-y", "rm", "claude"},
	} {
		_, err := parseGlobal(args)
		if err == nil {
			t.Errorf("parseGlobal(%q) was accepted", args)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("parseGlobal(%q): %v, want a usageError", args, err)
		}
		if !strings.Contains(err.Error(), args[0]) {
			t.Errorf("parseGlobal(%q): %v, want it to name %q", args, err, args[0])
		}
	}
}

// The verb and everything after it is not the global position's business,
// including the flag-shaped spellings of a verb and every flag on the run
// line.
func TestGlobalPositionPassesTheVerbLineThrough(t *testing.T) {
	for _, args := range [][]string{
		{"run", "claude", "--name", "x"},
		{"-h"},
		{"--help"},
		{"--version"},
		{"version"},
		{"run", "claude", "--json"},
	} {
		rest, err := parseGlobal(args)
		if err != nil {
			t.Errorf("parseGlobal(%q): %v", args, err)
			continue
		}
		if strings.Join(rest, " ") != strings.Join(args, " ") {
			t.Errorf("parseGlobal(%q) = %q, want it untouched", args, rest)
		}
	}
}

// A ref names the session inline: `brig run claude@refactor` is what `brig run
// claude --name refactor` already does, so the label lands on the same path
// rather than becoming a second way to hold a session.
func TestParseReadsASessionRef(t *testing.T) {
	o, profileName, tail, err := parse([]string{"claude@refactor", "-p", "hi"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if profileName != "claude" {
		t.Errorf("profile = %q, want claude", profileName)
	}
	if o.load.Name != "refactor" || !o.nameGiven {
		t.Errorf("name = %q, given = %v, want refactor, true", o.load.Name, o.nameGiven)
	}
	if strings.Join(tail, " ") != "-p hi" {
		t.Errorf("tail = %q, want the agent's own arguments untouched", tail)
	}
	// The two spellings of one session agree.
	viaFlag, _, _, err := parse([]string{"claude", "--name", "refactor"})
	if err != nil {
		t.Fatalf("parse --name: %v", err)
	}
	if viaFlag.load != o.load || viaFlag.nameGiven != o.nameGiven {
		t.Errorf("claude@refactor = %+v, claude --name refactor = %+v", o.load, viaFlag.load)
	}
}

// A malformed ref is a usage error naming what was typed, not a profile brig
// then reports as missing.
func TestParseRefusesABadRef(t *testing.T) {
	for _, args := range [][]string{
		{"claude@"},
		{"claude@a@b"},
		{"claude@Refactor"},
		{"claude@my-very-long-label"},
		{"@refactor"},
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
	}
}

// Both spellings of the session on one line is a line that asks for two
// different sessions. A silent winner would run one of them with the other
// still written on the command.
func TestParseRefusesARefAndNameTogether(t *testing.T) {
	_, _, _, err := parse([]string{"claude@refactor", "--name", "other"})
	if err == nil {
		t.Fatal("claude@refactor --name other was accepted")
	}
	for _, want := range []string{"refactor", "other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v, want it to name %q", err, want)
		}
	}
}

// Right of the ref every token is the agent's, brig's own spellings included.
// So brig says so instead of taking it: `brig run claude -p hi --quiet` runs
// the agent with --quiet, which is what it has always done, and now says that
// brig is not the one reading it.
func TestParseWarnsAboutItsOwnFlagsInTheAgentTail(t *testing.T) {
	var (
		o    options
		tail []string
		err  error
	)
	warning := captureStderr(t, func() {
		o, _, tail, err = parse([]string{"claude", "-p", "hi", "--quiet", "--cpus=2"})
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Still forwarded, and still not read: the warning changes nothing about
	// what runs.
	if strings.Join(tail, " ") != "-p hi --quiet --cpus=2" {
		t.Errorf("tail = %q, want it forwarded untouched", tail)
	}
	if o.quiet || o.load.CPUs != 0 {
		t.Errorf("brig read the agent's arguments: quiet %v, cpus %d", o.quiet, o.load.CPUs)
	}
	for _, want := range []string{"--quiet", "--cpus"} {
		if !strings.Contains(warning, want) {
			t.Errorf("nothing warned about %s:\n%s", want, warning)
		}
	}
}

// A `--` is the user saying the rest is the agent's, so there is nothing to
// point out about what follows it. Warning there would put a line on stderr
// for every scripted `brig exec claude -- ls` that happens to name one.
func TestParseSaysNothingAfterAnExplicitEnd(t *testing.T) {
	warning := captureStderr(t, func() {
		if _, _, tail, err := parse([]string{"claude", "--", "--quiet"}); err != nil {
			t.Errorf("parse: %v", err)
		} else if strings.Join(tail, " ") != "--quiet" {
			t.Errorf("tail = %q", tail)
		}
	})
	if strings.Contains(warning, "--quiet") {
		t.Errorf("brig warned about a token past --:\n%s", warning)
	}
}

// --mem is the spelling; --memory is what it was called and keeps working.
func TestParseReadsMemAndMemory(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--mem", "2048"},
		{"claude", "--mem=2048"},
		{"claude", "--memory", "2048"},
	} {
		o, _, _, err := parse(args)
		if err != nil {
			t.Errorf("parse(%q): %v", args, err)
			continue
		}
		if o.load.Mem != 2048 {
			t.Errorf("parse(%q) = mem %d, want 2048", args, o.load.Mem)
		}
	}
	// And it is brig's, so it is refused by name rather than forwarded.
	if _, _, _, err := parse([]string{"claude", "--mem", "lots"}); err == nil {
		t.Error("--mem lots was accepted")
	} else if !strings.Contains(err.Error(), "--mem") {
		t.Errorf("--mem lots: %v, want it to name --mem", err)
	}
}

// The lines that work today still work. Every one of these is a shape someone
// has typed or scripted, and the grammar change is only worth having if none
// of them moves.
func TestParseKeepsTodaysLines(t *testing.T) {
	for _, c := range []struct {
		args []string
		name string
		tail string
	}{
		{[]string{"claude", "--name", "x"}, "x", ""},
		{[]string{"claude", "-q"}, "", ""},
		{[]string{"claude", "-p", "hi"}, "", "-p hi"},
		{[]string{"claude", "--", "--name"}, "", "--name"},
		{[]string{"claude", "--", "ls"}, "", "ls"},
		{[]string{"--name", "x", "claude"}, "x", ""},
	} {
		o, profileName, tail, err := parse(c.args)
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if profileName != "claude" || o.load.Name != c.name || strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = (%q, %q, %q), want (claude, %q, %q)",
				c.args, profileName, o.load.Name, tail, c.name, c.tail)
		}
	}
}
