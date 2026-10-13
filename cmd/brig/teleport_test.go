package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// --teleportable is brig's on the run line, and the agent's once its own
// arguments begin, like every other run-line flag.
func TestParseTeleportable(t *testing.T) {
	o, _, _, err := parse("run", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Teleportable {
		t.Error("teleportable defaulted to on")
	}
	if o, _, _, _ = parse("run", []string{"claude", "--teleportable"}); !o.load.Teleportable {
		t.Error("--teleportable did not set it")
	}
	if o, _, _, _ = parse("run", []string{"--teleportable", "claude"}); !o.load.Teleportable {
		t.Error("--teleportable before the ref did not set it")
	}
	if o, _, tail, _ := parse("run", []string{"claude", "-p", "hi", "--teleportable"}); o.load.Teleportable ||
		!strings.Contains(strings.Join(tail, " "), "--teleportable") {
		t.Errorf("--teleportable was read out of the agent's own arguments (tail %q)", tail)
	}
}

// A line that asks for a teleportable session and hands it a host directory
// contradicts itself, so it is a usage error, exit 2, the same class as
// --no-project beside a project. Nothing boots: the refusal comes before any
// runtime is looked for.
func TestTeleportableWithAHostDirectoryIsAUsageError(t *testing.T) {
	project := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "--teleportable", "claude", project}, "project"},
		{[]string{"plan", "--teleportable", "claude", project}, "project"},
		{[]string{"run", "--teleportable", "--home", t.TempDir(), "claude"}, "--home"},
	} {
		scratchHost(t)
		_, err := captureStdout(t, func() error { return run(c.args) })
		if code := exitCode(err); code != exitUsage {
			t.Errorf("brig %s exits %d, want %d: %v", strings.Join(c.args, " "), code, exitUsage, err)
			continue
		}
		if !strings.Contains(err.Error(), "--teleportable") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("brig %s: the refusal does not name both halves: %v", strings.Join(c.args, " "), err)
		}
	}
}

// `brig ls` says which sandboxes are teleportable, and for those it names no
// host directory: deriving one from the sandbox name would report a home the
// session never had. The table says it in the WORKSPACE cell and keeps its
// columns; the JSON carries the same fact as an added field.
func TestListingMarksATeleportableSession(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	index := `{` +
		`"claude-code@far": {"home": "", "sandbox": "brig-claude-code-far", "teleportable": true},` +
		`"claude-code": {"home": "/ws/default", "sandbox": "brig-claude-code"}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := sandboxRows([]runtime.Instance{
		{Name: "brig-claude-code", State: "running"},
		{Name: "brig-claude-code-far", State: "running"},
	}, nil)

	var table bytes.Buffer
	printSandboxes(&table, rows)
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "REF SANDBOX STATE WORKSPACE" {
		t.Errorf("the listing's columns changed: %q", lines[0])
	}
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "claude-code@far "):
			if !strings.HasSuffix(line, " (in the sandbox disk)") {
				t.Errorf("the teleportable row reads %q", line)
			}
		case strings.HasPrefix(line, "claude-code "):
			if !strings.HasSuffix(line, " /ws/default") {
				t.Errorf("the ordinary row reads %q", line)
			}
		}
	}

	blob, err := json.Marshal(sandboxListData(rows, nil))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sandboxes {
		_, has := s["teleportable"]
		_, ws := s["workspace"]
		switch s["sandbox"] {
		case "brig-claude-code-far":
			if s["teleportable"] != true || ws {
				t.Errorf("the teleportable sandbox reads %v", s)
			}
		case "brig-claude-code":
			// Omitted when false, so an ordinary listing is unchanged.
			if has {
				t.Errorf("an ordinary sandbox carries teleportable: %v", s)
			}
		}
	}
}
