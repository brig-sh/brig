package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// The registry is built at run time now rather than being a package-level
// literal, so a test that looks a profile up has to load the built-ins the way
// main does. No test here writes to it, so once for the package is enough.
func TestMain(m *testing.M) {
	if err := profile.Load(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// The parse cases the bash wrapper's own suite ran, plus the ones the verb
// form and the sandbox flags add. brig's flags are read off the front in any
// order and everything from the first unrecognised argument onwards belongs
// to the agent.
func TestParse(t *testing.T) {
	cases := []struct {
		args     []string
		name     string
		given    bool
		template string
		tail     string
	}{
		{[]string{"claude"}, "", false, "claude", ""},
		{[]string{"claude", "--name", "foo"}, "foo", true, "claude", ""},
		{[]string{"claude", "-n", "foo"}, "foo", true, "claude", ""},
		{[]string{"claude", "--name=foo"}, "foo", true, "claude", ""},
		{[]string{"--name", "foo", "claude"}, "foo", true, "claude", ""},
		// Everything from the first agent argument onwards passes through.
		{[]string{"claude", "-p", "hi"}, "", false, "claude", "-p hi"},
		{[]string{"claude", "-n", "foo", "-p", "hi"}, "foo", true, "claude", "-p hi"},
		// A -- ends brig's own parsing, so an agent argument spelled like one
		// of brig's flags still reaches the agent.
		{[]string{"claude", "--", "--name", "notasession"}, "", false, "claude", "--name notasession"},
		{[]string{}, "", false, "", ""},
		// The second bare word is the project now, and brig still stops
		// reading its own line there, so what follows is the agent's. See
		// TestRunTakesTheProjectAsAPositional for the whole grammar.
		{[]string{"claude", "echo", "hi", "there"}, "", false, "claude", "hi there"},
	}
	for _, c := range cases {
		o, template, tail, err := parse(c.args)
		if err != nil {
			t.Errorf("parse(%q) failed: %v", c.args, err)
			continue
		}
		if o.load.Name != c.name || o.nameGiven != c.given || template != c.template ||
			strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = (%q, %v, %q, %q), want (%q, %v, %q, %q)",
				c.args, o.load.Name, o.nameGiven, template, strings.Join(tail, " "),
				c.name, c.given, c.template, c.tail)
		}
	}
}

func TestParseSandboxFlags(t *testing.T) {
	o, template, tail, err := parse([]string{
		"claude", "-t", "ghcr.io/me/img:latest", "-w", "/tmp/ws",
		"-m", "8192", "--cpus", "2", "-d", "-p", "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if template != "claude" {
		t.Errorf("template = %q", template)
	}
	if o.load.Image != "ghcr.io/me/img:latest" || o.load.Workspace != "/tmp/ws" {
		t.Errorf("image/workspace = %q, %q", o.load.Image, o.load.Workspace)
	}
	if o.load.Mem != 8192 || o.load.CPUs != 2 {
		t.Errorf("mem/cpus = %d, %d", o.load.Mem, o.load.CPUs)
	}
	if !o.detach {
		t.Error("-d did not set detach")
	}
	// The agent's own arguments still pass through untouched.
	if strings.Join(tail, " ") != "-p hi" {
		t.Errorf("tail = %q", tail)
	}
	// The inline form works for every value flag.
	o, _, _, err = parse([]string{"claude", "--image=x", "--workspace=/w", "--memory=1024", "--cpus=1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Image != "x" || o.load.Workspace != "/w" || o.load.Mem != 1024 || o.load.CPUs != 1 {
		t.Errorf("inline form = %+v", o.load)
	}
}

// --quiet is brig's own flag, read off the run line like the other bare flags
// and never handed to the agent. It suppresses the execution envelope.
func TestParseReadsQuiet(t *testing.T) {
	for _, args := range [][]string{{"claude", "--quiet"}, {"claude", "-q"}} {
		o, name, tail, err := parse(args)
		if err != nil {
			t.Fatalf("parse(%q): %v", args, err)
		}
		if !o.quiet || name != "claude" || len(tail) != 0 {
			t.Errorf("parse(%q) = quiet %v, name %q, tail %q", args, o.quiet, name, tail)
		}
	}
	// Absent by default, and after the agent's arguments begin it is the
	// agent's word rather than brig's.
	if o, _, _, _ := parse([]string{"claude"}); o.quiet {
		t.Error("quiet defaulted to on")
	}
	if o, _, _, _ := parse([]string{"claude", "-p", "hi", "--quiet"}); o.quiet {
		t.Error("--quiet was read out of the agent's own arguments")
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	cases := [][]string{
		{"claude", "--name"},     // nothing after it
		{"claude", "--name", ""}, // empty reads like no flag at all
		// Still refused with the agent's own arguments after it, where an
		// accepted empty name would quietly run the unnamed sandbox.
		{"claude", "--name", "", "-p", "hi"},
		{"claude", "--name", "", "--", "hi"},
		{"claude", "-m", "lots"},  // not a number
		{"claude", "--cpus", "0"}, // not a useful one
		{"claude", "-m", "-4"},    //
		{"claude", "--image"},     // value flag with nothing after it
	}
	for _, args := range cases {
		if _, _, _, err := parse(args); err == nil {
			t.Errorf("parse(%q) was accepted", args)
		}
	}
	// The inline form is the same flag, so it is refused the same way. These
	// reach the number check by a different route than the spaced form.
	for _, args := range [][]string{
		{"claude", "--memory=0"}, {"claude", "--cpus=-1"}, {"claude", "--memory=lots"},
	} {
		if _, _, _, err := parse(args); err == nil {
			t.Errorf("parse(%q) was accepted", args)
		}
	}
}

// The error names the flag the way the usage text does. The flag package
// prints one dash whichever spelling was typed, which turns --memory into
// -memory in the message for it.
func TestParseErrorsNameTheFlagAsTyped(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"claude", "--memory", "lots"}, "--memory"},
		{[]string{"claude", "-m", "lots"}, "-m"},
		{[]string{"claude", "--image"}, "--image"},
		{[]string{"claude", "-t"}, "-t"},
		// With both spellings on the line, the one carrying the bad value is
		// the token to go and fix, whichever of the two it happens to be.
		{[]string{"claude", "--memory=8", "-m", "0"}, "-m needs"},
		{[]string{"claude", "-m", "8", "--memory", "0"}, "--memory needs"},
	}
	for _, c := range cases {
		_, _, _, err := parse(c.args)
		if err == nil {
			t.Errorf("parse(%q) was accepted", c.args)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("parse(%q): %v, want it to name %s", c.args, err, c.want)
		}
	}
}

