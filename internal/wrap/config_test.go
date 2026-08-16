package wrap

import (
	"os"
	"path/filepath"
	"testing"
)

// The cases the bash wrapper's own suite ran for the guest-cwd block.
func TestGuestCwd(t *testing.T) {
	cases := []struct{ cwd, workspace, want string }{
		{"/h/sandboxed-claude-foo/proj", "/h/sandboxed-claude-foo", "/home/claude/proj"},
		{"/h/sandboxed-claude-foo", "/h/sandboxed-claude-foo", "/home/claude"},
		{"/h/elsewhere", "/h/sandboxed-claude-foo", "/home/claude"},
		// A named run's workspace is the suffixed directory, so a cwd in the
		// unnamed base does not match it: this run mounts only its own
		// workspace, so there is no guest directory to start in.
		{"/h/sandboxed-claude/proj", "/h/sandboxed-claude-foo", "/home/claude"},
		{"/h/sandboxed-claude-foo/a/b", "/h/sandboxed-claude-foo", "/home/claude/a/b"},
	}
	for _, c := range cases {
		if got := GuestCwd(c.cwd, c.workspace, "/home/claude"); got != c.want {
			t.Errorf("GuestCwd(%q, %q) = %q, want %q", c.cwd, c.workspace, got, c.want)
		}
	}
}

// The agent resolves the trust key to the git repository root, so keying on
// the cwd writes a key that is never read back and leaves the dialog up from
// any subdirectory of a repository.
func TestTrustKeyResolvesToTheRepositoryRoot(t *testing.T) {
	ws := t.TempDir()
	mustMkdir(t, filepath.Join(ws, "myrepo", "sub"))
	mustMkdir(t, filepath.Join(ws, "myrepo", ".git"))
	mustMkdir(t, filepath.Join(ws, "plain"))

	cases := []struct{ cwd, want string }{
		{filepath.Join(ws, "myrepo", "sub"), "/home/claude/myrepo"},
		{filepath.Join(ws, "myrepo"), "/home/claude/myrepo"},
		{filepath.Join(ws, "plain"), "/home/claude/plain"}, // no repository: the directory itself
		{ws, "/home/claude"},
	}
	for _, c := range cases {
		if got := TrustKey(c.cwd, ws, "/home/claude"); got != c.want {
			t.Errorf("TrustKey(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

// A worktree or submodule checkout has .git as a file, not a directory.
func TestTrustKeyAcceptsAGitFile(t *testing.T) {
	ws := t.TempDir()
	mustMkdir(t, filepath.Join(ws, "wt", "sub"))
	if err := os.WriteFile(filepath.Join(ws, "wt", ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TrustKey(filepath.Join(ws, "wt", "sub"), ws, "/home/claude"); got != "/home/claude/wt" {
		t.Errorf("TrustKey = %q, want /home/claude/wt", got)
	}
}

// The guest has nothing but the workspace mounted, so a repository ABOVE the
// workspace is invisible in there: the walk must stop at the workspace rather
// than keying on a root the guest cannot see.
func TestTrustKeyWalkStopsAtTheWorkspace(t *testing.T) {
	outer := t.TempDir()
	mustMkdir(t, filepath.Join(outer, ".git"))
	ws := filepath.Join(outer, "workspace")
	mustMkdir(t, filepath.Join(ws, "proj"))

	if got := TrustKey(filepath.Join(ws, "proj"), ws, "/home/claude"); got != "/home/claude/proj" {
		t.Errorf("TrustKey = %q, want /home/claude/proj (the .git above the workspace is not mounted)", got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
