package wrap

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/agent"
)

func TestHostSeeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "ananos"), 0o755); err != nil {
		t.Fatal(err)
	}
	// plugins deliberately absent: a path the user does not have must be
	// skipped, not fail the run.
	tm, _ := agent.Lookup("claude-code")

	if got := hostSeeds(tm, false); got != nil {
		t.Errorf("off by default, got %v", got)
	}
	got := hostSeeds(tm, true)
	if len(got) != 1 {
		t.Fatalf("want 1 seed (skills only), got %v", got)
	}
	if want := filepath.Join(home, ".claude", "skills"); got[0].Host != want {
		t.Errorf("host = %s, want %s", got[0].Host, want)
	}
	if want := filepath.Join(".claude", "skills"); got[0].Rel != want {
		t.Errorf("rel = %s, want %s", got[0].Rel, want)
	}
}

// TestSeedHostConfig covers the two properties the design turns on: it tops up
// new host entries on a later run, and it never overwrites what the sandbox
// already has.
func TestSeedHostConfig(t *testing.T) {
	src := t.TempDir()
	ws := t.TempDir()

	write(t, filepath.Join(src, "alpha", "SKILL.md"), "host alpha")
	write(t, filepath.Join(src, "loose.json"), "{}")
	if err := os.Symlink("alpha/SKILL.md", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	c := &Config{
		Workspace: ws,
		HostSeed:  []Seed{{Host: src, Rel: filepath.Join(".claude", "skills")}},
		Out:       io.Discard,
		Err:       io.Discard,
	}
	if err := c.seedHostConfig(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(ws, ".claude", "skills")
	if got := read(t, filepath.Join(dst, "alpha", "SKILL.md")); got != "host alpha" {
		t.Errorf("alpha = %q", got)
	}
	if got := read(t, filepath.Join(dst, "loose.json")); got != "{}" {
		t.Errorf("top-level files must be seeded too, got %q", got)
	}
	// A symlink is recreated, not followed.
	if fi, err := os.Lstat(filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was followed instead of recreated")
	}

	// The sandbox edits a seeded skill and installs one of its own; the host
	// gains a new skill and changes the one the sandbox edited.
	write(t, filepath.Join(dst, "alpha", "SKILL.md"), "guest edit")
	write(t, filepath.Join(dst, "installed-in-guest", "SKILL.md"), "guest only")
	write(t, filepath.Join(src, "alpha", "SKILL.md"), "host changed")
	write(t, filepath.Join(src, "beta", "SKILL.md"), "host beta")

	if err := c.seedHostConfig(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "alpha", "SKILL.md")); got != "guest edit" {
		t.Errorf("a seeded entry the sandbox changed must not be clobbered, got %q", got)
	}
	if got := read(t, filepath.Join(dst, "beta", "SKILL.md")); got != "host beta" {
		t.Errorf("a new host entry must be topped up, got %q", got)
	}
	if got := read(t, filepath.Join(dst, "installed-in-guest", "SKILL.md")); got != "guest only" {
		t.Errorf("guest-only content must survive, got %q", got)
	}
	// One-way: nothing reaches back into the host directory.
	if _, err := os.Stat(filepath.Join(src, "installed-in-guest")); !os.IsNotExist(err) {
		t.Error("seeding must never write back to the host config")
	}
}

// TestSeedRewritesRoot covers the paths agent config records about itself:
// Claude Code stores an absolute installPath per plugin, which is a host path
// the guest cannot resolve.
func TestSeedRewritesRoot(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	root := filepath.Join(home, ".claude")
	src := filepath.Join(root, "plugins")

	write(t, filepath.Join(src, "installed_plugins.json"),
		`{"installPath":"`+root+`/plugins/cache/x"}`)
	// A git checkout inside the seeded tree must be left alone, whatever it
	// happens to contain.
	write(t, filepath.Join(src, "marketplaces", "m", ".git", "config.json"), root)
	write(t, filepath.Join(src, "marketplaces", "m", "README.md"), root)

	c := &Config{
		Workspace: ws,
		HostSeed: []Seed{{
			Host:      src,
			Rel:       filepath.Join(".claude", "plugins"),
			HostRoot:  root,
			GuestRoot: "/home/claude/.claude",
		}},
		Out: io.Discard,
		Err: io.Discard,
	}
	if err := c.seedHostConfig(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(ws, ".claude", "plugins")
	if got, want := read(t, filepath.Join(dst, "installed_plugins.json")),
		`{"installPath":"/home/claude/.claude/plugins/cache/x"}`; got != want {
		t.Errorf("manifest = %s, want %s", got, want)
	}
	if got := read(t, filepath.Join(dst, "marketplaces", "m", ".git", "config.json")); got != root {
		t.Errorf("a .git directory must not be rewritten, got %s", got)
	}
	if got := read(t, filepath.Join(dst, "marketplaces", "m", "README.md")); got != root {
		t.Errorf("only .json is rewritten, got %s", got)
	}
}

func TestSeedHostConfigNoop(t *testing.T) {
	ws := t.TempDir()
	c := &Config{Workspace: ws, Out: io.Discard, Err: io.Discard}
	if err := c.seedHostConfig(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(ws); err != nil || len(entries) != 0 {
		t.Errorf("without opt-in the workspace must be untouched, got %v", entries)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
