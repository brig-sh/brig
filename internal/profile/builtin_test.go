package profile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A Brigfile that says builtin:claude-code means the profile brig ships. A
// file of that name in the profile directory overrides the registry entry,
// and must not reach this lookup, or a repository would run whatever image
// the reader's own override pins while claiming to run brig's.
func TestBuiltInIgnoresAFileOverride(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	override := []byte("name: claude-code\nbinary: claude\nimage: example.com/mine:1\n" +
		"guestHome: /root\nmem: 512\ncpus: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), override, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := Lookup("claude-code"); got.Image != "example.com/mine:1" {
		t.Fatalf("the override did not load, so this test proves nothing: image %q", got.Image)
	}

	p, blob, ok := BuiltIn("claude-code")
	if !ok {
		t.Fatal("BuiltIn(claude-code) found nothing")
	}
	if p.Image == "example.com/mine:1" || bytes.Contains(blob, []byte("example.com/mine")) {
		t.Errorf("BuiltIn returned the override: image %q", p.Image)
	}
	want, err := builtinFS.ReadFile("specs/claude-code.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, want) {
		t.Error("BuiltIn did not return the embedded spec bytes")
	}
}

// The lookup reads the embedded specs, so it works before Load has run and
// does not care what the registry holds.
func TestBuiltInNeedsNoRegistry(t *testing.T) {
	reset(t)
	registry = map[string]entry{}
	if _, _, ok := BuiltIn("codex"); !ok {
		t.Error("BuiltIn(codex) found nothing with an empty registry")
	}
}

// Only a canonical name of a shipped spec resolves. An alias is a spelling
// of the registry, and a path-like name must never reach the embedded FS.
func TestBuiltInRefusesWhatIsNotAShippedName(t *testing.T) {
	for _, name := range []string{"", "claude", "nope", "../specs/codex", "specs/codex", "Codex", "codex.yaml"} {
		if _, _, ok := BuiltIn(name); ok {
			t.Errorf("BuiltIn(%q) resolved", name)
		}
	}
}

// Two lookups share nothing, so a caller that edits one cannot change what
// the next caller is handed.
func TestBuiltInReturnsAFreshCopy(t *testing.T) {
	a, _, _ := BuiltIn("claude-code")
	if len(a.Deny) == 0 {
		t.Fatal("claude-code ships no deny list, so this test proves nothing")
	}
	a.Deny[0] = "CHANGED"
	b, _, _ := BuiltIn("claude-code")
	if b.Deny[0] == "CHANGED" {
		t.Error("an edit to one BuiltIn result reached the next")
	}
}
