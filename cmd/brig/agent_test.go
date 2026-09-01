package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every verb of the new group, through run() rather than through the functions
// underneath it, because the dispatch switch is half of what this change is:
// a verb that works when called directly and is not wired into `brig agent`
// is not a spelling anyone can type.
//
// The order is the order someone would type them -- new, then edit, then
// export, then rm -- so the group is exercised as a workflow rather than as
// seven unrelated calls.
func TestAgentVerbsWork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	stubEditor(t, `printf '\n# tuned by hand\n' >> "$1"`)

	for _, args := range [][]string{
		{"agent", "ls"},
		{"agent", "show", "claude-code"},
		{"agent", "show", "claude-code", "--json"},
		{"agent", "new", "mine", "--from", "claude-code"},
		{"agent", "edit", "mine"},
		{"agent", "export", "claude-code"},
		{"agent", "export", "codex", "spare"},
		{"agent", "rm", "spare", "-y"},
		{"agent", "rm", "mine", "-y"},
	} {
		if _, err := captureStdout(t, func() error { return run(args) }); err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
		}
	}

	// import takes a file, so it needs one written first. Round-tripping what
	// show printed is the documented flow and covers both verbs at once.
	out, err := captureStdout(t, func() error { return run([]string{"agent", "show", "codex"}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "codex.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(out, "name: codex", "name: round", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return run([]string{"agent", "import", path}) }); err != nil {
		t.Errorf("brig agent import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "round.yaml")); err != nil {
		t.Errorf("brig agent import wrote nothing: %v", err)
	}
}

// The recipe brig prints has to name the verbs brig has. A message that says
// "profile" sends the reader to a command that answers with a deprecation
// notice, which is one hop too many.
func TestAgentUnknownSubcommandNamesTheNewVerbs(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{{"agent"}, {"agent", "nonsense"}} {
		err := run(args)
		if err == nil {
			t.Fatalf("brig %s was accepted", strings.Join(args, " "))
		}
		for _, verb := range []string{"ls", "show", "new", "edit", "rm", "import", "export"} {
			if !strings.Contains(err.Error(), verb) {
				t.Errorf("brig %s does not name %s: %v", strings.Join(args, " "), verb, err)
			}
		}
		if strings.Contains(err.Error(), "profile") {
			t.Errorf("brig %s names the retired noun: %v", strings.Join(args, " "), err)
		}
	}
}

// show prints and writes nothing, so a second word is the old
// `brig export <p> <name>` habit rather than a destination. Saying which
// command took that job over is the whole value of the error.
func TestAgentShowRefusesADestinationAndNamesNew(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	err := run([]string{"agent", "show", "claude-code", "mine"})
	if err == nil {
		t.Fatal("brig agent show wrote a file")
	}
	if !strings.Contains(err.Error(), "brig agent new mine --from claude-code") {
		t.Errorf("the error does not name the command that copies one: %v", err)
	}
}

// new copies an agent, so it has to be told which one. Without --from there is
// nothing to copy and no sensible default: a blank starter file is a different
// feature.
func TestAgentNewNeedsFromAndAName(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{
		{"agent", "new", "mine"},
		{"agent", "new", "--from", "claude-code"},
		{"agent", "new", "mine", "spare", "--from", "claude-code"},
	} {
		if err := run(args); err == nil {
			t.Errorf("brig %s was accepted", strings.Join(args, " "))
		}
	}
}

// The help text is the only place a reader finds the vocabulary, so it has to
// carry the group that exists and not the one that is retiring. Asserted on
// the const rather than on run()'s output because that is where a future edit
// would put a stale line back.
func TestUsageTeachesTheAgentGroup(t *testing.T) {
	for _, want := range []string{"brig agent", "brig policy ls", "brig secret"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage text does not mention %q", want)
		}
	}
	// The retired spellings keep working and stay out of the help: a reader
	// who never saw them has no reason to learn one.
	for _, gone := range []string{"brig profiles", "brig profile ", "brig policies", "brig export "} {
		if strings.Contains(usage, gone) {
			t.Errorf("the usage text still teaches %q", gone)
		}
	}
	for _, want := range []string{
		"brig agent ls", "brig agent show", "brig agent new", "brig agent edit",
		"brig agent rm", "brig agent import", "brig agent export",
	} {
		if !strings.Contains(agentUsage, want) {
			t.Errorf("brig agent --help does not name %q", want)
		}
	}
}

// `brig agent --help` is a question, not a mistake, and so is the --help a
// verb's own parser sees. The profile group already answered both this way.
func TestAgentHelpPrintsUsageAndSucceeds(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{
		{"agent", "--help"}, {"agent", "-h"}, {"agent", "help"}, {"agent", "rm", "--help"},
	} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(out, "brig agent rm") {
			t.Errorf("brig %s printed no usage:\n%s", strings.Join(args, " "), out)
		}
	}
}

// secret keeps create|update|read|delete|ls|import, and that is a decision
// rather than an oversight this issue missed.
//
// The unified verb table would make it set|show|rm, and `set` is the problem:
// it means createOrUpdate, so a typo in the name writes a second secret
// instead of failing, and the run that needed the first one fails much later
// with a credential the sandbox never received. create and update each refuse
// the case the other is for, which is worth more than one consistent verb set
// across three nouns.
//
// This test exists so that the next reader who notices the inconsistency finds
// the reason before changing it. If the decision is revisited, revisit it here
// first.
func TestSecretKeepsItsOwnVerbSetOnPurpose(t *testing.T) {
	newFake(t)
	for _, verb := range []string{"create", "update", "read", "delete", "ls", "import"} {
		if !strings.Contains(secretUsage, "brig secret "+verb) {
			t.Errorf("brig secret --help no longer documents %q", verb)
		}
		// Documented and dispatched are two different things, and the verb set
		// is both: an unknown subcommand is the one error that would tell a
		// reader the exception had been undone.
		if err := secretCmd(&bytes.Buffer{}, []string{verb}); err != nil &&
			strings.Contains(err.Error(), "unknown secret subcommand") {
			t.Errorf("brig secret %s is no longer a subcommand: %v", verb, err)
		}
	}
	// And the verbs the unified table would have introduced are not there.
	for _, verb := range []string{"set", "show", "get"} {
		err := secretCmd(&bytes.Buffer{}, []string{verb, "gh-token"})
		if err == nil || !strings.Contains(err.Error(), "unknown secret subcommand") {
			t.Errorf("brig secret %s exists, so the exception was undone: %v", verb, err)
		}
	}
}
