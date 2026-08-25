package wrap

import (
	"os"
	"path/filepath"
	"testing"
)

// trustDirs declares the named directories, and every directory above them,
// to be ones brig's user cannot write, for the duration of the test. That is
// what a root-owned /Users or a root-owned, search-only /home looks like to the
// walk, and it is the only way to put one on a test path without running as
// root.
//
// The ancestors have to come along, because trust cannot resume once it is
// lost: a test directory sits under a temp directory the user owns, and an
// attacker who can write there can swap out everything below it wholesale,
// however that lower directory is owned. Declaring only the directory itself
// trusted would leave the walk correctly stopping above it, and test nothing.
func trustDirs(t *testing.T, dirs ...string) {
	t.Helper()
	trusted := map[string]bool{}
	for _, d := range dirs {
		for p := filepath.Clean(d); ; p = filepath.Dir(p) {
			trusted[p] = true
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	old := dirWritableByUs
	t.Cleanup(func() { dirWritableByUs = old })
	dirWritableByUs = func(path string, info os.FileInfo) bool {
		if trusted[filepath.Clean(path)] {
			return false
		}
		return old(path, info)
	}
}

// TestWorkspaceUnderASearchOnlyAncestorOpens is the shared-host layout: a
// root-owned, search-only directory on the way to the workspace, such as a
// /home or /srv/agents set to 0711 so users cannot list each other. Traversing
// it needs only search permission. Reading it is not required and must not be,
// because on that layout the user cannot.
func TestWorkspaceUnderASearchOnlyAncestorOpens(t *testing.T) {
	base := t.TempDir()
	gate := filepath.Join(base, "gate")
	home := filepath.Join(gate, "home")
	work := filepath.Join(home, "ws")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gate, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gate, 0o755) })
	trustDirs(t, base, gate)

	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatalf("a search-only ancestor refused the workspace: %v", err)
	}
	defer func() { _ = r.Close() }()
	if err := r.writeFile("probe", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "probe")); err != nil {
		t.Fatalf("the write did not land in the workspace: %v", err)
	}
}

// TestWorkspaceBehindATrustedAbsoluteSymlinkOpens is the admin layout: a
// root-owned absolute link on the way, /data pointing at /mnt/data. It is
// inside the part of the path the guest cannot touch, so it is safe to follow,
// and refusing it would break a layout that worked before.
func TestWorkspaceBehindATrustedAbsoluteSymlinkOpens(t *testing.T) {
	base := t.TempDir()
	gate := filepath.Join(base, "gate")
	real := filepath.Join(base, "real")
	work := filepath.Join(real, "ws")
	if err := os.MkdirAll(gate, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(gate, "data")
	if err := os.Symlink(real, link); err != nil { // absolute target
		t.Fatal(err)
	}
	trustDirs(t, base, gate)

	c := &Config{Workspace: filepath.Join(link, "ws")}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatalf("a trusted absolute symlink refused the workspace: %v", err)
	}
	defer func() { _ = r.Close() }()
	if err := r.writeFile("probe", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "probe")); err != nil {
		t.Fatalf("the write did not land in the real directory: %v", err)
	}
}

// TestWorkspaceRefusesAnUntrustedSymlinkInTheTail is the other half of the
// same rule. A link the guest could have planted sits in a directory brig's
// user can write, and it is refused however it is spelled.
func TestWorkspaceRefusesAnUntrustedSymlinkInTheTail(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "planted")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// base is ours, so nothing under it is trusted.
	c := &Config{Workspace: filepath.Join(link, "ws")}
	if r, err := c.openWorkspace(); err == nil {
		_ = r.Close()
		t.Fatal("a symlink in a writable directory was followed")
	}
}
