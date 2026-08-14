package wrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func TestHostProjections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "ananos"), 0o755); err != nil {
		t.Fatal(err)
	}
	// plugins deliberately absent: a path the user does not have must be
	// skipped, not fail the run.
	tm, _ := profile.Lookup("claude-code")

	if got := hostProjections(tm, false); got != nil {
		t.Errorf("off by default, got %v", got)
	}
	got := hostProjections(tm, true)
	if len(got) != 1 {
		t.Fatalf("want 1 projection (skills only), got %v", got)
	}
	if got[0].Host != filepath.Join(home, ".claude", "skills") {
		t.Errorf("host = %s", got[0].Host)
	}
	if got[0].Guest != "/home/claude/.claude/skills" {
		t.Errorf("guest = %s, want /home/claude/.claude/skills", got[0].Guest)
	}
	if !got[0].ReadOnly {
		t.Error("projection must be read-only: it is the user's real skills")
	}
}
