package wrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/brig-sh/brig/internal/runtime"
)

// A run that names no workspace gets a guest home brig creates for it, under
// the state directory and named after the sandbox. That home belongs to the
// sandbox: `brig rm` deletes it with the sandbox, so the next run of the same
// command starts clean. A home named with --home or BRIG_WORKSPACE outside
// that directory is the user's, and brig never deletes it.
//
// Which of the two a session has is recorded in the session index. An entry
// without the flag keeps its home, and that includes every session started by
// a release that put homes in ~/brig.

// homesDirName is the directory under stateDir that holds ephemeral homes.
const homesDirName = "homes"

// homesDir returns the absolute path of the directory brig creates ephemeral
// guest homes in.
func homesDir() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(dir, homesDirName))
}

// homeName returns the homes directory and the name of home inside it, or
// false when home is not a direct child of it.
func homeName(home string) (dir, name string, ok bool) {
	dir, err := homesDir()
	if err != nil {
		return "", "", false
	}
	rel, err := filepath.Rel(dir, home)
	if err != nil || rel == "." || rel == ".." ||
		strings.ContainsRune(rel, filepath.Separator) {
		return "", "", false
	}
	return dir, rel, true
}

// sandboxExists returns whether the runtime holds a sandbox named vmName,
// running or stopped. It is an error when the runtime cannot say, which
// includes a nil runtime and one that does not implement runtime.Exister.
func sandboxExists(rt runtime.Runtime, vmName string) (bool, error) {
	ex, ok := rt.(runtime.Exister)
	if !ok {
		return false, errors.New("the runtime cannot say whether a stopped sandbox exists")
	}
	return ex.Exists(vmName)
}

// removeHome deletes an ephemeral guest home. It returns false when there was
// nothing to delete.
//
// The home has to be a direct child of homesDir, and it is deleted through an
// os.Root opened there. A symlink the guest left in the home is removed, never
// followed. Directories are made writable first: a guest leaves read-only
// trees behind (a Go module cache, for instance), and RemoveAll cannot unlink
// the entries of a directory it may not write.
func removeHome(home string) (bool, error) {
	dir, name, ok := homeName(home)
	if !ok {
		return false, fmt.Errorf("the guest home %s is not under the homes "+
			"directory, so brig left it in place", home)
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// WalkDir hands a directory to the callback before it reads it, so each
	// directory is writable and readable by the time its entries are listed.
	// A symlink is not a directory entry, so it is never chmod-ed or walked.
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			_ = root.Chmod(p, 0o700)
		}
		return nil
	})
	if err := root.RemoveAll(name); err != nil {
		return false, fmt.Errorf("could not delete the guest home %s: %w", home, err)
	}
	return true, nil
}

// ephemeralEntry returns the index entry of the session carrying vmName when
// its home is one brig created, and false otherwise.
func ephemeralEntry(vmName string) (sessionEntry, bool) {
	for _, entry := range readSessionIndex() {
		if entry.Sandbox == vmName && entry.Ephemeral {
			return entry, true
		}
	}
	return sessionEntry{}, false
}

// EphemeralHomeOf returns the guest home `brig rm` would delete with vmName,
// or "" when it would keep it or there is none on disk.
func EphemeralHomeOf(vmName string) string {
	entry, ok := ephemeralEntry(vmName)
	if !ok {
		return ""
	}
	if _, err := os.Lstat(entry.Home); err != nil {
		return ""
	}
	return entry.Home
}

// DropEphemeralHome deletes the guest home of vmName when brig created it, and
// returns the path it deleted, or "" when there was nothing to delete.
//
// Call it only after the runtime has removed the sandbox, and before
// ForgetSandbox drops the record it reads.
func DropEphemeralHome(vmName string) (string, error) {
	entry, ok := ephemeralEntry(vmName)
	if !ok {
		return "", nil
	}
	removed, err := removeHome(entry.Home)
	if err != nil || !removed {
		return "", err
	}
	return entry.Home, nil
}

// reapOrphanHome deletes an ephemeral home that no session owns before a run
// boots on it.
//
// Such a home is left behind when its sandbox went away without `brig rm`,
// through the runtime's own CLI or a reinstall, and `brig ls` pruned the
// entry. Booting on it would hand a new sandbox the old guest state. The
// runtime has to say the sandbox does not exist, stopped or running: a
// listing that leaves stopped sandboxes out makes `brig ls` prune the entry
// of a stopped one, and that sandbox still owns its home. A runtime that
// cannot say deletes nothing. A home the run named is never reaped, even
// inside the homes directory.
func (c *Config) reapOrphanHome() {
	if !c.EphemeralHome || c.homeRemembered || c.homeGiven {
		return
	}
	if _, err := os.Lstat(c.Workspace); err != nil {
		return
	}
	if exists, err := sandboxExists(c.Runtime, c.VMName); err != nil || exists {
		return
	}
	if removed, err := removeHome(c.Workspace); err != nil {
		c.warnf("%v", err)
	} else if removed {
		c.warnf("deleted the guest home %s, left behind by a sandbox that was "+
			"removed outside brig", c.Workspace)
	}
}

// ephemeralNotice tells the user that this run's guest home goes with the
// sandbox. It prints only when brig is about to create that home, so it
// shows once per session.
func (c *Config) ephemeralNotice() {
	if !c.EphemeralHome {
		return
	}
	if _, err := os.Lstat(c.Workspace); err == nil {
		return
	}
	ref := sessionKey(c.Profile.Name, c.Slug)
	if c.Project != "" {
		c.warnf("%s is the only directory of yours shared with %s. Its guest home "+
			"is deleted by `brig rm %s`; pass --home <dir> to keep it.",
			c.Project, ref, ref)
	} else {
		c.warnf("none of your directories is shared with %s, so its guest home "+
			"and everything in it are deleted by `brig rm %s`. Name a project "+
			"(`brig run %s <dir>`) or pass --home <dir> to keep it.", ref, ref, ref)
	}
	if c.legacyHome != "" {
		// --home takes the base, and a named session appends its slug to it.
		c.warnf("%s is no longer the default guest home. Pass --home %s to keep "+
			"using it.", c.legacyHome, legacyDefaultWorkspace(c.Profile, ""))
	}
}
