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
		_, _, _, err := parse("run", args)
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
	_, name, tail, err := parse("run", []string{"claude", "--nope"})
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
		{"ls", func(args []string) error { return listSandboxes(args, false) }, []string{"claude"}},
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
	o, profileName, _, err := parse("run", []string{"--network", "offline", "claude"})
	if err != nil {
		t.Fatalf("--network offline: %v", err)
	}
	if profileName != "claude" {
		t.Errorf("profile = %q, want claude", profileName)
	}
	if o.load.Network != "offline" {
		t.Errorf("network = %q, want offline", o.load.Network)
	}

	if o, _, _, err = parse("run", []string{"--offline", "claude"}); err != nil {
		t.Fatalf("--offline: %v", err)
	}
	if o.load.Network != "offline" {
		t.Errorf("--offline gave network %q, want offline", o.load.Network)
	}

	// Saying both, and disagreeing, is a mistake worth naming rather than a
	// silent precedence puzzle.
	if _, _, _, err = parse("run", []string{"--offline", "--network", "shared", "claude"}); err == nil {
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
// The overlaps below are what that costs today, one map per shipped profile.
// They are the reason #47 retires -n, -t, -w and -m in v0.3, and the maps are
// expected to shrink as it does; a spelling that starts colliding, or one whose
// collision has gone, fails here first so a map cannot outlive what it records.
//
// The flag lists are hand-maintained. No profile spec declares its agent's
// flags -- internal/profile carries the image, the binary and the credentials
// and nothing about the CLI's grammar -- so there is nothing to derive them
// from, and this test is exactly as strong as they are. Each list says where it
// came from, because the strength is not uniform:
//
//   - A local capture from the agent's own `--help` is the strong tier: it is
//     the grammar the installed binary actually answers to.
//   - A published CLI reference is the weaker tier: it is what the docs say,
//     which can lag or lead the binary, and the agent is not on this machine to
//     check against.
//
// All lists were captured 2026-09-05.
func TestBrigFlagsOverlapWithShippedAgentsOnlyWhereKnown(t *testing.T) {
	// The brig spelling -> why it still collides, per profile.
	profiles := []struct {
		name          string
		agentFlags    []string
		knownOverlaps map[string]string
	}{
		{
			// Local capture from `claude --help`, Claude Code 2.1.261.
			name: "claude-code",
			agentFlags: []string{
				"--add-dir", "--agent", "--agents", "--all",
				"--allow-dangerously-skip-permissions", "--allowed-tools", "--allowedTools",
				"--append-system-prompt", "--autocompact", "--ax-screen-reader",
				"--background", "--bare", "--betas", "--bg", "--brief", "--chrome",
				"--cloud", "--continue", "--dangerously-skip-permissions", "--debug",
				"--debug-file", "--disable-slash-commands", "--disallowed-tools",
				"--disallowedTools", "--effort", "--environment",
				"--exclude-dynamic-system-prompt-sections", "--fallback-model", "--file",
				"--fork-session", "--forward-subagent-text", "--from-pr", "--help", "--ide",
				"--include-hook-events", "--include-partial-messages", "--input-format",
				"--json-schema", "--max-budget-usd", "--mcp-config", "--model", "--name",
				"--no-chrome", "--no-session-persistence", "--output-format",
				"--permission-mode", "--permission-prompt-tool", "--permission-prompts",
				"--plugin-dir", "--plugin-url", "--print", "--prompt-suggestions",
				"--remote-control", "--remote-control-session-name-prefix",
				"--replay-user-messages", "--restricted", "--resume", "--safe-mode",
				"--session-id", "--setting-sources", "--settings", "--strict-mcp-config",
				"--system-prompt", "--system-prompt-snapshot", "--teleport", "--tmux",
				"--tools", "--verbose", "--version", "--worktree",
				"-c", "-d", "-h", "-n", "-p", "-r", "-v", "-w",
			},
			knownOverlaps: map[string]string{
				"--name": "claude-code's own --name; brig's retires onto the ref in #47",
				"-n":     "claude-code -n; brig's retires onto the ref in #47",
				"-w":     "claude-code -w/--worktree; brig's --workspace retires onto --home in #47",
				"-d":     "claude-code -d/--debug; brig's --detach keeps -d, so this one stays",
			},
		},
		{
			// Local capture from `codex --help`.
			name: "codex",
			agentFlags: []string{
				"--add-dir", "--approve-for-me", "--ask-for-approval", "--cd", "--config",
				"--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust",
				"--disable", "--enable", "--help", "--image", "--last", "--local-provider",
				"--model", "--no-alt-screen", "--oss", "--profile", "--remote",
				"--remote-auth-token-env", "--sandbox", "--search", "--strict-config",
				"--version", "-a", "-c", "-C", "-h", "-i", "-m", "-p", "-s", "-V",
			},
			knownOverlaps: map[string]string{
				"--image": "codex's own --image; brig keeps --image, so this one stays",
				"-m":      "codex -m/--model; brig's -m/--memory retires onto --mem in #47",
			},
		},
		{
			// Local capture from `cursor-agent --help`, the binary the cursor
			// profile runs.
			name: "cursor",
			agentFlags: []string{
				"--api-key", "--approve-mcps", "--cloud", "--continue", "--force", "--header",
				"--help", "--list-models", "--mode", "--model", "--output-format", "--plan",
				"--print", "--resume", "--sandbox", "--skip-worktree-setup",
				"--stream-partial-output", "--trust", "--version", "--workspace", "--worktree",
				"--yolo", "-c", "-f", "-h", "-H", "-p", "-v", "-w",
			},
			knownOverlaps: map[string]string{
				"--workspace": "cursor's own --workspace; brig's retires onto --home in #47",
				"-w":          "cursor -w/--worktree; brig's --workspace retires onto --home in #47",
			},
		},
		{
			// Local capture from `grok --help`.
			name: "grok",
			agentFlags: []string{
				"--allow-host", "--allow-net", "--api-key", "--background-task-file",
				"--base-url", "--batch-api", "--directory", "--format", "--help",
				"--max-tool-rounds", "--model", "--no-sandbox", "--port", "--prompt",
				"--sandbox", "--session", "--update", "--verify", "--version",
				"-d", "-h", "-k", "-m", "-p", "-s", "-u", "-V",
			},
			knownOverlaps: map[string]string{
				"-d": "grok -d/--directory; brig's --detach keeps -d, so this one stays",
				"-m": "grok -m/--model; brig's -m/--memory retires onto --mem in #47",
			},
		},
		{
			// Not installed here. Published CLI reference at
			// https://geminicli.com/docs/cli/cli-reference/ , read 2026-09-05.
			name: "gemini",
			agentFlags: []string{
				"--allowed-mcp-server-names", "--allowed-tools", "--approval-mode", "--debug",
				"--delete-session", "--experimental-acp", "--experimental-zed-integration",
				"--extensions", "--help", "--include-directories", "--list-extensions",
				"--list-sessions", "--model", "--output-format", "--prompt",
				"--prompt-interactive", "--resume", "--sandbox", "--screen-reader",
				"--skip-trust", "--version", "--worktree", "--yolo",
				"-d", "-e", "-h", "-i", "-l", "-m", "-o", "-p", "-r", "-s", "-v", "-w", "-y",
			},
			knownOverlaps: map[string]string{
				"-d": "gemini -d/--debug; brig's --detach keeps -d, so this one stays",
				"-m": "gemini -m/--model; brig's -m/--memory retires onto --mem in #47",
				"-w": "gemini -w/--worktree; brig's --workspace retires onto --home in #47",
			},
		},
		{
			// Not installed here. Published CLI reference at
			// https://opencode.ai/docs/cli/ , read 2026-09-05. This is the union
			// of the global flags and every subcommand's flags, which is the right
			// scope: brig forwards the whole tail, so `brig run opencode serve
			// --port 3000` reaches a subcommand flag as directly as a global one.
			name: "opencode",
			agentFlags: []string{
				"--agent", "--attach", "--auto", "--command", "--continue", "--cors",
				"--days", "--description", "--dir", "--dry-run", "--event", "--file",
				"--force", "--fork", "--format", "--help", "--hostname", "--keep-config",
				"--keep-data", "--log-level", "--max-count", "--mdns", "--mdns-domain",
				"--method", "--mode", "--model", "--password", "--path", "--permissions",
				"--port", "--print-logs", "--prompt", "--provider", "--pure", "--refresh",
				"--sanitize", "--session", "--thinking", "--title", "--token", "--tools",
				"--username", "--variant", "--verbose", "--version",
				"-c", "-f", "-h", "-m", "-n", "-p", "-s", "-u", "-v",
			},
			knownOverlaps: map[string]string{
				"-n": "opencode -n; brig's retires onto the ref in #47",
				"-m": "opencode -m/--model; brig's -m/--memory retires onto --mem in #47",
			},
		},
		{
			// claude-desktop is kind: gui. The app owns the console and brig execs
			// no binary, so there is no flag vocabulary to collide with. The empty
			// list accounts for the profile rather than forgetting it.
			name:          "claude-desktop",
			agentFlags:    nil,
			knownOverlaps: map[string]string{},
		},
		{
			// ubuntu is kind: shell. It runs a shell, not an agent CLI, so the
			// same holds: nothing to collide with, listed so it is accounted for.
			name:          "ubuntu",
			agentFlags:    nil,
			knownOverlaps: map[string]string{},
		},
	}

	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			agent := make(map[string]bool, len(p.agentFlags))
			for _, f := range p.agentFlags {
				agent[f] = true
			}
			found := map[string]bool{}
			for _, f := range brigFlags {
				// Only the run line can collide. A global flag stands left of the
				// verb and the agent's words stand right of the ref, so the two
				// never occupy the same place: `brig --verbose run claude` is
				// brig's and `brig run claude --verbose` is claude's, and neither
				// reading is in doubt. That is a reason to put a flag brig shares
				// with an agent in the global position rather than a reason to list
				// it here.
				if f.position == posGlobal {
					continue
				}
				spellings := []string{"--" + f.long}
				if f.short != "" {
					spellings = append(spellings, "-"+f.short)
				}
				for _, s := range spellings {
					if !agent[s] {
						continue
					}
					found[s] = true
					if _, known := p.knownOverlaps[s]; !known {
						t.Errorf("brig's %s is also %s's, and brig reads it first, so "+
							"`brig run %s %s` never reaches the agent", s, p.name, p.name, s)
					}
				}
			}
			// And the other way, so a map shrinks when a spelling retires rather
			// than outliving the collision it documents.
			for s := range p.knownOverlaps {
				if !found[s] {
					t.Errorf("knownOverlaps for %s still lists %s, which no longer collides; "+
						"drop the entry", p.name, s)
				}
			}
		})
	}
}

