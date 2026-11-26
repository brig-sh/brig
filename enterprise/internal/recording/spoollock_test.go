// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdShared makes the next OpenSpool find a shared lock on its new file, as a
// sweep of orphan spools takes one, and drops it after hold (never when hold
// is zero).
func holdShared(t *testing.T, hold time.Duration) {
	t.Helper()
	afterSpoolCreate = func(path string) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil {
			t.Error(err)
		}
		if hold == 0 {
			t.Cleanup(func() { f.Close() })
			return
		}
		go func() { time.Sleep(hold); f.Close() }()
	}
	t.Cleanup(func() { afterSpoolCreate = func(string) {} })
}

// A shared lock held for a moment by a sweep does not fail the open.
func TestOpenSpoolWaitsOutAMomentaryLock(t *testing.T) {
	holdShared(t, 25*time.Millisecond)
	rec, err := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), time.Now(), 0)
	if err != nil {
		t.Fatalf("OpenSpool = %v", err)
	}
	rec.Close()
}

// When the lock cannot be taken the file this call created is removed, so no
// empty spool stays behind.
func TestOpenSpoolThatCannotLockRemovesItsFile(t *testing.T) {
	holdShared(t, 0)
	path := filepath.Join(t.TempDir(), "a.spool")
	if rec, err := OpenSpool(path, time.Now(), 0); err == nil {
		rec.Close()
		t.Fatal("OpenSpool succeeded under a held lock")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the spool is still there: %v", err)
	}
}

// A spool that already exists is not this call's, and is left alone.
func TestOpenSpoolLeavesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.spool")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(path, time.Now(), 0); err == nil {
		t.Fatal("OpenSpool opened an existing file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the existing file was removed: %v", err)
	}
}
