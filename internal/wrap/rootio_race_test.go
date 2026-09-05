package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A smoke test under load, and NOT the proof that the escape is closed.
//
// Read this honestly: with both round-5 defects restored, this test still
// passes on this machine (opened=24365, refused=0, nothing outside). The racer
// never coincides with the window, so it cannot distinguish the fixed code from
// the broken code, and a test that cannot fail is worth exactly what round 6
// said it is worth.
//
// It is kept because it exercises the real openWorkspace tens of thousands of
// times against a hostile parent and asserts the property that matters -- that
// nothing brig writes lands outside the directory it checked. The proof that
// each individual defect is closed lives in the deterministic tests: they drive
// the window through the afterWorkspaceCheck seam and each fails when its own
// guard is removed.
func TestWorkspaceIsNeverRedirectedUnderARace(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	outside := filepath.Join(base, "outside")
	realParent := filepath.Join(base, "parent.real")

	for _, d := range []string{work, filepath.Join(outside, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			// present -> absent -> symlink -> absent -> present, holding each
			// state briefly. The pauses matter: flipping as fast as the loop
			// allows never coincides with an open, so a racer without them
			// reports zero escapes against code that escapes readily -- a test
			// that proves nothing. Verified by restoring both defects: without
			// the pauses the racer passed; with them it fails.
			//
			// The absent steps are the attack itself: a component that could
			// not be Lstat'd used to be skipped rather than refused.
			_ = os.Rename(parent, realParent)
			time.Sleep(50 * time.Microsecond)
			_ = os.Symlink(outside, parent)
			time.Sleep(300 * time.Microsecond)
			_ = os.Remove(parent)
			time.Sleep(50 * time.Microsecond)
			_ = os.Rename(realParent, parent)
			time.Sleep(300 * time.Microsecond)
		}
	}()
	t.Cleanup(func() { stop.Store(true); <-done })

	var opened, refused int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := &Config{Workspace: work}
		r, err := c.openWorkspace()
		if err != nil {
			refused++
			continue
		}
		opened++
		_ = r.writeFile(".brig-git-credential", []byte("#!/bin/sh\necho pwned\n"), 0o755)
		_ = r.writeFile("probe", []byte("x"), 0o600)
		_ = r.Close()
	}
	stop.Store(true)
	<-done
	t.Logf("opened=%d refused=%d", opened, refused)

	if opened == 0 {
		t.Fatal("never opened the workspace successfully; the test proves nothing")
	}
	for _, name := range []string{".brig-git-credential", "probe"} {
		if _, err := os.Lstat(filepath.Join(outside, "work", name)); err == nil {
			t.Errorf("ESCAPED: brig wrote %s outside the workspace it checked", name)
		}
	}
}

// A component that vanishes must be a refusal, not a skip.
//
// This is the specific step the escape above depended on: the old walk did
// `if err != nil { continue }`, so removing a parent for an instant made the
// whole path check succeed without looking at it.
func TestAVanishingComponentIsRefused(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	old := afterWorkspaceCheck
	t.Cleanup(func() { afterWorkspaceCheck = old })

	// Remove the parent between the pre-create pass and the strict walk, using
	// the seam so this is deterministic rather than a race.
	afterWorkspaceCheck = func() {}
	if root, _, err := openWorkspaceHandle(work); err != nil {
		t.Fatalf("the honest path should pass: %v", err)
	} else {
		_ = root.Close()
	}
	if err := os.Rename(parent, filepath.Join(base, "gone")); err != nil {
		t.Fatal(err)
	}
	if root, _, err := openWorkspaceHandle(work); err == nil {
		_ = root.Close()
		t.Fatal("a component that vanished mid-check was accepted")
	} else if !errors.Is(err, errPlantedSymlink) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The walk must inspect EVERY component, not just the immediate parent.
//
// Round 6 showed the previous test only ever planted its link one level up, so
// crippling the walk to depth 1 passed the whole suite. This nests three deep
// and plants at the grandparent.
func TestWalkChecksEveryComponentNotJustTheParent(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c")
	work := filepath.Join(deep, "work")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "b", "c", "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Plant at <base>/a -- three levels above the workspace.
	if err := os.Rename(filepath.Join(base, "a"), filepath.Join(base, "a.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "a")); err != nil {
		t.Fatal(err)
	}

	c := &Config{Workspace: work}
	_, err := c.openWorkspace()
	if err == nil {
		t.Fatal("a symlink three levels up was not noticed")
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
