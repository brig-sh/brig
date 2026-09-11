package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// removeRuntime is a runtime holding a fixed list of sandboxes and recording
// which of them it was asked to remove. Everything else it embeds is nil and
// panics if reached: rm has no business booting or exec'ing anything.
type removeRuntime struct {
	runtime.Runtime
	list    []runtime.Instance
	removed []string
}

func (r *removeRuntime) Kind() string                      { return "hull" }
func (r *removeRuntime) Bin() string                       { return "hull" }
func (r *removeRuntime) List() ([]runtime.Instance, error) { return r.list, nil }
func (r *removeRuntime) Stop(string) error                 { return nil }
func (r *removeRuntime) Remove(name string) error {
	r.removed = append(r.removed, name)
	return nil
}

// removeHost is jsonRunHost with the same fake wired into both detection
// seams: `rm --all` reads the instance list off the read-verb detector, and
// `rm <ref>` goes through the run line's.
func removeHost(t *testing.T, rt *removeRuntime) {
	t.Helper()
	jsonRunHost(t, rt)
	withRuntime(t, rt)
}

func twoSandboxes() *removeRuntime {
	return &removeRuntime{list: []runtime.Instance{
		{Name: "brig-faker", State: "running"},
		{Name: "brig-faker-refactor", State: "stopped"},
	}}
}

// `brig rm --all` removes every sandbox there is, so with nobody to ask it
// refuses and names the flag that answers in advance, rather than assuming yes
// and making the scripted case the one that cannot be stopped. The test binary
// has no terminal on stdin, which is exactly that case.
func TestRemoveAllRefusesWithoutTerminalOrYes(t *testing.T) {
	for _, args := range [][]string{{"rm", "--all"}, {"reset"}} {
		rt := twoSandboxes()
		removeHost(t, rt)
		_, err := captureStdout(t, func() error { return run(args) })
		if err == nil {
			t.Fatalf("brig %s removed without asking", strings.Join(args, " "))
		}
		if !strings.Contains(err.Error(), "-y") {
			t.Errorf("brig %s: %v, want it to name -y", strings.Join(args, " "), err)
		}
		if len(rt.removed) != 0 {
			t.Errorf("brig %s removed %v after refusing", strings.Join(args, " "), rt.removed)
		}
	}
}

// -y is the answer given in advance. Both spellings, on either side of --all.
func TestRemoveAllWithYesRemovesEverySandbox(t *testing.T) {
	for _, args := range [][]string{
		{"rm", "--all", "-y"},
		{"rm", "-y", "--all"},
		{"rm", "--all", "--yes"},
		{"reset", "-y"},
	} {
		rt := twoSandboxes()
		removeHost(t, rt)
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
			continue
		}
		if len(rt.removed) != 2 {
			t.Errorf("brig %s removed %v, want both", strings.Join(args, " "), rt.removed)
		}
		for _, name := range []string{"brig-faker", "brig-faker-refactor"} {
			if !strings.Contains(out, name) {
				t.Errorf("brig %s did not report removing %s:\n%s", strings.Join(args, " "), name, out)
			}
		}
	}
}

// --dry-run prints what --all would remove, as refs, and removes nothing. It
// exits 0: the list is the answer, not a refusal.
func TestRemoveAllDryRunListsAndRemovesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"rm", "--all", "--dry-run"},
		{"rm", "--dry-run", "--all"},
		{"rm", "--all", "--dry-run", "-y"},
		{"reset", "--dry-run"},
	} {
		rt := twoSandboxes()
		removeHost(t, rt)
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
			continue
		}
		if len(rt.removed) != 0 {
			t.Errorf("brig %s removed %v under --dry-run", strings.Join(args, " "), rt.removed)
		}
		for _, ref := range []string{"faker", "faker@refactor"} {
			if !strings.Contains(out, ref) {
				t.Errorf("brig %s did not list %s:\n%s", strings.Join(args, " "), ref, out)
			}
		}
		if !strings.Contains(out, "running") || !strings.Contains(out, "stopped") {
			t.Errorf("brig %s did not say the state of each:\n%s", strings.Join(args, " "), out)
		}
	}
}

