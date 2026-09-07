package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// completionHost gives the engine a host to answer about: two profiles of its
// own, two sessions in the index, and two policies. Every case below reads the
// same fixture, so what changes between them is only where the cursor is.
//
// The built-in profiles are there too -- Load adds to the registry rather than
// replacing it -- which is what the cases matching on a prefix rely on.
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

// Where the cursor is decides what may stand there. One table, because that is
// what the engine is: a line, a position, and the vocabulary legal in it.
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
		// The retired spellings still work and say what replaced them. None of
		// them is a word to teach a reader who has not typed it yet.
		absent: []string{"exec", "shell", "env", "create", "reset", "profiles", "policies", completeVerb},
	}, {
		name:      "left of the verb the flags are the global ones",
		words:     []string{"-"},
		directive: dirNames,
		exactly:   []string{"--quiet", "--verbose", "-q"},
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
		// --name and --workspace are retiring onto other spellings; --memory is
		// the older name of --mem and is in no help text; -q on the run line is
		// the position that is retiring, not the flag.
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
		// bash breaks a word on '=', so the inline spelling arrives split.
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
		name:      "right of the ref a flag is the agent's",
		words:     []string{"run", "claude", "--"},
		directive: dirNone,
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

// A directive that says "nothing here" never carries candidates, and one that
// says "candidates follow" never comes back empty. A shell reads the first line
// and stops; the two halves of an answer have to agree.
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

// The engine is asked on a keystroke, so it says nothing a reader did not ask
// for. A profile that will not parse is the case that would otherwise print:
// every other verb warns about it, and here it costs a candidate instead.
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

// The answer is a directive line and then the candidates, one per line. This is
// the whole of what the three scripts parse, so it is a test of its own.
func TestCompleteWireFormat(t *testing.T) {
	completionHost(t)

	var out bytes.Buffer
	completeCmd(&out, []string{"telemetry", ""})
	if got, want := out.String(), ":names\noff\non\nstatus\n"; got != want {
		t.Errorf("answer %q, want %q", got, want)
	}

	out.Reset()
	completeCmd(&out, []string{"run", "claude", "-"})
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

// Every shell brig names in its own usage has a script to print, and every
// script is registered for the command that runs it.
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
		// A script that knows a directive brig does not emit, or misses one it
		// does, is a script that will fall back to filenames where brig asked
		// for silence.
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
