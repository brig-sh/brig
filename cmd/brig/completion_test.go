package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// completionHost sets up the state the engine reads: one file-backed profile,
// two sessions in the index, one policy. Every case below shares it, so the
// only variable is where the cursor is.
//
// The built-ins are present too, since Load adds to the registry rather than
// replacing it; the prefix-matching cases rely on that.
func completionHost(t *testing.T) {
	t.Helper()

	profiles := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", profiles)
	write(t, filepath.Join(profiles, "mine.yaml"), `name: mine
kind: shell
image: docker.io/library/ubuntu:24.04
guestHome: /root
mem: 2048
cpus: 2
`)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}

	state := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", state)
	index := map[string]map[string]string{
		"claude-code":          {"home": "/tmp/one", "sandbox": "brig-claude-code"},
		"claude-code@refactor": {"home": "/tmp/two", "sandbox": "brig-claude-code-refactor"},
	}
	blob, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "sessions.json"), string(blob))

	policies := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", policies)
	write(t, filepath.Join(policies, "no-net.yaml"), `apiVersion: brig.sh/v1alpha1
name: no-net
desc: nothing leaves
egress:
  default: deny
`)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One table: a line, a cursor position, and what is legal there.
func TestCompletePositions(t *testing.T) {
	completionHost(t)

	cases := []struct {
		name string
		// words is the line with `brig` dropped; the last of them is the word
		// under the cursor, empty when the cursor is on fresh whitespace.
		words     []string
		directive string
		// want are candidates that must be offered, absent ones that must not.
		want    []string
		absent  []string
		exactly []string
	}{{
		name:      "a bare line offers the verbs",
		words:     []string{""},
		directive: dirNames,
		want:      []string{"run", "sh", "stop", "rm", "ls", "info", "agent", "completion"},
		// The retired spellings still work and print what replaced them, but
		// they are removed in v0.3, so completion does not offer them.
		absent: []string{"exec", "shell", "env", "create", "reset", "profiles", "policies", completeVerb},
	}, {
		name:      "left of the verb the flags are the global ones",
		words:     []string{"-"},
		directive: dirNames,
		// Read off brigFlags rather than written out. A hand-kept copy of this
		// set is what broke: --json became global in #7 and this case still
		// named the three flags that came before it, so two changes that were
		// each correct left main red when they met. The table is the one place
		// a flag declares its position, so the test asks it.
		exactly: globalFlagSpellings(),
	}, {
		name:      "a global flag does not hide the verb after it",
		words:     []string{"-q", "r"},
		directive: dirNames,
		exactly:   []string{"rm", "run"},
	}, {
		name:      "the ref position offers agents and the sessions under them",
		words:     []string{"run", "claude-code"},
		directive: dirNames,
		exactly:   []string{"claude-code", "claude-code@refactor"},
	}, {
		name:      "the separator narrows to that agent's sessions",
		words:     []string{"run", "claude-code@"},
		directive: dirNames,
		exactly:   []string{"claude-code@refactor"},
	}, {
		name:      "a profile of your own is a ref like any other",
		words:     []string{"run", "mi"},
		directive: dirNames,
		exactly:   []string{"mine"},
	}, {
		name:      "run-line flags are brig's own, up to the ref",
		words:     []string{"run", "--"},
		directive: dirNames,
		want:      []string{"--home", "--image", "--mem", "--cpus", "--network", "--skills", "--detach", "--no-project", "--offline"},
		// --name and --workspace are deprecated; --memory is undocumented; -q
		// on the run line is the retiring position, not the flag.
		absent: []string{"--name", "--workspace", "--memory", "--quiet", "--verbose"},
	}, {
		name:      "a flag only run acts on is not offered by a verb that continues a session",
		words:     []string{"sh", "--"},
		directive: dirNames,
		absent:    []string{"--detach", "--no-project"},
	}, {
		name:      "rm offers the flag that names no session",
		words:     []string{"rm", "--a"},
		directive: dirNames,
		exactly:   []string{"--all"},
	}, {
		name:      "a closed set completes its own words",
		words:     []string{"run", "claude", "--network", ""},
		directive: dirNames,
		exactly:   []string{"isolated", "offline", "shared"},
	}, {
		name:      "a flag naming a host directory hands the slot to the shell",
		words:     []string{"run", "--home", ""},
		directive: dirDirs,
	}, {
		// bash splits on '=', so the inline spelling arrives as three tokens.
		name:      "the inline spelling of a flag completes like the spaced one",
		words:     []string{"run", "--network", "=", "off"},
		directive: dirNames,
		exactly:   []string{"offline"},
	}, {
		name:      "on run the word after the ref is the project brig mounts",
		words:     []string{"run", "claude", ""},
		directive: dirDirs,
	}, {
		name:      "and only the first one",
		words:     []string{"run", "claude", "./project", ""},
		directive: dirNone,
	}, {
		name:      "no other verb has a project slot",
		words:     []string{"sh", "claude", ""},
		directive: dirNone,
	}, {
		// A flag right of the ref is brig's if brigFlags has it. An unknown
		// one belongs to the agent, and brig has no list for that.
		name:      "brig's own flags stand on either side of the ref",
		words:     []string{"run", "claude", "--"},
		directive: dirNames,
		want:      []string{"--mem", "--home"},
	}, {
		name:      "and everything past the end of brig's parsing is",
		words:     []string{"run", "claude", "--", "-"},
		directive: dirNone,
	}, {
		name:      "stop offers the sessions there are, not the agents there could be",
		words:     []string{"stop", ""},
		directive: dirNames,
		exactly:   []string{"claude-code", "claude-code@refactor"},
	}, {
		name:      "info reads a profile without booting one, so it takes any agent",
		words:     []string{"info", "mi"},
		directive: dirNames,
		exactly:   []string{"mine"},
	}, {
		name:      "ls takes one flag and no operand",
		words:     []string{"ls", ""},
		directive: dirNone,
	}, {
		name:      "a noun command offers its subcommands",
		words:     []string{"agent", ""},
		directive: dirNames,
		exactly:   []string{"edit", "export", "import", "ls", "new", "rm", "show"},
		// list, save and load still work and are in no help text.
	}, {
		name:      "a subcommand that opens a file offers only agents that have one",
		words:     []string{"agent", "edit", ""},
		directive: dirNames,
		exactly:   []string{"mine"},
	}, {
		name:      "a subcommand that reads an agent offers the built-ins too",
		words:     []string{"agent", "show", "claude-c"},
		directive: dirNames,
		want:      []string{"claude-code"},
	}, {
		name:      "a flag's value is completed by what the flag names",
		words:     []string{"agent", "new", "ours", "--from", "claude-c"},
		directive: dirNames,
		want:      []string{"claude-code"},
	}, {
		name:      "a subcommand taking a file hands the slot to the shell",
		words:     []string{"agent", "import", ""},
		directive: dirFiles,
	}, {
		name:      "policies are completed where a policy goes",
		words:     []string{"policy", "attach", ""},
		directive: dirNames,
		exactly:   []string{"no-net"},
	}, {
		name:      "and the profile it binds to, in the slot after it",
		words:     []string{"policy", "attach", "no-net", "mi"},
		directive: dirNames,
		exactly:   []string{"mine"},
	}, {
		// Listing them opens the store, and a keystroke must not.
		name:      "secret names are not offered",
		words:     []string{"secret", "read", ""},
		directive: dirNone,
	}, {
		name:      "the store's own subcommands are",
		words:     []string{"secret", ""},
		directive: dirNames,
		exactly:   []string{"create", "delete", "import", "ls", "read", "update"},
	}, {
		name:      "telemetry is three words",
		words:     []string{"telemetry", ""},
		directive: dirNames,
		exactly:   []string{"off", "on", "status"},
	}, {
		name:      "completion offers the shells it has",
		words:     []string{"completion", ""},
		directive: dirNames,
		exactly:   []string{"bash", "fish", "zsh"},
	}, {
		name:      "a verb brig does not have completes nothing",
		words:     []string{"bogus", ""},
		directive: dirNone,
	}, {
		// With the cursor straight after '=', the separator is the current
		// word. It reads as an empty value prefix.
		name:      "the separator under the cursor starts the value",
		words:     []string{"run", "--network", "="},
		directive: dirNames,
		exactly:   []string{"isolated", "offline", "shared"},
	}, {
		name:      "an inline value is consumed with its separator, not read as the ref",
		words:     []string{"run", "--mem", "=", "4096", ""},
		directive: dirNames,
		want:      []string{"claude-code", "claude-code@refactor"},
	}, {
		name:      "and it does not shift a group's operand slots",
		words:     []string{"policy", "attach", "--name", "=", "x", ""},
		directive: dirNames,
		exactly:   []string{"no-net"},
	}, {
		// split() reads brig's flags on both sides of the ref, so completion
		// offers them on both sides. Withholding them after the ref left
		// `brig run claude --mem` uncompletable.
		name:      "brig's own flag is offered right of the ref",
		words:     []string{"run", "claude", "--m"},
		directive: dirNames,
		exactly:   []string{"--mem"},
	}, {
		// And stops where split() stops: a second positional starts the
		// agent's argv, so a brig flag after it belongs to the agent.
		name:      "but not once the agent's arguments have begun",
		words:     []string{"run", "claude", "./project", "npm", "--m"},
		directive: dirNone,
	}, {
		name:      "nor is a flag's value completed into the agent's argv",
		words:     []string{"sh", "claude", "npm", "--network", ""},
		directive: dirNone,
	}, {
		name:      "a continuing verb reads brig's flags after its ref too",
		words:     []string{"stop", "claude-code", "--m"},
		directive: dirNames,
		exactly:   []string{"--mem"},
	}, {
		name:      "and the tail begins at its first bare word past the ref",
		words:     []string{"stop", "claude-code", "extra", "--m"},
		directive: dirNone,
	}, {
		// --all replaces the ref, so no ref is offered after it. The flags
		// that go with it are.
		name:      "rm --all offers no ref",
		words:     []string{"rm", "--all", ""},
		directive: dirNone,
	}, {
		name:      "rm --all offers the answer and the preview",
		words:     []string{"rm", "--all", "--"},
		directive: dirNames,
		exactly:   []string{"--dry-run", "--yes"},
	}, {
		// --dry-run is read wherever it stands on the rm line, before or
		// after the ref.
		name:      "rm offers --dry-run beside a ref",
		words:     []string{"rm", "claude-code", "--d"},
		directive: dirNames,
		exactly:   []string{"--dry-run"},
	}, {
		name:      "and --all is offered only where the ref would have gone",
		words:     []string{"rm", "--a"},
		directive: dirNames,
		exactly:   []string{"--all"},
	}, {
		// Once a ref is named, --all is not offered. No other brig flag starts
		// "--a", so the correct answer is nothing.
		name:      "a ref named, --all is not on offer",
		words:     []string{"rm", "claude-code", "--a"},
		directive: dirNone,
	}, {
		name:      "a secret write takes a file, and says so",
		words:     []string{"secret", "create", "gh-token", "-"},
		directive: dirNames,
		exactly:   []string{"--file", "--stdin", "-f"},
	}, {
		name:      "and the file is the shell's to complete",
		words:     []string{"secret", "create", "gh-token", "-f", ""},
		directive: dirFiles,
	}, {
		// Only bash has "=" in COMP_WORDBREAKS. zsh and fish send
		// `--network=is` as one token, so it arrives as the current word and
		// would otherwise be matched against flag names.
		//
		// The candidate carries the flag because the shell replaces the whole
		// current word: a bare "isolated" would replace `--network=is` with
		// `isolated`.
		name:      "an inline value under the cursor completes the value",
		words:     []string{"run", "--network=is"},
		directive: dirNames,
		exactly:   []string{"--network=isolated"},
	}, {
		name:      "with the whole set when nothing is typed after the separator",
		words:     []string{"run", "--network="},
		directive: dirNames,
		exactly:   []string{"--network=isolated", "--network=offline", "--network=shared"},
	}, {
		name:      "the split shape keeps its bare values, as bash needs",
		words:     []string{"run", "--network", "=", "is"},
		directive: dirNames,
		exactly:   []string{"isolated"},
	}, {
		name:      "a group flag's value completes inline too",
		words:     []string{"agent", "new", "ours", "--from=claude-c"},
		directive: dirNames,
		exactly:   []string{"--from=claude-code"},
	}, {
		// A path cannot be qualified this way: the shell would complete it
		// against the flag text. Offering nothing leaves the inline spelling
		// where it already was in these two shells.
		name:      "an inline path is not completed",
		words:     []string{"run", "--home=/pa"},
		directive: dirNone,
	}, {
		name:      "an inline value on a flag brig does not have completes nothing",
		words:     []string{"run", "--bogus=x"},
		directive: dirNone,
	}, {
		name:      "a flag that already has its value leaves the ref next",
		words:     []string{"run", "--network=offline", ""},
		directive: dirNames,
		want:      []string{"claude-code", "claude-code@refactor"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directive, got := complete(tc.words)
			if directive != tc.directive {
				t.Errorf("brig %s: directive %q, want %q (candidates %v)",
					strings.Join(tc.words, " "), directive, tc.directive, got)
			}
			if tc.exactly != nil && !equal(got, tc.exactly) {
				t.Errorf("brig %s: candidates %v, want exactly %v",
					strings.Join(tc.words, " "), got, tc.exactly)
			}
			for _, want := range tc.want {
				if !has(got, want) {
					t.Errorf("brig %s: %q is not offered (got %v)",
						strings.Join(tc.words, " "), want, got)
				}
			}
			for _, absent := range tc.absent {
				if has(got, absent) {
					t.Errorf("brig %s: %q is offered and should not be",
						strings.Join(tc.words, " "), absent)
				}
			}
		})
	}
}