// A sandbox whose ref brig cannot derive -- one named through BRIG_NAME, say --
// is still removed by --all, so the list names it by its sandbox name rather
// than leaving it out.
func TestRemoveAllDryRunNamesAnUnreadableSandbox(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-custom", State: "running"}}}
	removeHost(t, rt)
	out, err := captureStdout(t, func() error { return run([]string{"rm", "--all", "--dry-run"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "brig-custom") {
		t.Errorf("the unreadable sandbox is not listed:\n%s", out)
	}
}

// Nothing to remove is nothing to ask about.
func TestRemoveAllAsksNothingWhenThereIsNothing(t *testing.T) {
	rt := &removeRuntime{}
	removeHost(t, rt)
	if _, err := run2(t, []string{"rm", "--all"}); err != nil {
		t.Errorf("brig rm --all with no sandboxes: %v", err)
	}
}

// A sandbox that is not brig's is neither listed nor removed.
func TestRemoveAllLeavesOtherSandboxesAlone(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{
		{Name: "brig-faker", State: "running"},
		{Name: "somebody-else", State: "running"},
	}}
	removeHost(t, rt)
	out, err := run2(t, []string{"rm", "--all", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "somebody-else") {
		t.Errorf("a sandbox that is not brig's is listed:\n%s", out)
	}
	if _, err := run2(t, []string{"rm", "--all", "-y"}); err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != "brig-faker" {
		t.Errorf("removed %v, want only brig-faker", rt.removed)
	}
}

// `brig rm <ref> --dry-run` names the one sandbox it would remove and its
// workspace, and removes nothing.
func TestRemoveRefDryRun(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	out, err := run2(t, []string{"rm", "faker@refactor", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 0 {
		t.Errorf("removed %v under --dry-run", rt.removed)
	}
	if !strings.Contains(out, "faker@refactor") {
		t.Errorf("the ref is not named:\n%s", out)
	}
	if !strings.Contains(out, os.Getenv("BRIG_WORKSPACE")) {
		t.Errorf("the workspace is not named:\n%s", out)
	}
}

// A ref with no sandbox is still a not-found under --dry-run: there is nothing
// the command would do, and saying "would remove" about it would be a lie.
func TestRemoveRefDryRunIsStillNotFound(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	_, err := run2(t, []string{"rm", "faker@nope", "--dry-run"})
	var nf *notFoundError
	if !errors.As(err, &nf) {
		t.Errorf("brig rm faker@nope --dry-run: %v, want a notFoundError", err)
	}
}

// `brig rm <ref>` asks no question, so -y is a flag that would do nothing.
// Refused by name rather than accepted silently.
func TestRemoveRefRefusesYes(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	for _, args := range [][]string{{"rm", "faker", "-y"}, {"rm", "-y", "faker"}, {"rm", "--yes", "faker"}} {
		_, err := run2(t, args)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("brig %s: %v, want a usageError", strings.Join(args, " "), err)
			continue
		}
		if !strings.Contains(err.Error(), args[1]) && !strings.Contains(err.Error(), args[2]) {
			t.Errorf("brig %s: %v, want it to name the flag", strings.Join(args, " "), err)
		}
	}
	if len(rt.removed) != 0 {
		t.Errorf("removed %v after refusing", rt.removed)
	}
}

// After removing one sandbox, rm says the workspace is still there, and where.
func TestRemoveRefSaysTheWorkspaceStays(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	var err error
	stderr := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"rm", "faker"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != "brig-faker" {
		t.Errorf("removed %v, want brig-faker", rt.removed)
	}
	if !strings.Contains(stderr, "stays on the host") || !strings.Contains(stderr, os.Getenv("BRIG_WORKSPACE")) {
		t.Errorf("rm did not say where the workspace is:\n%s", stderr)
	}
}

// At a terminal, `rm --all` lists what it is about to remove and asks, and a
// typed yes goes through. The prompt is on stderr, so a removal inside a
// pipeline still asks where a person can see it.
func TestRemoveAllAtATerminalListsAndAsks(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	stderr := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"rm", "--all"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 2 {
		t.Errorf("removed %v after a yes, want both", rt.removed)
	}
	for _, want := range []string{"faker@refactor", "stopped", "Workspaces stay on the host", "[y/N]"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the prompt does not say %q:\n%s", want, stderr)
		}
	}
}

// The question defaults to no. A terminal whose other end has hung up answers
// nothing, and nothing is what gets removed.
func TestRemoveAllAtATerminalDefaultsToNo(t *testing.T) {
	rt := twoSandboxes()
	removeHost(t, rt)
	_ = terminalStdin(t).Close()
	var err error
	captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"rm", "--all"}) })
	})
	if err == nil {
		t.Fatal("an unanswered rm --all went through")
	}
	if len(rt.removed) != 0 {
		t.Errorf("an unanswered rm --all removed %v", rt.removed)
	}
}

// run2 is run with stdout captured, for a test that reads it or only wants it
// out of the test's own output.
func run2(t *testing.T, args []string) (string, error) {
	t.Helper()
	return captureStdout(t, func() error { return run(args) })
}