// The error quotes the argument as it was written. A parsed int cannot carry
// it -- -0, 0x0 and 0 all arrive as 0 -- and quoting that instead hands the
// reader a number they never typed to look for on their own line.
func TestParseErrorsQuoteTheValueAsTyped(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"claude", "--memory", "-0"}, `not "-0"`},
		{[]string{"claude", "--memory", "0x0"}, `not "0x0"`},
		{[]string{"claude", "--cpus=1_0"}, `not "1_0"`},
		{[]string{"claude", "-m", "lots"}, `not "lots"`},
		// The value is quoted into the flag package's message ahead of the
		// separator brig splits on, so one containing that text has to survive.
		{[]string{"claude", "--memory", "4 for flag x"}, `not "4 for flag x"`},
		{[]string{"claude", "--detach=1 for x"}, `not "1 for x"`},
	} {
		_, _, _, err := parse(c.args)
		if err == nil {
			t.Errorf("parse(%q) was accepted", c.args)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("parse(%q): %v, want it to quote %s", c.args, err, c.want)
		}
	}
}

// Numbers are decimal. Read base-0 instead, --memory 010 boots a guest a fifth
// of the size asked for and says nothing about it.
func TestParseReadsNumbersAsDecimal(t *testing.T) {
	o, _, _, err := parse([]string{"claude", "--memory", "010", "--cpus", "08"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Mem != 10 || o.load.CPUs != 8 {
		t.Errorf("mem/cpus = %d, %d, want 10, 8", o.load.Mem, o.load.CPUs)
	}
}

// An unrecognised flag is the agent's, not a mistake. This is the whole reason
// the flag package cannot be handed the line directly -- it would reject one.
func TestParseHandsUnknownFlagsToTheAgent(t *testing.T) {
	cases := []struct {
		args []string
		tail string
	}{
		{[]string{"claude", "--resume"}, "--resume"},
		{[]string{"claude", "--dangerously-skip-permissions"}, "--dangerously-skip-permissions"},
		// One of brig's own flags after an agent argument stays the agent's:
		// brig's parsing has already ended by then.
		{[]string{"claude", "-p", "hi", "--name", "foo"}, "-p hi --name foo"},
		// And brig still collects its own up to that point.
		{[]string{"claude", "-d", "--resume", "-n", "foo"}, "--resume -n foo"},
	}
	for _, c := range cases {
		o, name, tail, err := parse(c.args)
		if err != nil {
			t.Errorf("parse(%q): %v", c.args, err)
			continue
		}
		if name != "claude" || strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = (%q, %q), want (claude, %q)", c.args, name, tail, c.tail)
		}
		if o.nameGiven {
			t.Errorf("parse(%q) took the agent's --name as a session name", c.args)
		}
	}
}

// Only the two documented spellings are brig's. The flag package would answer
// to -name and --n as well, and taking them would silently eat an agent's own
// flag of the same name.
func TestParseOnlyTakesTheDocumentedSpellings(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--n", "foo"},
		{"claude", "-name", "foo"},
		{"claude", "-image", "x"},
		{"claude", "--d"},
		{"claude", "-skills"},
		{"claude", "-cpus", "2"},
	} {
		o, name, tail, err := parse(args)
		if err != nil {
			t.Errorf("parse(%q): %v", args, err)
			continue
		}
		if name != "claude" || strings.Join(tail, " ") != strings.Join(args[1:], " ") {
			t.Errorf("parse(%q) = (%q, %q), want it all handed to the agent", args, name, tail)
		}
		if o.nameGiven || o.load.Image != "" || o.load.CPUs != 0 || o.detach || o.load.Skills {
			t.Errorf("parse(%q) bound %+v as brig's own", args, o.load)
		}
	}
}