// :none, :dirs and :files never carry candidates; :names is never empty. The
// scripts read the first line and branch on it, so the two must agree.
func TestCompleteAnswersAreWellFormed(t *testing.T) {
	completionHost(t)

	lines := [][]string{
		{""}, {"-"}, {"run", ""}, {"run", "--"}, {"run", "claude", ""},
		{"run", "claude", "-"}, {"stop", ""}, {"agent", ""}, {"agent", "import", ""},
		{"policy", "show", ""}, {"secret", "read", ""}, {"bogus", ""}, {},
	}
	for _, words := range lines {
		directive, candidates := complete(words)
		switch directive {
		case dirNames:
			if len(candidates) == 0 {
				t.Errorf("brig %s: %s with nothing to offer", strings.Join(words, " "), directive)
			}
		case dirNone, dirDirs, dirFiles:
			if len(candidates) > 0 {
				t.Errorf("brig %s: %s carrying candidates %v", strings.Join(words, " "), directive, candidates)
			}
		default:
			t.Errorf("brig %s: unknown directive %q", strings.Join(words, " "), directive)
		}
	}
}

// The engine runs on a keystroke, so it must write nothing to stderr. A
// profile that will not parse is the case that would otherwise print: every
// other verb warns about it. Here it costs a candidate instead.
func TestCompleteIsSilentAboutABrokenProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	write(t, filepath.Join(dir, "broken.yaml"), "name: [unclosed\n")

	var out bytes.Buffer
	noise := captureStderr(t, func() { completeCmd(&out, []string{"run", ""}) })

	if noise != "" {
		t.Errorf("completion wrote %q to stderr; a keystroke prints nothing", noise)
	}
	if !strings.HasPrefix(out.String(), dirNames+"\n") {
		t.Errorf("completion answered %q, want candidates for the ref position", out.String())
	}
}

