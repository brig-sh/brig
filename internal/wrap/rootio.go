package wrap

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// errPlantedSymlink marks a refusal caused by a symlink inside the workspace
// that leads out of it, so a caller that otherwise shrugs off an I/O error --
// trustGuestCwd treats an unreadable state file as nothing to do -- can tell
// this one apart and fail the run instead of quietly carrying on.
var errPlantedSymlink = errors.New("a symlink in the workspace leads out of it")

// afterWorkspaceCheck runs between the path check and the open. It does
// nothing outside tests; see openWorkspace.
var afterWorkspaceCheck = func() {}

// workspaceRoot is the workspace opened once as an [os.Root]. Every host-side
// read and write brig makes inside the workspace goes through it.
//
// The workspace is the guest's home, mounted read-write, and brig writes into
// it from the host on every invocation -- the marker, the onboarding seed, the
// trust key. So its contents are the sandbox's to choose: plant a symlink
// where brig writes next and brig, which runs as you and outside the sandbox,
// follows it to a host path the guest could never reach. os.Root is the
// control for exactly that, and the only one that holds: it resolves every
// path itself, refuses an absolute symlink outright, and will not let a
// relative one climb past the root, with no window between the check and the
// open for the guest to swap the file in.
//
// The rule for this package is therefore simple: nothing reaches into the
// workspace with a plain os.* call. Reads of the *host's* own files -- the
// user's ~/.claude that seedHostConfig copies from -- are a different matter
// and stay ordinary, because those live outside the workspace by design.
type workspaceRoot struct {
	root *os.Root
	dir  string
}

// openWorkspace creates the workspace if it is not there yet and opens it as a
// root.
func (c *Config) openWorkspace() (*workspaceRoot, error) {
	// The walk below inspects a path *string*; MkdirAll and OpenRoot then
	// resolve that same string again, from scratch. Two resolutions, and the
	// guest owns a component in between them whenever a workspace is nested
	// under a directory a sandbox can write -- which is the case the walk's own
	// doc comment names as the reachable one. Flipping that component between
	// the check and the open redirected everything brig writes next, and one of
	// those things is a mode-0755 credential helper, so the race was worth
	// running: it landed outside the workspace in 43 attempts.
	//
	// Cleaning first because the walk cleans and the callers did not, so
	// `<tmp>/link/../real` was checked as `<tmp>/real` -- skipping `link`
	// entirely -- and then opened as written, through the link.
	c.Workspace = filepath.Clean(c.Workspace)
	if err := c.checkWorkspacePath(); err != nil {
		return nil, err
	}
	// A seam, so the window between the check and the open can be opened
	// deliberately in a test. Without it the identity check below is only
	// reachable by winning a real race, and a guard that can only be tested by
	// racing is a guard that goes untested.
	afterWorkspaceCheck()
	if err := os.MkdirAll(c.Workspace, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(c.Workspace)
	if err != nil {
		return nil, err
	}
	// From here on the root is a directory handle and a later swap of any
	// component cannot move it. What is still open is the window that just
	// closed behind us, so ask whether it was used: walk again, and require
	// that the handle we are holding is the same directory the path names now.
	//
	// Both halves are needed. The walk alone misses a component swapped out and
	// back; the identity check alone misses a component that is still a symlink
	// but happens to point where we expected. Together they say: this handle is
	// the directory that passed the check, and nothing moved underneath it.
	if err := c.checkWorkspacePath(); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := sameDir(root, c.Workspace); err != nil {
		_ = root.Close()
		return nil, err
	}
	return &workspaceRoot{root: root, dir: c.Workspace}, nil
}

// sameDir reports whether an open root still refers to the directory that path
// names.
func sameDir(root *os.Root, path string) error {
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("cannot stat the opened workspace: %w", err)
	}
	named, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", path, err)
	}
	if !os.SameFile(opened, named) {
		return fmt.Errorf("refusing to use %s as the workspace: it changed while brig was "+
			"opening it, so the directory brig holds is not the one you named. Something with "+
			"write access to its parent moved it: %w", path, errPlantedSymlink)
	}
	return nil
}

// verifyStillOurs re-runs the workspace check against an already-open root.
//
// The share handed to the runtime is a path, and the runtime resolves it again
// -- after the image pull, after the VM starts, a window a whole boot wide. A
// path cannot be made immune to that from here, so this narrows it to the
// smallest it can be: check immediately before the handover, holding the
// directory handle that proves what the path meant when brig looked.
func (r *workspaceRoot) verifyStillOurs() error {
	c := &Config{Workspace: r.dir}
	if err := c.checkWorkspacePath(); err != nil {
		return err
	}
	return sameDir(r.root, r.dir)
}

