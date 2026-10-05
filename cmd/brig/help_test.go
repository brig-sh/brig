package main

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// The top-level verbs the issue lists. Before this change, each refused --help
// and -h with a usage error and printed nothing to stdout.
var topLevelVerbs = []string{"run", "sh", "info", "plan", "rm", "stop", "ls", "logs", "doctor", "version"}

// Asking a verb for help is a question, not a mistake: it is answered with that
// verb's usage, on stdout, and the exit status is 0.
func TestEveryTopLevelVerbAnswersHelp(t *testing.T) {
	for _, verb := range topLevelVerbs {
		for _, flag := range []string{"--help", "-h"} {
			out, err := captureStdout(t, func() error { return run([]string{verb, flag}) })
			if err != nil {
				t.Errorf("brig %s %s: %v", verb, flag, err)
				continue
			}
			if out == "" {
				t.Errorf("brig %s %s printed nothing", verb, flag)
			}
			if out == usage {
				t.Errorf("brig %s %s printed the global usage, not the verb's own:\n%s", verb, flag, out)
			}
			if !strings.Contains(out, "brig "+verb) {
				t.Errorf("brig %s %s does not name the verb:\n%s", verb, flag, out)
			}
		}
	}
}

// The two spellings of the same question answer with the same text, so a reader
// who learns one has learnt the other. The group verbs are in the table too: a
// verb group is a verb, and `brig help agent` answers with the group's own
// usage, the text `brig agent --help` prints, which is unchanged.
func TestHelpVerbPrintsTheTextTheFlagDoes(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	verbs := append([]string{"agent", "secret", "policy", "network", "telemetry", "completion"}, topLevelVerbs...)
	for _, verb := range verbs {
		byFlag, err := captureStdout(t, func() error { return run([]string{verb, "--help"}) })
		if err != nil {
			t.Fatalf("brig %s --help: %v", verb, err)
		}
		byVerb, err := captureStdout(t, func() error { return run([]string{"help", verb}) })
		if err != nil {
			t.Fatalf("brig help %s: %v", verb, err)
		}
		if byFlag != byVerb {
			t.Errorf("brig help %s and brig %s --help differ:\n--- brig help %s ---\n%s\n--- brig %s --help ---\n%s",
				verb, verb, verb, byVerb, verb, byFlag)
		}
	}
}

// A help flag is read wherever brig's own flags can stand before the ref, not
// only first: `brig run -d --help` asks the same question as `brig run --help`
// and is answered with run's usage and a zero exit. The value of a flag is not
// a help request, and neither is a --help after the ref -- both fall through
// to the agent.
func TestHelpFlagAmongBrigsFlagsAnswersHelp(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{
		{"run", "-d", "--help"},
		{"run", "--image", "x", "--help"},
		{"run", "-d", "-h"},
		{"run", "--image=x", "--help"},
		{"logs", "--tail", "100", "--help"},
	} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
			continue
		}
		if out == usage {
			t.Errorf("brig %s printed the global usage, not the verb's own:\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(out, "brig "+args[0]) {
			t.Errorf("brig %s did not print its usage:\n%s", strings.Join(args, " "), out)
		}
	}
}

// A bare help request is the command list, as it always was, and an unknown
// word keeps that answer rather than a refusal.
func TestBareHelpStillPrintsTheGlobalUsage(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{{"help"}, {"help", "nosuchverb"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("brig %v: %v", args, err)
			continue
		}
		if out != usage {
			t.Errorf("brig %v is no longer the global usage:\n%s", args, out)
		}
	}
}

// --help after the ref belongs to the agent: the ref is already brig's, so the
// next --help is the agent's word and split must hand it over untouched. This
// is the line the two spellings above must not break.
func TestParseKeepsHelpAfterProfileForTheAgent(t *testing.T) {
	_, name, tail, err := parse("run", []string{"claude", "--help"})
	if err != nil {
		t.Fatalf("parse run claude --help: %v", err)
	}
	if name != "claude" || strings.Join(tail, " ") != "--help" {
		t.Errorf("parse = (%q, %q), want (claude, --help)", name, tail)
	}
}

// And through the whole run: the fake agent is started with --help in its argv,
// rather than brig answering it as its own.
func TestRunForwardsHelpAfterTheRefToTheAgent(t *testing.T) {
	var argv []string
	rt := &jsonRuntime{attachFn: func(spec runtime.ExecSpec) (int, error) {
		argv = append([]string(nil), spec.Cmd...)
		return 0, nil
	}}
	jsonRunHost(t, rt)

	// Under --json a successful agent run comes back as an agentExit carrying
	// its status, so 0 is the success here the way main reads it.
	if _, err := captureStdout(t, func() error { return run([]string{"--json", "run", "faker", "--help"}) }); exitCode(err) != 0 {
		t.Fatalf("run faker --help: exit %d", exitCode(err))
	}
	if len(argv) == 0 || argv[len(argv)-1] != "--help" {
		t.Errorf("agent argv = %q, want --help forwarded", argv)
	}
}

// info previews the envelope a run line would boot, so the run-line flags that
// change that envelope change what info prints: `brig info --image X claude`
// names X. Its usage has to say so, or a reader who wants the preview for a
// different image or network has no way to learn the line takes them.
func TestInfoUsageNamesTheFlagsThatChangeTheEnvelope(t *testing.T) {
	for _, flag := range []string{"--image", "--home", "--network", "--offline", "--publish", "--json"} {
		if !strings.Contains(infoUsage, flag) {
			t.Errorf("brig info --help does not name %s, which changes what info prints:\n%s", flag, infoUsage)
		}
	}
}
