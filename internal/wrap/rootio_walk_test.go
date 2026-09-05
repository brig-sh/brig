package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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
//
// This one does not pin the window above. It was checked against the commit
// before this change and passed there too, because the walk had already been
// taught to refuse a component it could not read rather than skip it. What it
// pins is that older fix: putting the skip back makes it fail. Worth keeping
// for that, and worth saying so, because a test that looks like it guards the
// change it ships with and does not is worse than no test at all.
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

// TestWorkspaceSwappedBetweenLstatAndOpen drives the one window left inside a
// step of the descent. A step looks at a name, sees a directory, and opens it.
// os.Root follows a symlink that stays under the root, so a link swapped in
// between those two calls is followed, and the path check that comes after
// agrees with it, because the name now resolves to where the handle landed.
// The step has to ask the handle what it opened and compare that with what it
// looked at.
func TestWorkspaceSwappedBetweenLstatAndOpen(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	outside := filepath.Join(parent, "outside") // under the same root, so os.Root follows a link to it
	for _, d := range []string{work, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	old := betweenLstatAndOpen
	t.Cleanup(func() { betweenLstatAndOpen = old })
	betweenLstatAndOpen = func(name string) {
		if name != "work" {
			return
		}
		betweenLstatAndOpen = func(string) {}
		if err := os.Rename(work, filepath.Join(parent, "work.real")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink("outside", work); err != nil {
			t.Error(err)
		}
	}

	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err != nil {
		if !errors.Is(err, errPlantedSymlink) {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
		return
	}
	defer func() { _ = r.Close() }()
	if err := r.writeFile(".brig-git-credential", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write through the root failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, ".brig-git-credential")); err == nil {
		t.Fatal("the workspace was swapped for a link between the look and the open, and brig wrote through it")
	}
}

// TestVerifyStillOursRefusesAWorkspaceThatMoved pins the check made just
// before the path is handed to the runtime: the name must still resolve to the
// directory brig holds.
func TestVerifyStillOursRefusesAWorkspaceThatMoved(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	other := filepath.Join(base, "other")
	for _, d := range []string{work, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.verifyStillOurs(); err != nil {
		t.Fatalf("an untouched workspace was refused: %v", err)
	}
	if err := os.Rename(work, filepath.Join(base, "work.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, work); err != nil {
		t.Fatal(err)
	}
	err = r.verifyStillOurs()
	if err == nil {
		t.Fatal("a workspace whose name now points elsewhere was handed on")
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A component swapped for a fifo between the look and the open hangs the boot.
//
// The descent refuses a symlink, and that was the only type it looked at.
// os.Root.OpenRoot on a fifo blocks until a writer arrives, so a guest that
// can swap a parent component -- the same capability the swap above assumes --
// can stop brig rather than redirect it. A hang is not an escape, but it is
// reachable from the same position and it never times out on its own.
//
// The test runs the open in a goroutine because a regression here does not
// fail, it stops: without the bound below the whole package would hang.
func TestWorkspaceSwappedForAFifoDoesNotHang(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	old := betweenLstatAndOpen
	t.Cleanup(func() { betweenLstatAndOpen = old })
	betweenLstatAndOpen = func(name string) {
		if name != "work" {
			return
		}
		betweenLstatAndOpen = func(string) {}
		if err := os.Rename(work, filepath.Join(parent, "work.real")); err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Mkfifo(work, 0o644); err != nil {
			t.Error(err)
		}
	}

	done := make(chan error, 1)
	go func() {
		c := &Config{Workspace: work}
		r, err := c.openWorkspace()
		if r != nil {
			_ = r.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fifo was accepted as the workspace")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a fifo hung: the descent looked only for symlinks, so a component " +
			"swapped for a fifo blocks the boot with no timeout")
	}
}

// The descent must not resolve a component's name twice.
//
// Guarding the open with O_DIRECTORY stops a fifo blocking us, but the step
// then opened the same name again to get a root, and a swap landing between
// those two opens was caught only afterwards. The step opens the descriptor
// once now and builds the root from that, so there is no second lookup to
// race. The test renames the component away between the two, which under the
// old shape is the moment the second open would have found something else.
func TestDescentDoesNotResolveAComponentTwice(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	decoy := filepath.Join(parent, "decoy")
	for _, d := range []string{work, decoy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A marker only the real workspace has, so the assertion is about which
	// directory the handle holds rather than about the walk finishing.
	if err := os.WriteFile(filepath.Join(work, "real"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := betweenLstatAndOpen
	t.Cleanup(func() { betweenLstatAndOpen = old })
	betweenLstatAndOpen = func(name string) {
		if name != "work" {
			return
		}
		betweenLstatAndOpen = func(string) {}
		// Put a different directory at the name the step is about to open.
		if err := os.Rename(work, filepath.Join(parent, "work.real")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(decoy, work); err != nil {
			t.Error(err)
		}
	}

	c := &Config{Workspace: work}
	r, err := c.openWorkspace()
	if err != nil {
		// Refusing is a correct outcome: the name stopped naming what was
		// opened. What must not happen is opening the decoy and calling it the
		// workspace.
		return
	}
	defer func() { _ = r.Close() }()
	if _, err := r.lstat("real"); err != nil {
		t.Fatalf("the handle is not the directory the walk looked at: %v", err)
	}
}
