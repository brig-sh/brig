package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchHost points brig's whole environment at directories of the test's own
// and takes the runtime away, so a lifecycle verb reaches the point where it
// would need one and stops there.
//
// Taking the runtime away is what makes these tests safe to run on a machine
// that has sandboxes on it: `brig rm --all` with a runtime on PATH removes
// every sandbox the person running the test owns. With none, detection fails
// before any of these verbs touches an instance, and what is left to observe is
// exactly what these tests are about -- whether brig accepted the line.
func scratchHost(t *testing.T) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	t.Setenv("BRIG_RUNTIME", "")
	emptyPath(t)
}

// took reports whether brig accepted the line it was given, as opposed to
// refusing the words in it. A verb that got as far as needing a runtime, or a
// credential, has accepted its operand: the only two errors that mean the
// operand itself was refused are the usage error and the not-found error, which
// is why they carry exit codes of their own.
func took(err error) bool {
	var ue *usageError
	var nf *notFoundError
	return !errors.As(err, &ue) && !errors.As(err, &nf)
}

// brig sh is one verb for what exec and shell were two of: with a command it
// runs that command, with none it opens a login shell. Both shapes have to
// reach the sandbox, so neither may be refused on the way in -- which is the
// half that can be asserted without a runtime. That the command lands as
// `bash -lc` and a bare sh as `bash -l` is argv, and script/smoke.sh asserts it
// against the stub.
func TestShAcceptsACommandAndNoCommand(t *testing.T) {
	for _, args := range [][]string{
		{"sh", "claude"},
		{"sh", "claude", "echo", "hi"},
		{"sh", "claude@refactor"},
		{"sh", "claude@refactor", "--", "git", "status"},
	} {
		scratchHost(t)
		_, err := captureStdout(t, func() error { return run(args) })
		if !took(err) {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}
	// And a tail is the guest's command rather than a stray token, which is the
	// decision rejectTail makes for every verb.
	if err := rejectTail("sh", []string{"git", "status"}); err != nil {
		t.Errorf("rejectTail(sh) refused a guest command: %v", err)
	}
}

// `brig rm --all` is what `brig reset` was: every brig sandbox, workspaces left
// alone. It is read before the run line, because --all names no session and the
// run-line parser would refuse it as a brig flag that does not exist.
//
// The refusals are the point of the test as much as the acceptance. A flag
// typed to make a destructive command safer must not be read past and ignored,
// and a ref beside --all is two different requests on one line.
func TestRemoveAllTakesNothingElse(t *testing.T) {
	scratchHost(t)
	if err := run([]string{"rm", "--all"}); !took(err) {
		t.Errorf("brig rm --all was refused: %v", err)
	}
	for _, args := range [][]string{
		{"rm", "--all", "claude"},
		{"rm", "claude", "--all"},
		{"rm", "--all", "--dry-run"},
	} {
		scratchHost(t)
		err := run(args)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("brig %s: %v, want a usageError", strings.Join(args, " "), err)
			continue
		}
		// Named, so the reader knows which word to take off the line.
		stray := args[len(args)-1]
		if stray == "--all" {
			stray = args[1]
		}
		if !strings.Contains(err.Error(), stray) {
			t.Errorf("brig %s: %v, want it to name %q", strings.Join(args, " "), err, stray)
		}
	}
}

// A bare ref with no verb is accepted, and a verb is still a verb. Both halves
// matter: the second is what keeps the surface from becoming data-dependent,
// because a ref is only tried once every verb has been ruled out. Without that
// ordering, installing an agent called `ls` would change what `brig ls` means.
func TestVerblessRefIsLastInTheChain(t *testing.T) {
	for _, args := range [][]string{{"claude"}, {"claude-code"}, {"claude@refactor"}} {
		scratchHost(t)
		_, err := captureStdout(t, func() error { return run(args) })
		if !took(err) {
			t.Errorf("brig %s was refused: %v", strings.Join(args, " "), err)
		}
	}

	// An agent whose name is one of brig's verbs. `brig ls` has to stay the
	// listing: with no runtime on PATH that reports an empty list and exits 0,
	// where running the agent could only have failed for want of one.
	scratchHost(t)
	dir := os.Getenv("BRIG_PROFILE_DIR")
	blob := "name: ls\nimage: i\nguestHome: /home/ls\nbinary: ls\nmem: 1\ncpus: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ls.yaml"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"ls"}) })
	if err != nil {
		t.Fatalf("brig ls with an agent called ls: %v", err)
	}
	if !strings.Contains(out, "SANDBOX") {
		t.Errorf("brig ls ran the agent called ls rather than listing:\n%s", out)
	}

	// And a token that is neither a verb nor an agent is an unknown command,
	// not a missing profile: the reader typed a word brig does not have, and
	// the message that names the vocabulary is the one that helps.
	scratchHost(t)
	err = run([]string{"nosuchthing"})
	if err == nil {
		t.Fatal("brig nosuchthing was accepted")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("brig nosuchthing: %v, want it to report an unknown command", err)
	}
}

// A ref-shaped token that brig cannot use is diagnosed as the ref it plainly
// is, and not as a command nobody typed.
//
// The separator is what settles it. '@' is brig's, reserved in
// internal/session for exactly one job, and no verb has one -- so a token
// carrying one has a single possible reading, and "unknown command" would be a
// wrong answer rather than a cautious one. The bare word above keeps the
// cautious answer, because `brig nosuchthing` really could be either.
//
// Asserted as equality with the verbed form rather than against fixed text.
// The two spellings ask the same question, so a change to how brig answers it
// has to reach both or the pair stops matching -- which no assertion on the
// wording of one of them would catch.
func TestARefShapedTokenIsDiagnosedAsARef(t *testing.T) {
	for _, ref := range []string{
		"claude@BAD",      // a label that is not slug-clean
		"claude@a@b",      // two separators
		"claude@",         // a label the typing stopped short of
		"@refactor",       // no agent
		"nosuch@refactor", // an agent brig does not have
	} {
		scratchHost(t)
		verbless := run([]string{ref})
		scratchHost(t)
		verbed := run([]string{"run", ref})

		if verbless == nil || verbed == nil {
			t.Errorf("brig %s was accepted", ref)
			continue
		}
		if verbless.Error() != verbed.Error() {
			t.Errorf("brig %s said\n  %v\nbut brig run %s said\n  %v", ref, verbless, ref, verbed)
		}
		// The message being equal is not enough: a bare ParseRef failure falls
		// through to exit 1 while the verbed form's usage error exits 2, so the
		// two read alike and a script still tells them apart. Pin the class too,
		// including nosuch@refactor, which already reaches exit 3 both ways.
		if exitCode(verbless) != exitCode(verbed) {
			t.Errorf("brig %s exits %d, brig run %s exits %d",
				ref, exitCode(verbless), ref, exitCode(verbed))
		}
		if strings.Contains(verbless.Error(), "unknown command") {
			t.Errorf("brig %s was reported as an unknown command: %v", ref, verbless)
		}
	}
}
