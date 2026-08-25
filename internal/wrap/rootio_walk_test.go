package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkspaceParentSwappedDuringTheWalk drives the window inside the walk
// rather than the one after it.
//
// The walk proves that no component on the way to the workspace is a symlink,
// and hands back the identity of the directory it ended on. Both halves are
// done by resolving a path string, and a string is resolved from the top every
// time. So the step that reaches the leaf re-reads the prefix that the earlier
// steps had just finished vouching for.
//
// Flip a parent into a symlink in that gap and the leaf is reached through it.
// The identity handed back is then the attacker's directory, the open resolves
// the same string the same way and lands in the same place, and the comparison
// between them is two wrong answers agreeing. Nothing downstream can tell,
// because everything downstream was derived from the poisoned resolution.
//
// The undirected version of this is TestWorkspaceWritesNeverLandOutsideUnderARace,
// which wins the same race by spinning. This one takes it deterministically
// through the seam, so the window is pinned by a test that cannot pass by
// getting lucky.
func TestWorkspaceParentSwappedDuringTheWalk(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	old := duringWorkspaceWalk
	t.Cleanup(func() { duringWorkspaceWalk = old })
	duringWorkspaceWalk = func() {
		duringWorkspaceWalk = func() {} // once, so only the leaf step is hit
		if err := os.Rename(parent, filepath.Join(base, "parent.real")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Error(err)
		}
	}

	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err != nil {
		// Refusing is a correct outcome, as long as it is refused for being
		// what it is rather than by accident.
		if !errors.Is(err, errPlantedSymlink) {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
		return
	}
	defer func() { _ = r.Close() }()

	// Allowed, so it must be holding the real workspace. Writing the guest git
	// credential helper is the write that matters: it is mode 0755 and git
	// runs it on the host, so a redirected one is host code execution.
	if err := r.writeFile(".brig-git-credential", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write through the root failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "work", ".brig-git-credential")); err == nil {
		t.Fatal("the parent was swapped during the walk and brig wrote through it")
	}
}

// TestWorkspaceParentVanishesDuringTheWalk is the same seam with the parent
// removed rather than replaced. A component that is not there is a component
// being moved, so the run has to stop rather than resolve whatever appears
// next.
func TestWorkspaceParentVanishesDuringTheWalk(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	old := duringWorkspaceWalk
	t.Cleanup(func() { duringWorkspaceWalk = old })
	duringWorkspaceWalk = func() {
		duringWorkspaceWalk = func() {}
		if err := os.Rename(parent, filepath.Join(base, "gone")); err != nil {
			t.Error(err)
		}
	}

	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err == nil {
		_ = r.Close()
		t.Fatal("a parent that vanished mid-walk was accepted")
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
