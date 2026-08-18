package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The workspace check and the things that use its result must agree about
// which directory they mean.
//
// The check walks a path *string*; MkdirAll and OpenRoot then resolve that
// string again. A guest owning a parent component -- the nested-workspace case
// the walk's own comment names -- flips it in between, and everything brig
// writes next lands somewhere else. One of those things is a mode-0755
// credential helper, which makes it host code execution rather than a stray
// write.

// TestWorkspacePathIsCleanedBeforeItIsChecked.
//
// filepath.Clean is lexical and the kernel is not. `<base>/link/../real`
// cleans to `<base>/real`, which is what the walk inspected -- while MkdirAll
// and OpenRoot, handed the path as written, resolved `link` first and then
// applied `..` to wherever it landed. Point the link at a directory whose
// parent is not `<base>` and the two answers are different directories, so the
// walk vouched for one and brig wrote into the other.
func TestWorkspacePathIsCleanedBeforeItIsChecked(t *testing.T) {
	base := t.TempDir()
	checked := filepath.Join(base, "real")        // what the walk sees
	opened := filepath.Join(base, "deep", "real") // where the kernel lands
	inner := filepath.Join(base, "deep", "inner")
	for _, d := range []string{checked, opened, inner} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// link -> <base>/deep/inner, so link/.. is <base>/deep, not <base>.
	if err := os.Symlink(inner, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	// Built by concatenation on purpose: filepath.Join cleans, so composing
	// this with Join would hand openWorkspace an already-cleaned path and test
	// nothing. A path that arrives from a config file or an environment
	// variable has had no such courtesy.
	c := &Config{Workspace: base + "/link/../real"}
	r, err := c.openWorkspace()
	if err != nil {
		// Refusing is acceptable; resolving through the link is not.
		if !errors.Is(err, errPlantedSymlink) {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
		return
	}
	defer func() { _ = r.Close() }()

	if err := r.writeFile("probe", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(opened, "probe")); err == nil {
		t.Fatal("the workspace resolved through the symlink: brig wrote into a directory " +
			"the check never looked at")
	}
	if _, err := os.Lstat(filepath.Join(checked, "probe")); err != nil {
		t.Fatalf("the write did not land in the directory that was checked: %v", err)
	}
}

// TestWorkspaceRefusesAComponentSwappedAfterTheCheck is the TOCTOU itself.
//
// The swap is driven through the afterWorkspaceCheck seam so it lands exactly
// in the window -- after the walk has passed, before the root is opened. Doing
// it any earlier just re-tests the walk, which was the flaw in the first
// version of this test: it passed whether or not the guard it was named for
// existed.
func TestWorkspaceRefusesAComponentSwappedAfterTheCheck(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	work := filepath.Join(parent, "work")
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(elsewhere, "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	old := afterWorkspaceCheck
	t.Cleanup(func() { afterWorkspaceCheck = old })
	afterWorkspaceCheck = func() {
		afterWorkspaceCheck = func() {} // once
		if err := os.Rename(parent, filepath.Join(base, "parent.real")); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(elsewhere, parent); err != nil {
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

	// Allowed. Then it must not be the attacker's directory: brig plants a
	// mode-0755 credential helper in here, so a redirected workspace is host
	// code execution, not a stray file.
	if err := r.writeFile(".brig-git-credential", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "work", ".brig-git-credential")); err == nil {
		t.Fatal("the workspace was swapped after the check and brig wrote through it")
	}
}

// TestWorkspaceWritesNeverLandOutsideUnderARace is the undirected version: an
// attacker flipping a parent component as fast as it can while brig opens the
// workspace and writes. Bounded, so it cannot hang CI; the assertion is not
// "the race was won" but "nothing was ever written outside".
func TestWorkspaceWritesNeverLandOutsideUnderARace(t *testing.T) {
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
	realParent := filepath.Join(base, "parent.real")

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = os.Rename(parent, realParent)
			_ = os.Symlink(outside, parent)
			_ = os.Remove(parent)
			_ = os.Rename(realParent, parent)
		}
	}()
	t.Cleanup(func() { stop.Store(true); <-done })

	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		c := &Config{Workspace: work}
		r, err := c.openWorkspace()
		if err != nil {
			continue // refused, which is the correct outcome
		}
		_ = r.writeFile(".brig-git-credential", []byte("#!/bin/sh\n"), 0o755)
		_ = r.Close()
	}
	stop.Store(true)
	<-done

	// Whatever happened above, nothing brig writes may exist outside.
	for _, name := range []string{".brig-git-credential", "probe"} {
		if _, err := os.Lstat(filepath.Join(outside, "work", name)); err == nil {
			t.Errorf("brig wrote %s outside the workspace", name)
		}
	}
}

// TestReadFileRefusesAFifo: os.Root refuses a symlink out of the workspace and
// opens a FIFO like any other file. open(2) then waits for a writer the guest
// never provides, so brig hangs on the host -- at .claude.json, which is read
// on an ordinary run by default.
func TestReadFileRefusesAFifo(t *testing.T) {
	for _, name := range []string{".claude.json", ".gitconfig"} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(ws, name), 0o644); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			c := &Config{Workspace: ws}
			r, err := c.openWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()

			type res struct{ err error }
			ch := make(chan res, 1)
			go func() {
				_, rerr := r.readFile(name)
				ch <- res{rerr}
			}()
			select {
			case got := <-ch:
				if got.err == nil {
					t.Fatalf("a FIFO at %s was read as if it were a file", name)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("brig blocked reading a guest-planted FIFO at %s", name)
			}
		})
	}
}
