package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
)

// The listing prints the ref every other verb takes. That is the defect this
// issue was filed about: the column used to hold the sandbox's own name, which
// no verb accepts, so a reader who copied what they saw got an error.
func TestListingPrintsTheRef(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_WORKSPACE", t.TempDir())

	// Two sessions brig recorded, one of them the agent's default session, and
	// one sandbox that was never indexed.
	index := `{` +
		`"claude-code@refactor": {"home": "/ws/refactor", "sandbox": "brig-claude-code-refactor"},` +
		`"claude-code": {"home": "/ws/default", "sandbox": "brig-claude-code"}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := sandboxRows([]runtime.Instance{
		{Name: "brig-claude-code", State: "running"},
		{Name: "brig-claude-code-refactor", State: "stopped"},
		// Indexed by nothing and derivable from nothing: the name after the
		// prefix matches no agent brig has.
		{Name: "brig-mystery", State: "running"},
		// Not brig's, so not in the listing at all.
		{Name: "someone-elses-container", State: "running"},
	}, nil)

	want := map[string]string{
		"brig-claude-code":          "claude-code",
		"brig-claude-code-refactor": "claude-code@refactor",
		"brig-mystery":              "",
	}
	if len(rows) != len(want) {
		t.Fatalf("the listing has %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		if got, ok := want[r.name]; !ok {
			t.Errorf("the listing holds %q, which is not brig's", r.name)
		} else if r.ref != got {
			t.Errorf("%s is listed as ref %q, want %q", r.name, r.ref, got)
		}
	}

	// A sandbox with no ref to print says so with a placeholder rather than an
	// empty column, and never with a guess: the table is read by a person, and
	// a blank cell reads like a bug in brig rather than a fact about that
	// sandbox.
	var table bytes.Buffer
	printSandboxes(&table, rows)
	if !strings.Contains(table.String(), "REF") {
		t.Errorf("the listing has no REF column:\n%s", table.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(table.String()), "\n") {
		if !strings.HasPrefix(line, "brig-mystery") {
			continue
		}
		if !strings.Contains(line, noRef) {
			t.Errorf("a sandbox with no ref is listed as %q, want %q in it", line, noRef)
		}
	}
}

// `brig ls -q` prints refs and nothing else, which is the form a script reads.
// No header, no state, no workspace -- and nothing for a sandbox that has no
// ref, because every line of this output is meant to be a word another verb
// takes.
func TestListingQuietPrintsRefsOnly(t *testing.T) {
	var buf bytes.Buffer
	printRefs(&buf, []sandboxRow{
		{name: "brig-claude-code", ref: "claude-code", state: "running", workspace: "/ws"},
		{name: "brig-mystery", ref: "", state: "running", workspace: "/ws"},
	})
	if got := strings.Fields(buf.String()); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("ls -q printed %q, want the one ref on its own", buf.String())
	}
	// -q is a flag on ls, and the only one. Anything else is still refused.
	scratchHost(t)
	for _, args := range [][]string{{"-q"}, {"--quiet"}} {
		if _, err := captureStdout(t, func() error { return listSandboxes(args, false, false) }); err != nil {
			t.Errorf("brig ls %s: %v", args[0], err)
		}
	}
	if _, err := captureStdout(t, func() error { return listSandboxes([]string{"-x"}, false, false) }); err == nil {
		t.Error("brig ls -x was accepted")
	}
}

// The acceptance criterion for the whole change: every ref `brig ls -q` prints
// is a word every verb takes. If this can fail, a user who pipes the listing
// into a verb gets an error out of brig's own output, which is the fault the
// issue was filed about.
//
// Driven through run() rather than through the parser, so what is asserted is
// the command line as someone would type it -- the verb, the operand, and the
// dispatch between them.
func TestListingRefsRoundTripThroughEveryVerb(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	index := `{` +
		`"claude-code@refactor": {"home": "/ws/refactor", "sandbox": "brig-claude-code-refactor"},` +
		`"claude-code": {"home": "/ws/default", "sandbox": "brig-claude-code"}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := sandboxRows([]runtime.Instance{
		{Name: "brig-claude-code", State: "running"},
		{Name: "brig-claude-code-refactor", State: "stopped"},
		{Name: "brig-mystery", State: "running"},
	}, nil)
	var buf bytes.Buffer
	printRefs(&buf, rows)
	refs := strings.Fields(buf.String())
	if len(refs) < 2 {
		t.Fatalf("ls -q printed %d refs, want the sessions in the index: %q", len(refs), refs)
	}

	// The same promise holds for `ls --json`: every ref it prints is a ref, and
	// it prints exactly the refs -q does. If the two disagreed, a script reading
	// the JSON would get an operand no verb takes -- the fault this test is about,
	// one shape further out.
	jsonRefs := []string{}
	for _, s := range sandboxListData(rows, nil).Sandboxes {
		if s.Ref != "" {
			jsonRefs = append(jsonRefs, s.Ref)
		}
	}
	if strings.Join(jsonRefs, " ") != strings.Join(refs, " ") {
		t.Fatalf("ls --json prints refs %q, ls -q prints %q", jsonRefs, refs)
	}

	// Every verb that takes a session, in the spelling the help text teaches
	// and in the spellings it retires, plus the verbless form. exec is the one
	// that needs a command of its own.
	lines := func(ref string) [][]string {
		return [][]string{
			{"run", ref}, {"sh", ref}, {"stop", ref}, {"rm", ref}, {"info", ref},
			{"create", ref}, {"shell", ref}, {"env", ref}, {"exec", ref, "--", "true"},
			{ref},
		}
	}
	for _, ref := range refs {
		// A ref brig printed has to parse as one before anything else can be
		// true of it.
		if _, err := session.ParseRef(ref); err != nil {
			t.Errorf("ls -q printed %q, which is not a ref: %v", ref, err)
			continue
		}
		for _, args := range lines(ref) {
			scratchHost(t)
			_, err := captureStdout(t, func() error { return run(args) })
			if !took(err) {
				t.Errorf("brig %s refused a ref brig ls -q printed: %v",
					strings.Join(args, " "), err)
			}
		}
	}
}