// A directive line, then one candidate per line. This is all the scripts
// parse, so it gets its own test.
func TestCompleteWireFormat(t *testing.T) {
	completionHost(t)

	var out bytes.Buffer
	completeCmd(&out, []string{"telemetry", ""})
	if got, want := out.String(), ":names\noff\non\nstatus\n"; got != want {
		t.Errorf("answer %q, want %q", got, want)
	}

	// A directive with no candidates: past `--` the line is the agent's.
	out.Reset()
	completeCmd(&out, []string{"run", "claude", "--", "-"})
	if got, want := out.String(), ":none\n"; got != want {
		t.Errorf("answer %q, want %q", got, want)
	}
}

// The hidden verb is reached the way a script reaches it: the guard `--` and
// then the words. It answers before the dispatcher's own notices, so nothing a
// keystroke did not ask for reaches the terminal.
func TestCompleteThroughTheDispatcher(t *testing.T) {
	completionHost(t)

	var out string
	noise := captureStderr(t, func() {
		got, err := captureStdout(t, func() error {
			return run([]string{completeVerb, "--", "telemetry", ""})
		})
		if err != nil {
			t.Fatal(err)
		}
		out = got
	})

	if want := ":names\noff\non\nstatus\n"; out != want {
		t.Errorf("brig %s -- telemetry '': %q, want %q", completeVerb, out, want)
	}
	if noise != "" {
		t.Errorf("brig %s wrote %q to stderr", completeVerb, noise)
	}
}