// A boolean flag written with a value means that value. Reading --skills=false
// as on would turn the guest's access to the host's skills back on.
func TestParseBoolsTakeAnInlineValue(t *testing.T) {
	for _, args := range [][]string{{"claude", "--detach=false"}, {"claude", "--skills=false"}} {
		o, _, _, err := parse(args)
		if err != nil {
			t.Errorf("parse(%q): %v", args, err)
			continue
		}
		if o.detach || o.load.Skills {
			t.Errorf("parse(%q) read =false as on", args)
		}
	}
	// And a value that is neither is a mistake worth saying out loud.
	if _, _, _, err := parse([]string{"claude", "--detach=garbage"}); err == nil {
		t.Error("--detach=garbage was accepted")
	} else if !strings.Contains(err.Error(), "--detach") || strings.Contains(err.Error(), "parse error") {
		t.Errorf("--detach=garbage: %v, want brig's own wording naming --detach", err)
	}
}

// A value is taken as given. `--name -p` is a session called -p, and guessing
// otherwise would make what a flag means depend on what its value looks like.
//
// Which leaves "hi" as the second bare word on the line, and that is the
// project: the flag swallowed -p, so nothing has ended brig's parsing before
// it.
func TestParseTakesAValueThatLooksLikeAFlag(t *testing.T) {
	o, name, tail, err := parse([]string{"claude", "--name", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Name != "-p" || !o.nameGiven {
		t.Errorf("name = %q, given = %v", o.load.Name, o.nameGiven)
	}
	if name != "claude" || len(tail) != 0 {
		t.Errorf("profile = %q, tail = %q", name, tail)
	}
	if o.load.Project != "hi" {
		t.Errorf("project = %q, want hi", o.load.Project)
	}
}

// --skills hands the guest the user's real skills and plugins, so it is opt-in
// and must never arrive from anywhere but the flag itself.
func TestParseSkillsIsOptIn(t *testing.T) {
	o, _, _, err := parse([]string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Skills {
		t.Error("skills defaulted to on")
	}
	if o, _, _, _ = parse([]string{"claude", "--skills"}); !o.load.Skills {
		t.Error("--skills did not set it")
	}
	// After the agent's arguments start it is the agent's word, not brig's.
	if o, _, _, _ = parse([]string{"claude", "-p", "hi", "--skills"}); o.load.Skills {
		t.Error("--skills was read out of the agent's own arguments")
	}
}

// `brig ls` reports what a sandbox is mounting, so the recorded workspace beats
// the one derived from the sandbox name -- and beats an ambient BRIG_WORKSPACE
// that the derivation would otherwise pick up. A session created with -w has a
// directory neither of those can name, and the column used to show the derived
// one with no sign that it was wrong.
func TestWorkspaceOfPrefersTheRecordedWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_WORKSPACE", "/somewhere/else")

	// Keyed by the ref, and the sandbox is in the value: the listing has a
	// sandbox name in hand and no ref, so this is the direction it reads.
	index := filepath.Join(dir, "sessions.json")
	body := `{"claude-code@rc23test":` +
		`{"home": "/private/tmp/ws-rc23", "sandbox": "brig-claude-code-rc23test"}}`
	if err := os.WriteFile(index, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The runtime is never reached: the recorded path answers the question
	// before anything has to resolve a profile.
	if got := workspaceOf("brig-claude-code-rc23test", nil); got != "/private/tmp/ws-rc23" {
		t.Errorf("ls reported %q, want the workspace the session was created with", got)
	}

	// With nothing recorded, the derivation still answers, which is what a
	// sandbox created before the index existed relies on.
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	if got := workspaceOf("brig-claude-code", nil); got != "/somewhere/else" {
		t.Errorf("ls reported %q for an unrecorded sandbox, want the derived path", got)
	}
}

// `brig ls` is where brig learns that a sandbox went away without going
// through `brig rm` -- removed with the runtime's own CLI, say -- so it is
// where the index is pruned. An entry whose sandbox is still listed stays,
// stopped or not: a stopped sandbox is the same session, and its home is where
// the work is.
func TestListingPrunesTheSessionsWhoseSandboxIsGone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)

	index := filepath.Join(dir, "sessions.json")
	body := `{` +
		`"claude-code@kept": {"home": "/ws/kept", "sandbox": "brig-claude-code-kept"},` +
		`"claude-code@gone": {"home": "/ws/gone", "sandbox": "brig-claude-code-gone"}}`
	if err := os.WriteFile(index, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// What the runtime lists: the stopped sandbox brig still has, and one
	// container of somebody else's, which is why the prune is handed the whole
	// list rather than brig's own share of it.
	pruneSessionIndex([]runtime.Instance{
		{Name: "brig-claude-code-kept", State: "stopped"},
		{Name: "some-other-container", State: "running"},
	})

	if got := wrap.WorkspaceOfSandbox("brig-claude-code-kept"); got != "/ws/kept" {
		t.Errorf("ls pruned a session whose sandbox it just listed: %q", got)
	}
	if got := wrap.WorkspaceOfSandbox("brig-claude-code-gone"); got != "" {
		t.Errorf("ls kept %q for a sandbox the runtime does not have", got)
	}
}

// The two variable-width columns hold what is in them, so each is as wide as
// its own widest value. A constant was wrong at both ends: brig-claude-desktop
// plus a ten-character session slug is 30 characters, which ran into the state
// and pushed the rest of the row out, and a listing of short names paid for the
// width anyway. An agent from a file can be called anything, so there is no
// constant that fits every name brig can generate -- nor every ref, which is
// that name plus a label of up to ten characters.
func TestSandboxListingSizesItsColumnsToTheirContents(t *testing.T) {
	long := sandboxPrefix + "claude-desktop-" + strings.Repeat("s", 10)
	longRef := "claude-desktop@" + strings.Repeat("s", 10)
	short := sandboxPrefix + "codex"
	for _, c := range []struct {
		what      string
		rows      []sandboxRow
		ref, name int
	}{
		{"the longest name brig can generate", []sandboxRow{
			{ref: longRef, name: long, state: "stopped", workspace: "/ws/long"},
		}, len(longRef), len(long)},
		{"values shorter than their headings", []sandboxRow{
			{ref: "codex", name: short, state: "running", workspace: "/ws/codex"},
		}, len("codex"), len(short)},
		// A sandbox with no ref prints the placeholder, and a column of
		// placeholders is as wide as the heading over it.
		{"a sandbox with no ref", []sandboxRow{
			{name: short, state: "running", workspace: "/ws/codex"},
		}, len("REF"), len(short)},
		// The no-runtime case prints the header and no rows, and a header with
		// nothing under it is as wide as the headings.
		{"a header on its own", nil, len("REF"), len("SANDBOX")},
	} {
		var buf bytes.Buffer
		printSandboxes(&buf, c.rows)
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != len(c.rows)+1 {
			t.Fatalf("%s: listing has %d lines, want a header and %d row(s):\n%s",
				c.what, len(lines), len(c.rows), buf.String())
		}
		for i, line := range lines {
			ref, name, state := "REF", "SANDBOX", "STATE"
			if i > 0 {
				r := c.rows[i-1]
				ref, name, state = refCell(r.ref), r.name, r.state
			}
			// The ref leads the row: it is the column that is also the
			// identifier every other verb takes.
			if !strings.HasPrefix(line, ref) {
				t.Errorf("%s: line %d is %q, want it to start with %q", c.what, i, line, ref)
			}
			if got := strings.Index(line, name); got != c.ref+1 {
				t.Errorf("%s: line %d puts %q at column %d, want %d:\n%s",
					c.what, i, name, got, c.ref+1, buf.String())
			}
			if got := strings.Index(line, state); got != c.ref+1+c.name+1 {
				t.Errorf("%s: line %d puts %q at column %d, want %d:\n%s",
					c.what, i, state, got, c.ref+1+c.name+1, buf.String())
			}
		}
	}
}
