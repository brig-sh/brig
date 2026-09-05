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
	// Relative to the workspace, which is the guest's home, so the agent finds
	// it at /home/claude/.claude/skills.
	if got[0].Rel != filepath.Join(".claude", "skills") {
		t.Errorf("rel = %s, want .claude/skills", got[0].Rel)
	}
}

// The guest needs to write inside these directories -- installing a plugin,
// populating a cache -- which is the whole reason they are copied rather than
// mounted read-only.
func TestSeedHostConfigCopiesAndStaysWritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".claude", "skills", "ananos")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: hostProjections(tm, true)}

	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md")
	if b, err := os.ReadFile(copied); err != nil {
		t.Fatalf("skill was not copied: %v", err)
	} else if string(b) != "hello" {
		t.Fatalf("copied content = %q", b)
	}
	// Writable, which a read-only mount was not.
	if err := os.WriteFile(filepath.Join(c.Workspace, ".claude", "skills", "new"), []byte("x"), 0o644); err != nil {
		t.Fatalf("the seeded directory is not writable: %v", err)
	}
}

// What the guest has done since belongs to the guest. Re-seeding must not
// overwrite an edited skill or drop a plugin it installed.
func TestSeedHostConfigDoesNotClobberTheGuest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(hostSkills, "ananos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSkills, "ananos", "SKILL.md"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: hostProjections(tm, true)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	// The guest edits the skill and installs one of its own.
	edited := filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md")
	if err := os.WriteFile(edited, []byte("guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(c.Workspace, ".claude", "skills", "guest-installed")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}

	// A second boot.
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(edited); string(b) != "guest" {
		t.Errorf("the guest's edit was overwritten: %q", b)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the guest's own skill was removed: %v", err)
	}
}

// A skill added on the host after the workspace exists should still arrive,
// which is why this seeds entry by entry rather than once per directory.
func TestSeedHostConfigPicksUpNewHostEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(hostSkills, "first"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: hostProjections(tm, true)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(hostSkills, "second"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace, ".claude", "skills", "second")); err != nil {
		t.Errorf("a skill added on the host later did not arrive: %v", err)
	}
}

// The host's own directory is what read-only was protecting. Copying must
// leave it exactly as it was, whatever the guest does to its copy.
func TestSeedHostConfigLeavesTheHostAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSkill := filepath.Join(home, ".claude", "skills", "ananos")
	if err := os.MkdirAll(hostSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(hostSkill, "SKILL.md")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, _ := profile.Lookup("claude-code")
	c := &Config{Workspace: t.TempDir(), Profile: tm, HostConfig: hostProjections(tm, true)}
	if err := c.seedHostConfig(mustRoot(t, c)); err != nil {
		t.Fatal(err)
	}

	// The guest rewrites and deletes inside its copy.
	if err := os.WriteFile(filepath.Join(c.Workspace, ".claude", "skills", "ananos", "SKILL.md"), []byte("guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(c.Workspace, ".claude", "skills", "ananos")); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(hostFile); err != nil {
		t.Fatalf("the host's skill was removed: %v", err)
	} else if string(b) != "host" {
		t.Fatalf("the host's skill was modified: %q", b)
	}
}