// Every shell named in the usage text has a script, and every script handles
// every directive the engine emits.
func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var out bytes.Buffer
		if err := completionCmd(&out, []string{shell}); err != nil {
			t.Fatalf("brig completion %s: %v", shell, err)
		}
		script := out.String()
		if !strings.Contains(script, completeVerb) {
			t.Errorf("the %s script asks brig nothing", shell)
		}
		// A script missing a directive falls back to filenames where the
		// engine asked for silence.
		for _, directive := range []string{dirNames, dirDirs, dirFiles} {
			if !strings.Contains(script, directive) {
				t.Errorf("the %s script does not handle %s", shell, directive)
			}
		}
	}

	var out bytes.Buffer
	if err := completionCmd(&out, []string{"tcsh"}); err == nil {
		t.Error("brig completion tcsh: no error for a shell brig has no script for")
	}
	if err := completionCmd(&out, nil); err == nil {
		t.Error("brig completion: no error with no shell named")
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The groups table duplicates what the noun subcommands accept, so it can
// drift. Each subcommand registers its flags on a flag.FlagSet inside the
// function that runs it, leaving nothing for completion to read, so this test
// reads the registrations from the source instead.
//
// The check is one-directional: every flag name registered by a noun-group
// parser must appear somewhere in the table. It does not verify which
// subcommand a flag belongs to, because the parsers are shared -- nameAndYes
// serves both `secret delete` and `agent rm`. It catches the case that matters:
// a flag added to a subcommand and not added here, which nothing else would
// catch because a missing candidate is silent.
func TestGroupsTableCoversEveryGroupFlag(t *testing.T) {
	// Parsers serving positions the groups table does not describe. Listed by
	// name, and each is checked below to still register something, so an entry
	// cannot outlive what it excuses.
	//
	// The list is short because the scan below only sees a flag name written as
	// a string literal. The run line registers through a closure over the
	// spelling (`func(n string) { fs.StringVar(..., n, ...) }`), and doctor and
	// ls hand-parse with no FlagSet. That is a real limit: a group flag
	// registered through a variable would not be seen. All of them are literals
	// today.
	elsewhere := map[string]string{
		"parseGlobal": "the global position, left of the verb; see brigFlags",
	}
	seen := map[string]bool{}

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	table := groupFlagNames()

	for _, f := range pkg["main"].Files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				name, ok := registeredFlag(n)
				if !ok {
					return true
				}
				if _, excused := elsewhere[fn.Name.Name]; excused {
					seen[fn.Name.Name] = true
					return true
				}
				if !table[name] {
					t.Errorf("%s registers the flag %q, and the groups table does not offer it. "+
						"Add it to the subcommand's entry, or -- if it belongs to a position the "+
						"table does not describe -- name that parser in `elsewhere` with the reason",
						fn.Name.Name, spell(name))
				}
				return true
			})
		}
	}

	// A renamed parser, or one that no longer registers a flag, would leave a
	// stale entry excusing something that cannot occur.
	for fn, why := range elsewhere {
		if !seen[fn] {
			t.Errorf("`elsewhere` excuses %s (%s), but it registers no flag any more. Drop the line", fn, why)
		}
	}
}

// registeredFlag returns the flag name from an fs.BoolVar/StringVar/... call:
// the second argument, after the pointer.
func registeredFlag(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !strings.HasSuffix(sel.Sel.Name, "Var") {
		return "", false
	}
	lit, ok := call.Args[1].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return name, true
}

// groupFlagNames returns every flag in the groups table by bare name, so a
// registration matches whichever way it is spelled.
func groupFlagNames() map[string]bool {
	out := map[string]bool{}
	for _, subs := range groups {
		for _, s := range subs {
			for _, f := range s.flags {
				out[strings.TrimLeft(f, "-")] = true
			}
			for f := range s.values {
				out[strings.TrimLeft(f, "-")] = true
			}
		}
	}
	return out
}

// globalFlagSpellings is every spelling completion offers left of the verb:
// the long form of each global flag, and the short one where it has it.
func globalFlagSpellings() []string {
	var out []string
	for _, f := range brigFlags {
		if f.position != posGlobal {
			continue
		}
		out = append(out, "--"+f.long)
		if f.short != "" {
			out = append(out, "-"+f.short)
		}
	}
	sort.Strings(out)
	return out
}
