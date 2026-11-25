package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --isolation microvm is a run-line flag with one value, and it reaches the
// configuration as the requirement wrap refuses a weaker boundary for.
func TestParseIsolationMicroVM(t *testing.T) {
	for _, args := range [][]string{
		{"--isolation", "microvm", "claude"},
		{"--isolation=microvm", "claude"},
		{"claude", "--isolation", "microvm"},
	} {
		o, profile, _, err := parse("run", args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if profile != "claude" {
			t.Errorf("%v: profile = %q, want claude", args, profile)
		}
		if o.load.Isolation != "microvm" {
			t.Errorf("%v: isolation = %q, want microvm", args, o.load.Isolation)
		}
	}
}

// A run that does not name the flag requires nothing, which is what every run
// did before the flag existed.
func TestParseWithoutIsolationRequiresNothing(t *testing.T) {
	o, _, _, err := parse("run", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Isolation != "" {
		t.Errorf("isolation = %q, want none", o.load.Isolation)
	}
}

// Every other value is a usage error that names the flag and the value, and
// none of them is read as no requirement: a typo that dropped the requirement
// would boot the sandbox it was written to stop. An empty value is one of
// them, since --isolation= and --isolation "" look like a flag that was set.
func TestParseRefusesAnyOtherIsolation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--isolation", "container", "claude"}, `"container"`},
		{[]string{"--isolation", "auto", "claude"}, `"auto"`},
		{[]string{"--isolation", "microVM", "claude"}, `"microVM"`},
		{[]string{"--isolation", "micro-vm", "claude"}, `"micro-vm"`},
		{[]string{"--isolation=", "claude"}, `""`},
		{[]string{"--isolation", "", "claude"}, `""`},
	} {
		_, _, _, err := parse("run", tc.args)
		if err == nil {
			t.Errorf("%v was accepted", tc.args)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("%v: %T, want a usage error, which exits 2", tc.args, err)
		}
		for _, want := range []string{"--isolation", tc.want, "microvm"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: the error does not say %s: %v", tc.args, want, err)
			}
		}
	}
}

// container is the value the issue names for opting in to less. Nothing can be
// allowed by it today, because a shim that shares the host kernel is refused
// without being asked, so the refusal says that and not only that it is
// unknown. The day something can use it is the day it is added.
func TestParseSaysWhyContainerIsNotOffered(t *testing.T) {
	_, _, _, err := parse("run", []string{"--isolation", "container", "claude"})
	if err == nil || !strings.Contains(err.Error(), "shares the host kernel") {
		t.Errorf("the refusal of container does not say why: %v", err)
	}
}

// A flag with no value is the flag package's own refusal, named by flag.
func TestParseIsolationNeedsAValue(t *testing.T) {
	if _, _, _, err := parse("run", []string{"claude", "--isolation"}); err == nil {
		t.Error("--isolation with no value was accepted")
	}
}

// The agent's own arguments are not brig's: after -- the flag is the agent's.
func TestParseLeavesIsolationAfterTheEndToTheAgent(t *testing.T) {
	o, _, tail, err := parse("run", []string{"claude", "--", "--isolation", "container"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Isolation != "" {
		t.Errorf("brig read the agent's --isolation: %q", o.load.Isolation)
	}
	if strings.Join(tail, " ") != "--isolation container" {
		t.Errorf("tail = %v", tail)
	}
}

// brig reads its own flags up to the first word that is the agent's, and a flag
// past that point goes to the agent with a notice. For --network that is a
// notice about a posture the line did not get. For a requirement it is a run
// that boots without the thing it required, under a notice that -q drops, so
// the flag is refused there instead of forwarded: a requirement that fails open
// is not one.
func TestParseRefusesIsolationInTheAgentsTail(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "-p", "hi", "--isolation", "microvm"},
		{"claude", "-p", "hi", "--isolation=microvm"},
		{"claude", "--resume", "--isolation", "microvm"},
		{"claude", "--resume", "--isolation=container"},
	} {
		_, _, _, err := parse("run", args)
		if err == nil {
			t.Errorf("%v: the requirement was handed to the agent", args)
			continue
		}
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("%v: %T, want a usage error", args, err)
		}
		for _, want := range []string{"--isolation", "before the profile"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: the error does not say %q: %v", args, want, err)
			}
		}
	}
}

// -- is the line saying the rest is the agent's, and brig takes it at its word.
// A guest command on sh is the guest's from its first word on, as for every
// other brig flag, so an --isolation there is the command's own.
func TestParseLeavesAnExplicitTailAndAShellCommandAlone(t *testing.T) {
	if _, _, tail, err := parse("run", []string{"claude", "--", "-p", "--isolation", "microvm"}); err != nil ||
		strings.Join(tail, " ") != "-p --isolation microvm" {
		t.Errorf("after --: tail %v, err %v", tail, err)
	}
	if o, _, tail, err := parse("sh", []string{"--isolation", "microvm", "claude", "mycmd", "--isolation", "x"}); err != nil ||
		o.load.Isolation != "microvm" || strings.Join(tail, " ") != "mycmd --isolation x" {
		t.Errorf("a shell command's own flag: isolation %q, tail %v, err %v", o.load.Isolation, tail, err)
	}
}