// checkWorkspacePath walks the workspace path a component at a time and
// refuses a symlink in any of them.
//
// Lstat'ing the workspace itself catches a link AT the workspace and nothing
// else, and it is not the last component that matters -- it is every one of
// them. `MkdirAll` and `OpenRoot` resolve the whole path, so a link at any
// level redirects the guest's home just as completely, and the case that makes
// it reachable is ordinary: a workspace nested under another sandbox's
// workspace, or under any directory a guest can write, is a path whose parent
// components the guest chooses. The root cannot see this -- by the time it has
// a directory handle, the redirection has already happened.
//
// Links the machine itself is made of are skipped, by owner: /tmp and /var on
// macOS are symlinks, root-owned, and nothing a sandbox can replace, so
// refusing them would refuse every workspace under a temporary directory for a
// hazard that does not exist. What a guest plants is owned by the user brig
// runs as, and that is what this looks at. (brig running as root therefore
// checks nothing here; running an agent sandbox as root is its own problem,
// and this is not the control that would fix it.)
func (c *Config) checkWorkspacePath() error {
	path := filepath.Clean(c.Workspace)
	var components []string
	for {
		components = append(components, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	// Top down, so the message names the outermost link rather than a deeper
	// one that only exists because of it.
	for i := len(components) - 1; i >= 0; i-- {
		p := components[i]
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || ownedByRoot(info) {
			continue
		}
		target, _ := os.Readlink(p)
		if p == filepath.Clean(c.Workspace) {
			return fmt.Errorf("refusing to use %s as the workspace: it is a symlink to %q, "+
				"and the workspace is mounted read-write as the sandbox's home, so brig will not "+
				"write a guest's home through a link. Point BRIG_WORKSPACE (or --workspace) at the "+
				"real directory: %w", c.Workspace, target, errPlantedSymlink)
		}
		return fmt.Errorf("refusing to use %s as the workspace: %s on the way to it is a "+
			"symlink to %q, so the sandbox's home would be created somewhere other than where "+
			"you asked for it. Point BRIG_WORKSPACE (or --workspace) at the real directory: %w",
			c.Workspace, p, target, errPlantedSymlink)
	}
	return nil
}

// Close releases the directory handle the root holds open.
func (r *workspaceRoot) Close() error { return r.root.Close() }

// path is the host path of a workspace-relative name, for messages only.
// Nothing opens what this returns.
func (r *workspaceRoot) path(rel string) string { return filepath.Join(r.dir, rel) }

// refuse turns a root's own refusal into something the person reading it can
// act on.
//
// "path escapes from parent" is accurate and says nothing about which file, or
// about the fact that a sandbox put it there on purpose. Every other error --
// a missing file, a full disk -- is passed through untouched, so callers that
// test for fs.ErrNotExist keep working.
func (r *workspaceRoot) refuse(op, rel string, err error) error {
	link, target, ok := r.plantedLink(rel)
	if !ok {
		return err
	}
	return r.plantedError(op, rel, link, target)
}

// plantedError is the one message every refusal in this file produces. It says
// which link, where it points, and why brig treats it as the sandbox's doing
// rather than as a file the user is fond of -- because brig writes only
// regular files in there, so a link in the way of one was put there.
func (r *workspaceRoot) plantedError(op, rel, link, target string) error {
	what := fmt.Sprintf("%s is a symlink to %q", r.path(link), target)
	if link == rel {
		what = fmt.Sprintf("it is a symlink to %q", target)
	}
	return fmt.Errorf("refusing to %s %s: %s, and brig writes only regular files inside the "+
		"workspace. The workspace is mounted read-write as the sandbox's home, so that link was "+
		"put there from inside the sandbox, to have brig -- which runs as you, on the host -- "+
		"reach a file the sandbox cannot. Nothing was written; inspect %s and remove it before "+
		"running brig again: %w",
		op, r.path(rel), what, r.path(link), errPlantedSymlink)
}

// plantedLink finds the first component of rel that is a symlink, and reports
// where it points. Lstat and Readlink do not follow the final component, so
// this reads the link itself and never what it aims at.
func (r *workspaceRoot) plantedLink(rel string) (link, target string, ok bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		info, err := r.root.Lstat(prefix)
		if err != nil {
			// Either nothing is there or the walk has already passed the link
			// that stops it; a deeper component cannot be reached to be read.
			return "", "", false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := r.root.Readlink(prefix)
		if err != nil {
			return "", "", false
		}
		return prefix, target, true
	}
	return "", "", false
}

// symlinkRefusal covers the link the root itself would have allowed: one that
// stays inside the workspace. It escapes nothing, but brig still will not
// write through it -- a link where the agent's state file belongs was not put
// there by the agent, and following it silently is how the dangling case
// creates a file wherever it was aimed.
func (r *workspaceRoot) symlinkRefusal(op, rel string) error {
	link, target, ok := r.plantedLink(rel)
	if !ok {
		return fmt.Errorf("refusing to %s %s: it is a symlink, and brig writes only regular "+
			"files inside the workspace: %w", op, r.path(rel), errPlantedSymlink)
	}
	return r.plantedError(op, rel, link, target)
}

func (r *workspaceRoot) readFile(rel string) ([]byte, error) {
	// os.Root refuses a symlink out of the workspace; it opens a FIFO like any
	// other file, and open(2) on a FIFO waits for a writer. A guest that plants
	// one at .claude.json or .gitconfig -- both read on an ordinary run, the
	// first by default -- hangs brig on the host indefinitely, and .gitconfig
	// also parks a `git config -f` child on it that outlives the read.
	//
	// brig's own rule is that it writes only regular files inside the
	// workspace; this holds it to the same rule when reading. The check is
	// before the open, so nothing has blocked yet.
	if fi, err := r.root.Lstat(rel); err != nil {
		return nil, r.refuse("read", rel, err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read %s: the workspace holds %s there, not a "+
			"regular file, and reading it would block brig on the host. Something inside the "+
			"sandbox put it there: %w", r.path(rel), fi.Mode().Type(), errPlantedSymlink)
	}
	blob, err := r.root.ReadFile(rel)
	if err != nil {
		return nil, r.refuse("read", rel, err)
	}
	return blob, nil
}

func (r *workspaceRoot) writeFile(rel string, blob []byte, mode os.FileMode) error {
	if err := r.root.WriteFile(rel, blob, mode); err != nil {
		return r.refuse("write", rel, err)
	}
	return nil
}

func (r *workspaceRoot) mkdirAll(rel string, mode os.FileMode) error {
	if err := r.root.MkdirAll(rel, mode); err != nil {
		return r.refuse("create", rel, err)
	}
	return nil
}

// lstat reports on the entry itself, symlink included, without following it.
func (r *workspaceRoot) lstat(rel string) (os.FileInfo, error) {
	info, err := r.root.Lstat(rel)
	if err != nil {
		return nil, r.refuse("read", rel, err)
	}
	return info, nil
}

// exists answers the "is it already there" question the seeding code asks,
// and treats a symlink as a refusal rather than as an answer: reporting one as
// present skips the write silently, and reporting it as absent writes through
// it. Neither is something to do quietly with a file a sandbox planted.
func (r *workspaceRoot) exists(op, rel string) (bool, error) {
	info, err := r.lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case info.Mode()&os.ModeSymlink != 0:
		return false, r.symlinkRefusal(op, rel)
	}
	return true, nil
}

func (r *workspaceRoot) rename(oldRel, newRel string) error {
	if err := r.root.Rename(oldRel, newRel); err != nil {
		return r.refuse("write", newRel, err)
	}
	return nil
}

func (r *workspaceRoot) symlink(target, rel string) error {
	if err := r.root.Symlink(target, rel); err != nil {
		return r.refuse("create", rel, err)
	}
	return nil
}

func (r *workspaceRoot) openFile(rel string, flag int, mode os.FileMode) (*os.File, error) {
	f, err := r.root.OpenFile(rel, flag, mode)
	if err != nil {
		return nil, r.refuse("write", rel, err)
	}
	return f, nil
}

// writeIfChanged leaves a file alone when it already holds exactly this
// content, so a regenerated-every-run file does not churn its mtime.
func (r *workspaceRoot) writeIfChanged(rel, content string, mode os.FileMode) error {
	// The existence check is what makes a planted link loud here: without it a
	// dangling one reads as "absent" and the write goes through it.
	if _, err := r.exists("write", rel); err != nil {
		return err
	}
	if existing, err := r.readFile(rel); err == nil && string(existing) == content {
		return r.chmod(rel, mode)
	}
	if err := r.writeFile(rel, []byte(content), mode); err != nil {
		return err
	}
	return r.chmod(rel, mode)
}

func (r *workspaceRoot) chmod(rel string, mode os.FileMode) error {
	if err := r.root.Chmod(rel, mode); err != nil {
		return r.refuse("change the mode of", rel, err)
	}
	return nil
}

// createTemp is os.CreateTemp confined to the root, for the write-then-rename
// that keeps the agent's state file intact when a write is cut short. The name
// is unpredictable and O_EXCL fails on anything already at it, symlink
// included, so nothing can be waiting where this lands.
func (r *workspaceRoot) createTemp(prefix string) (*os.File, string, error) {
	for range 10000 {
		name := fmt.Sprintf("%s%08x", prefix, rand.Uint32())
		f, err := r.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", r.refuse("write", name, err)
		}
	}
	return nil, "", fmt.Errorf("cannot create a temporary file in %s", r.dir)
}

func (r *workspaceRoot) remove(rel string) error { return r.root.Remove(rel) }
