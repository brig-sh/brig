package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace is created and opened by path, and MkdirAll and OpenRoot both
// resolve the whole of it -- so a symlink at ANY component redirects the
// sandbox's home, not just one at the workspace itself. Lstat'ing the last
// component was the whole of the check, and it is the only check that can see
// this: once os.Root holds a directory handle, the redirection has already
// happened and every path below it is honestly "inside the root".
//
// The setup is the one that makes this reachable: a workspace under a
// directory the guest can write to, which is any workspace nested inside
// another sandbox's.
func TestWorkspaceRefusesASymlinkedParentComponent(t *testing.T) {
	tmp, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "home")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	// tmp/parent -> outside, so tmp/parent/work is outside/work.
	link := filepath.Join(tmp, "parent")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	c := testConfig(t, filepath.Join(link, "work"), tmp)
	_, err := c.openWorkspace()
	wantRefused(t, err, link)

	// Nothing was created through the link: the refusal comes before any
	// directory is made.
	if _, err := os.Stat(filepath.Join(outside, "work")); !os.IsNotExist(err) {
		t.Errorf("the workspace was created through the planted link anyway: %v", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not say what it found: %v", err)
	}
}

// The link at the workspace itself keeps its own message, which names
// BRIG_WORKSPACE and the directory to point it at.
func TestWorkspaceStillRefusesASymlinkAtTheWorkspace(t *testing.T) {
	tmp, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(tmp, "ws")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, link, tmp)
	_, err := c.openWorkspace()
	wantRefused(t, err, link)
}

// An ordinary workspace under an ordinary directory is not refused -- and on
// macOS every temporary directory sits under /var, which is itself a symlink,
// so this is the case that says the check knows the difference between a link
// the operating system ships and one a sandbox planted.
func TestWorkspaceAcceptsAnOrdinaryPath(t *testing.T) {
	tmp := t.TempDir()
	c := testConfig(t, filepath.Join(tmp, "nested", "work"), tmp)
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatalf("an ordinary workspace was refused: %v", err)
	}
	_ = r.Close()
}