// Every usage that teaches the run line teaches the flag, with the one value it
// takes, so a reader who wants the requirement does not have to find it in the
// docs. The pages are the ones that already list --network.
func TestUsagesTeachIsolation(t *testing.T) {
	for name, text := range map[string]string{
		"brig help": usage, "brig run": runUsage, "brig sh": shUsage,
		"brig info": infoUsage, "brig plan": planUsage,
	} {
		for _, want := range []string{"--isolation", "microvm"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q:\n%s", name, want, text)
			}
		}
	}
}

// The value completes, in the spaced and the inline spelling, and the flag is
// offered where --network is.
func TestCompleteIsolation(t *testing.T) {
	completionHost(t)

	for _, words := range [][]string{
		{"run", "claude", "--isolation", ""},
		{"run", "--isolation", "="},
		{"run", "--isolation", "=", "mi"},
	} {
		directive, got := complete(words)
		if directive != dirNames || !equal(got, []string{"microvm"}) {
			t.Errorf("brig %s: %q %v, want microvm", strings.Join(words, " "), directive, got)
		}
	}
	if _, got := complete([]string{"run", "--is"}); !has(got, "--isolation") {
		t.Errorf("run --is does not offer --isolation: %v", got)
	}
}

// The tail that is refused is the one brig itself ended, and no more. A -- in
// it is the typist saying the rest is the agent's, as it is when it comes
// first, so a flag after it is not read; and on exec, as on sh, the tail is the
// guest's command from its first word. Both lines forwarded before the flag
// existed, so refusing them is a regression, and the advice to put the flag
// before the profile is wrong for a line that never asked for the requirement.
func TestParseDoesNotReadPastDashDashOrIntoAnExecCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		verb string
		args []string
		tail string
	}{
		"a -- after an unknown flag":   {"run", []string{"claude", "--resume", "--", "--isolation", "microvm"}, "--resume -- --isolation microvm"},
		"a -- after the agent's words": {"run", []string{"claude", "-p", "hi", "--", "--isolation=microvm"}, "-p hi -- --isolation=microvm"},
		"a guest command on exec":      {"exec", []string{"claude", "mytool", "--isolation", "strict"}, "mytool --isolation strict"},
		"a guest command after --":     {"exec", []string{"claude", "mytool", "--", "--isolation", "strict"}, "mytool -- --isolation strict"},
	} {
		t.Run(name, func(t *testing.T) {
			o, _, tail, err := parse(tc.verb, tc.args)
			if err != nil {
				t.Fatalf("parse = %v, want the line forwarded as it was", err)
			}
			if o.load.Isolation != "" {
				t.Errorf("brig read the agent's --isolation: %q", o.load.Isolation)
			}
			if got := strings.Join(tail, " "); got != tc.tail {
				t.Errorf("tail = %q, want %q", got, tc.tail)
			}
		})
	}

	// What stays refused: a flag that stands before the --, in the part brig
	// ended itself, on a verb whose tail is the agent's.
	for _, args := range [][]string{
		{"claude", "--resume", "--isolation", "microvm", "--", "-p"},
		{"claude", "-p", "hi", "--isolation=microvm", "--", "-p"},
	} {
		if _, _, _, err := parse("run", args); err == nil {
			t.Errorf("run %v: the requirement was handed to the agent", args)
		}
	}
}

// brig info boots nothing, and a script uses it as the check that comes before a
// run. With the requirement it must refuse on the ground run does, with run's
// code and before it prints anything, in text and in JSON: exit 0 on a host
// where run exits 7 is a go-ahead the host did not give. Without the flag it
// reports as it always did.
func TestInfoRefusesWhatARunWouldRefuseForItsIsolation(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "nerdctl")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "nerdctl")
	t.Setenv("BRIG_RUNTIME_BIN", stub)
	t.Setenv("BRIG_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("BRIG_STATE_DIR", filepath.Join(dir, "state"))

	for _, tc := range []struct {
		name string
		shim string
		args []string
		code int // 0 is a report
	}{
		{"a shim brig cannot place", "io.containerd.kata.v2", []string{"info", "--isolation", "microvm", "claude-code"}, 7},
		{"a shim that shares the host kernel", "runc", []string{"info", "--isolation", "microvm", "claude-code"}, 7},
		{"the same in JSON", "io.containerd.kata.v2", []string{"--json", "info", "--isolation", "microvm", "claude-code"}, 7},
		{"a microVM", "io.containerd.urunc.v2", []string{"info", "--isolation", "microvm", "claude-code"}, 0},
		{"no requirement on a shim brig cannot place", "io.containerd.kata.v2", []string{"info", "claude-code"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIG_CONTAINERD_RUNTIME", tc.shim)
			out, err := captureStdout(t, func() error { return run(tc.args) })
			if got := exitCode(err); got != tc.code {
				t.Fatalf("brig %s: exit %d, want %d: %v", strings.Join(tc.args, " "), got, tc.code, err)
			}
			if tc.code == 7 {
				if out != "" {
					t.Errorf("a refused info printed a report:\n%s", out)
				}
				if !strings.Contains(err.Error(), "--isolation microvm: this run would stand on") {
					t.Errorf("the refusal is not run's: %v", err)
				}
				return
			}
			if !strings.Contains(out, "ISOLATION") {
				t.Errorf("info printed no ISOLATION row:\n%s", out)
			}
		})
	}
}