// Global flags go left of the verb, and the set is closed: an unknown token
// there is a usage error naming it, not an operand and not an agent argument.
// No agent is named yet at that point, so nothing else can claim it.
//
// The tokens below are not brig flags. Adding one as a global flag means
// removing it here.
func TestGlobalPositionRefusesAnUnknownToken(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "run", "claude"},
		{"--nope", "ls"},
		{"-y", "rm", "claude"},
	} {
		_, _, err := parseGlobal(args)
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
		_, rest, err := parseGlobal(args)
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
	o, profileName, tail, err := parse("run", []string{"claude@refactor", "-p", "hi"})
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
	viaFlag, _, _, err := parse("run", []string{"claude", "--name", "refactor"})
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
		// Long is not what refuses this -- a label is not shortened, so there
		// is no length to refuse -- the spaces are.
		{"claude@my very long label"},
		{"@refactor"},
	} {
		_, _, _, err := parse("run", args)
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
	_, _, _, err := parse("run", []string{"claude@refactor", "--name", "other"})
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
		o, _, tail, err = parse("run", []string{"claude", "-p", "hi", "--quiet", "--cpus=2"})
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
		if _, _, tail, err := parse("run", []string{"claude", "--", "--quiet"}); err != nil {
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
		o, _, _, err := parse("run", args)
		if err != nil {
			t.Errorf("parse(%q): %v", args, err)
			continue
		}
		if o.load.Mem != 2048 {
			t.Errorf("parse(%q) = mem %d, want 2048", args, o.load.Mem)
		}
	}
	// And it is brig's, so it is refused by name rather than forwarded.
	if _, _, _, err := parse("run", []string{"claude", "--mem", "lots"}); err == nil {
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
		o, profileName, tail, err := parse("run", c.args)
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
