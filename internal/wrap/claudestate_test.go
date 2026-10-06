package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateProfile keeps its onboarding file inside the config directory, the
// way claude-code does with CLAUDE_CONFIG_DIR set.
const stateProfile = `
onboarding:
  file: .claude/.claude.json
  seed:
    hasCompletedOnboarding: true
  trustKey: [projects, hasTrustDialogAccepted]
`

func stateConfig(t *testing.T) (*Config, string) {
	t.Helper()
	ws := t.TempDir()
	return testConfig(t, ws, ws, testProfile(t, stateProfile)), ws
}

// preparedBefore leaves the marker an earlier run of brig writes, so the
// workspace reads as one brig prepared.
func preparedBefore(t *testing.T, ws string) {
	t.Helper()
	writeWorkspaceFile(t, ws, markerFile, "brig\n")
}

func writeWorkspaceFile(t *testing.T, ws, rel, body string) {
	t.Helper()
	p := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An existing workspace keeps its state: the file moves to where the agent
// now reads it, and nothing is left behind at the old path.
func TestClaudeStateMovesIntoTheConfigDir(t *testing.T) {
	c, ws := stateConfig(t)
	preparedBefore(t, ws)
	writeWorkspaceFile(t, ws, ".claude.json", `{"numStartups":42}`)
	if err := c.migrateClaudeState(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(ws, ".claude", ".claude.json"))
	if err != nil || string(got) != `{"numStartups":42}` {
		t.Fatalf("moved state = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("the old file is still there: %v", err)
	}
}

// What is already at the new path is the agent's, and wins.
func TestClaudeStateMoveDoesNotOverwrite(t *testing.T) {
	c, ws := stateConfig(t)
	preparedBefore(t, ws)
	writeWorkspaceFile(t, ws, ".claude.json", `{"old":true}`)
	writeWorkspaceFile(t, ws, ".claude/.claude.json", `{"new":true}`)
	if err := c.migrateClaudeState(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, ".claude", ".claude.json"))
	if string(got) != `{"new":true}` {
		t.Errorf("the newer state was overwritten: %s", got)
	}
	if old, _ := os.ReadFile(filepath.Join(ws, ".claude.json")); string(old) != `{"old":true}` {
		t.Errorf("the old file was touched: %s", old)
	}
}

func TestClaudeStateMoveRefusesASymlink(t *testing.T) {
	t.Run("at the old file", func(t *testing.T) {
		c, ws := stateConfig(t)
		preparedBefore(t, ws)
		victim := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(victim, []byte(`{"auths":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		plantLink(t, ws, ".claude.json", victim)
		wantRefused(t, c.migrateClaudeState(mustRoot(t, c)), ".claude.json")
		if _, err := os.Lstat(filepath.Join(ws, ".claude", ".claude.json")); !os.IsNotExist(err) {
			t.Errorf("something was moved through the link: %v", err)
		}
	})
	t.Run("at the config dir", func(t *testing.T) {
		c, ws := stateConfig(t)
		preparedBefore(t, ws)
		outside := t.TempDir()
		writeWorkspaceFile(t, ws, ".claude.json", `{}`)
		plantLink(t, ws, ".claude", outside)
		wantRefused(t, c.migrateClaudeState(mustRoot(t, c)), ".claude")
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("the state was moved out of the workspace: %v", entries)
		}
	})
}

// A profile whose state file is anywhere else is not touched.
func TestClaudeStateMoveIsOnlyForTheNewLayout(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws, testProfile(t, "onboarding:\n  file: .claude.json\n"))
	preparedBefore(t, ws)
	writeWorkspaceFile(t, ws, ".claude.json", `{}`)
	if err := c.migrateClaudeState(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".claude.json")); err != nil {
		t.Errorf("the file was moved for a profile that reads it at the root: %v", err)
	}
}

// A fresh workspace has no .claude until something makes it, and the seed
// still has to land, with real JSON, before the volume targets are prepared:
// a target made first would be an empty file the seed then leaves alone.
func TestPrepareWorkspaceSeedsStateInsideTheConfigDir(t *testing.T) {
	c, ws := stateConfig(t)
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(ws, ".claude", ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "hasCompletedOnboarding") ||
		!strings.Contains(string(got), "hasTrustDialogAccepted") {
		t.Errorf("seed = %s", got)
	}
}

// A directory brig never prepared keeps its file: it belongs to whoever put
// it there.
func TestClaudeStateStaysInAHomeBrigNeverPrepared(t *testing.T) {
	c, ws := stateConfig(t)
	writeWorkspaceFile(t, ws, ".claude.json", `{"numStartups":7}`)
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if old, err := os.ReadFile(filepath.Join(ws, ".claude.json")); err != nil || string(old) != `{"numStartups":7}` {
		t.Errorf("the file was moved or changed: %q, %v", old, err)
	}
}

// The user's own home keeps the host agent's state where the host agent
// reads it, even after brig ran there before.
func TestClaudeStateStaysInTheUsersHome(t *testing.T) {
	c, ws := stateConfig(t)
	t.Setenv("HOME", ws)
	preparedBefore(t, ws)
	writeWorkspaceFile(t, ws, ".claude.json", `{"mcpServers":{"x":{}}}`)
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if old, err := os.ReadFile(filepath.Join(ws, ".claude.json")); err != nil || string(old) != `{"mcpServers":{"x":{}}}` {
		t.Errorf("the host agent's state was moved or changed: %q, %v", old, err)
	}
}
