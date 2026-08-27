package wrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The workspace index: the host directory each sandbox was started with.
//
// Every invocation resolves the workspace from scratch -- --workspace, then
// BRIG_WORKSPACE, then ~/brig/<profile>-<name> -- and EnsureRunning compares
// the running sandbox against whatever that produced. That is right for an
// invocation that names a directory and wrong for one that does not: a session
// created with --workspace never matches its own default, so the next flagless
// verb reads the mismatch as a stale share and restarts the sandbox. The work
// survives that, because it is in the workspace on the host. The guest's
// memory-only state does not, and an in-sandbox login goes with it.
//
// So the path a sandbox was started with is written down, and read back when
// the invocation names none. It is an index and not a source of truth: the
// runtime stays authoritative about what is actually mounted, the stale-share
// check still runs against whatever path is resolved, and an explicit
// --workspace or BRIG_WORKSPACE still wins -- asking for a different directory
// is something a user is entitled to do, restart and all.
//
// It lives beside gateway-ips.json, which is the same kind of file for the
// same kind of reason: small per-sandbox bookkeeping that has to outlive one
// invocation. Deliberately not a record inside the workspace itself, which
// would make that directory both state and identity -- copy it to a second
// machine, or point two sandbox names at it, and it would carry a claim about
// which sandbox owns it.
//
// Two brig processes recording at once can lose one of the two entries, since
// each reads the file, edits its own key and writes the whole map back. The
// cost is one restart for whichever sandbox lost, which is the cost of not
// having the index at all -- worth less than a lock file on the path every
// boot takes.

// workspaceIndexName is the file, inside stateDir.
const workspaceIndexName = "workspaces.json"

// stateDir is where brig keeps what has to outlive a single invocation.
// BRIG_STATE_DIR moves it, which is what lets a test run without writing into
// the state of whoever is running the test.
func stateDir() (string, error) {
	if dir := os.Getenv("BRIG_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep brig's state in: %w", err)
	}
	return filepath.Join(home, ".brig"), nil
}

// indexPath is stateDir/name, for one of brig's small bookkeeping files.
func indexPath(name string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// readIndex returns the string map recorded under name, and an empty one for
// every way the read can fail.
//
// A file that is missing, unreadable or corrupt is not worth failing a command
// over: at worst an absent entry costs one restart, which is exactly the
// behaviour of the release before these files existed. Failing instead would
// make a stray file in ~/.brig able to stop every brig command on the host.
func readIndex(name string) map[string]string {
	index := map[string]string{}
	path, err := indexPath(name)
	if err != nil {
		return index
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return index
	}
	if err := json.Unmarshal(blob, &index); err != nil {
		return map[string]string{}
	}
	return index
}

// writeIndex replaces the file named name with the map it is given.
func writeIndex(name string, index map[string]string) error {
	path, err := indexPath(name)
	if err != nil {
		return err
	}
	// 0700 for the directory: it holds nothing secret, but it names host
	// directories of somebody's, and brig's other state is already private.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file and renamed, so a crash mid-write
	// cannot leave a half-parsed map behind -- which would cost every entry in
	// the file rather than one of them.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readWorkspaceIndex and writeWorkspaceIndex are the workspace index's names
// for the shared read and write above.
func readWorkspaceIndex() map[string]string { return readIndex(workspaceIndexName) }
func writeWorkspaceIndex(index map[string]string) error {
	return writeIndex(workspaceIndexName, index)
}

// RememberedWorkspace is the workspace a sandbox was started with, or "" when
// nothing has been recorded for it -- a sandbox created before this index
// existed, or one whose entry has been pruned.
//
// Exported for the two callers outside a run: `brig ls`, which reports the
// directory a running sandbox actually has rather than the one its name would
// derive, and anything that has a sandbox name and no Config.
func RememberedWorkspace(vmName string) string {
	return readWorkspaceIndex()[vmName]
}

// ForgetWorkspace drops a removed sandbox's entry, so the next sandbox to take
// that name starts from the ordinary resolution rather than inheriting a
// directory chosen for a different one.
//
// Errors are dropped rather than returned, the way releaseGatewayIP drops its
// own: a removal that worked must not report a failure because a bookkeeping
// file could not be rewritten.
func ForgetWorkspace(vmName string) {
	index := readWorkspaceIndex()
	if _, ok := index[vmName]; !ok {
		return
	}
	delete(index, vmName)
	_ = writeWorkspaceIndex(index)
}

// rememberWorkspace records the directory this sandbox has just been started
// with, so a later invocation that names none finds it again.
//
// Also called when the running sandbox already matches, which is what fills
// the index in for a sandbox created before it existed: the entry is written
// once, and every invocation after that is a read.
//
// A failure to write is a warning rather than an error. The sandbox is up and
// working, so failing the command here would be refusing the thing that
// succeeded over the note about it -- but the cost lands later and nowhere
// near this line, on some flagless verb that restarts the sandbox, so it is
// worth saying out loud where the reason is still visible.
func (c *Config) rememberWorkspace() {
	index := readWorkspaceIndex()
	if index[c.VMName] == c.Workspace {
		return
	}
	index[c.VMName] = c.Workspace
	if err := writeWorkspaceIndex(index); err != nil {
		c.warnf("could not record %s as the workspace of %s (%v). A later command "+
			"that names no workspace will fall back to the default one and restart "+
			"the sandbox.", c.Workspace, c.VMName, err)
	}
}
