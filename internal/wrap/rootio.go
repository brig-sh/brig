package wrap

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// errPlantedSymlink marks a refusal caused by a symlink inside the workspace
// that leads out of it, so a caller that otherwise shrugs off an I/O error --
// trustGuestCwd treats an unreadable state file as nothing to do -- can tell
// this one apart and fail the run instead of quietly carrying on.
var errPlantedSymlink = errors.New("a symlink in the workspace leads out of it")

// afterWorkspaceCheck runs between the path check and the open. It does
// nothing outside tests; see openWorkspace.
var afterWorkspaceCheck = func() {}

// duringWorkspaceWalk runs while the path is being resolved, after the
// ancestors have been dealt with and before the final component. It does
// nothing outside tests.
//
// It exists because the two seams are not the same window. afterWorkspaceCheck
// opens the gap between a finished walk and the open, which the identity
// comparison closes. This one opens the gap *inside* the walk, which is the
// gap an attacker actually gets: a resolution that re-reads the prefix can be
// poisoned before it reaches the leaf, and then everything downstream agrees
// with itself about the wrong directory.
var duringWorkspaceWalk = func() {}

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
	// ident is the directory the path check ended on, captured while the path
	// was known to be symlink-free. Everything that asks "is this still our
	// workspace?" compares against this, never against a fresh resolution of
	// the path.
	ident os.FileInfo
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

	// Refuse a planted link BEFORE creating anything. Creating first and
	// checking after would still be safe in the sense that the run stops, but
	// it would leave a directory made through the attacker's link, and "the
	// refusal comes before any directory is made" is a property worth keeping.
	//
	// This pass tolerates a component that does not exist yet -- MkdirAll is
	// about to create it -- and refuses a symlink or any other stat error.
	if err := c.checkWorkspacePathBeforeCreate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Workspace, 0o755); err != nil {
		return nil, err
	}

	// Walk down by handle rather than by path. Each step names one component
	// relative to a directory that is already open, so no step re-reads the
	// components above it, and there is no longer a resolution for an attacker
	// to poison between the check and its result.
	//
	// Two earlier versions of this guard were defeated by variations of one
	// trick. The first compared the open root against os.Stat(path), and
	// os.Stat re-resolves from scratch, so a symlink present during the open
	// and absent during the comparison made both sides agree. The second
	// captured the identity during the walk, which was better but not enough:
	// the walk itself resolved a string per component, so flipping a parent
	// between the last two steps poisoned the captured value, and the
	// comparison then had two wrong answers to agree about. Descending by
	// handle removes the class rather than the instance.
	root, want, err := openWorkspaceHandle(c.Workspace)
	if err != nil {
		return nil, err
	}

	// A seam, so a test can swap the directory after brig is holding it. The
	// swap is meant to do nothing now: the handle references the directory it
	// opened, not the name it was reached by.
	afterWorkspaceCheck()

	return &workspaceRoot{root: root, dir: c.Workspace, ident: want}, nil
}

// openWorkspaceHandle opens the workspace by descending one component at a
// time, and hands back the directory it ended on together with that
// directory's identity.
//
// The rule at every step is the same: look at one name inside a directory brig
// already holds open, refuse it if it is a symlink, then open it and repeat.
// Because each name is a single component resolved against an open descriptor,
// nothing above it is read again. A component swapped after brig has passed it
// cannot change where brig ends up, and one swapped before it is passed is seen
// for what it is.
//
// A symlink owned by root is followed rather than refused, which is what lets
// this work on macOS at all: /var is a link to private/var and /tmp is a link
// to it, so every temporary directory on the platform sits behind one. A guest
// writes as the user brig runs as and cannot create a root-owned link in a
// directory it can reach, so the exception costs nothing it protects.
func openWorkspaceHandle(path string) (*os.Root, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("refusing to use %s as the workspace: %w", path, err)
	}
	components := workspaceComponents(abs)

	root, err := os.OpenRoot(components[0])
	if err != nil {
		return nil, nil, fmt.Errorf("refusing to use %s as the workspace: cannot open %s: %w",
			abs, components[0], err)
	}
	for i, p := range components[1:] {
		if i == len(components)-2 {
			duringWorkspaceWalk()
		}
		name := filepath.Base(p)
		info, err := root.Lstat(name)
		if err != nil {
			// A component that is not there is a component being moved. The
			// leaf was created moments ago and every parent had to exist for
			// that to work, so absence here is not a race brig can wait out.
			_ = root.Close()
			return nil, nil, fmt.Errorf("refusing to use %s as the workspace: %s could not be "+
				"read while brig was checking the path (%v), so something is moving it: %w",
				abs, p, err, errPlantedSymlink)
		}
		if info.Mode()&os.ModeSymlink != 0 && !ownedByRoot(info) {
			target, _ := root.Readlink(name)
			_ = root.Close()
			if p == abs {
				return nil, nil, fmt.Errorf("refusing to use %s as the workspace: it is a symlink "+
					"to %q, and the workspace is mounted read-write as the sandbox's home, so brig "+
					"will not write a guest's home through a link. Point BRIG_WORKSPACE (or "+
					"--workspace) at the real directory: %w", abs, target, errPlantedSymlink)
			}
			return nil, nil, fmt.Errorf("refusing to use %s as the workspace: %s on the way to it "+
				"is a symlink to %q, so the sandbox's home would be created somewhere other than "+
				"where you asked for it. Point BRIG_WORKSPACE (or --workspace) at the real "+
				"directory: %w", abs, p, target, errPlantedSymlink)
		}
		next, err := root.OpenRoot(name)
		_ = root.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("refusing to use %s as the workspace: cannot open %s: %w",
				abs, p, err)
		}
		root = next
	}

	ident, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("cannot stat the opened workspace %s: %w", abs, err)
	}

	// The descent is holding the right directory. The path is a separate
	// question, and it is the one that matters downstream: the share handed to
	// the runtime is a string, resolved in another process later, so a path
	// that has stopped naming this directory is a path brig must not pass on.
	//
	// Resolving it again here is safe in a way the old walk was not. The value
	// it is checked against came from the descent, so a poisoned resolution can
	// only disagree, and disagreement only ever refuses. There is no reading of
	// this that lets a swapped component be accepted.
	byPath, err := os.Stat(abs)
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("refusing to use %s as the workspace: it could not be read "+
			"after brig opened it (%v), so something is moving it: %w", abs, err, errPlantedSymlink)
	}
	if !os.SameFile(byPath, ident) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("refusing to use %s as the workspace: the name stopped "+
			"pointing at the directory brig opened, so the sandbox would be handed somewhere "+
			"else as its home: %w", abs, errPlantedSymlink)
	}
	return root, ident, nil
}

// rootIs reports whether an open root refers to the directory the walk ended
// on.
//
// `want` must come from checkWorkspacePath and nowhere else. Comparing against
// a fresh os.Stat of the path is what made the previous version useless: that
// re-resolves, so an attacker who can flip a component controls both sides of
// the comparison.
func rootIs(root *os.Root, want os.FileInfo, path string) error {
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("cannot stat the opened workspace: %w", err)
	}
	if !os.SameFile(opened, want) {
		return fmt.Errorf("refusing to use %s as the workspace: it changed between the check "+
			"and the open, so the directory brig holds is not the one it checked. Something "+
			"with write access to a parent directory moved it: %w", path, errPlantedSymlink)
	}
	return nil
}

// verifyStillOurs re-walks the path and reports whether it still names the
// directory this root holds.
//
// This is NOT a narrow window. The share handed to the runtime is a path
// string, and hull resolves it on its own time -- after the image digest is
// resolved, after the pull, after the VM starts. On a cold image that is
// minutes. An earlier comment here claimed this moved the window "from the
// whole boot to two statements", which was wrong: it moves nothing, because
// the resolution that matters happens in another process later.
//
// What it does buy is that brig refuses to hand over a path that has ALREADY
// stopped meaning what it checked. That is worth doing and worth being honest
// about: closing the rest needs hull to accept a descriptor rather than a path.
func (r *workspaceRoot) verifyStillOurs() error {
	c := &Config{Workspace: r.dir}
	want, err := c.checkWorkspacePath()
	if err != nil {
		return err
	}
	if !os.SameFile(want, r.ident) {
		return fmt.Errorf("refusing to hand %s to the runtime: it no longer names the "+
			"directory brig prepared, so the sandbox would get somewhere else as its home: %w",
			r.dir, errPlantedSymlink)
	}
	return rootIs(r.root, r.ident, r.dir)
}

// checkWorkspacePathBeforeCreate walks the path before the workspace exists.
//
// It is the same refusal as checkWorkspacePath with one difference: a component
// that is simply absent is fine here, because MkdirAll is about to create it.
// Everything else -- a symlink, or a stat that fails for any other reason -- is
// a refusal, so nothing is ever created through a planted link.
func (c *Config) checkWorkspacePathBeforeCreate() error {
	for _, p := range workspaceComponents(c.Workspace) {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("refusing to use %s as the workspace: %s could not be read "+
				"(%v): %w", c.Workspace, p, err, errPlantedSymlink)
		}
		if info.Mode()&os.ModeSymlink == 0 || ownedByRoot(info) {
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

// workspaceComponents returns the path and every ancestor, outermost first, so
// a message names the outermost link rather than a deeper one that only exists
// because of it.
func workspaceComponents(workspace string) []string {
	path := filepath.Clean(workspace)
	var components []string
	for {
		components = append(components, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	for i, j := 0, len(components)-1; i < j; i, j = i+1, j-1 {
		components[i], components[j] = components[j], components[i]
	}
	return components
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
func (c *Config) checkWorkspacePath() (os.FileInfo, error) {
	root, ident, err := openWorkspaceHandle(c.Workspace)
	if err != nil {
		return nil, err
	}
	_ = root.Close()
	return ident, nil
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
	f, err := r.openRegular(rel, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	blob, err := io.ReadAll(f)
	if err != nil {
		return nil, r.refuse("read", rel, err)
	}
	return blob, nil
}

// openRegular opens an entry inside the workspace and refuses anything that is
// not a regular file, deciding from the OPEN DESCRIPTOR rather than from a
// prior Lstat.
//
// Two bugs live in the difference. Lstat-then-open is a TOCTOU: a guest that
// renames a FIFO over a regular file between the two wins, and it was won in
// about three seconds. And Lstat answers about the LINK, so a .gitconfig
// symlinked to a file INSIDE the workspace -- which os.Root has always allowed,
// and which a person may reasonably have done to their own dotfiles -- was
// refused, with a message claiming it "leads out of" the workspace when it does
// not.
//
// The fstat is what does the work. O_NONBLOCK is kept as belt-and-braces and
// is NOT load-bearing here: Go opens a FIFO without blocking anyway (the
// runtime opens non-blocking and registers the descriptor with the poller), so
// the open returns and the type can be inspected; the block that hung brig came
// at the first read, inside os.Root.ReadFile. Verified by removing O_NONBLOCK,
// which changes no test outcome. It stays so the behaviour does not silently
// depend on that runtime detail, and on a regular file it costs nothing.
//
// Symlinks are left to os.Root, which already refuses the ones that escape.
func (r *workspaceRoot) openRegular(rel string, flag int) (*os.File, error) {
	f, err := r.root.OpenFile(rel, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, r.refuse("read", rel, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, r.refuse("read", rel, err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("refusing to use %s: the workspace holds a %s there, not a "+
			"regular file, and brig would block on the host reading or writing it. Something "+
			"inside the sandbox put it there: %w",
			r.path(rel), describeType(fi.Mode()), errPlantedSymlink)
	}
	return f, nil
}

// describeType names a file type in words. fs.FileMode.Type() renders as
// "L---------", which is not something to put in front of a person.
func describeType(m os.FileMode) string {
	switch {
	case m&os.ModeNamedPipe != 0:
		return "named pipe (FIFO)"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device node"
	case m&os.ModeDir != 0:
		return "directory"
	case m&os.ModeSymlink != 0:
		return "symlink"
	}
	return m.Type().String()
}

func (r *workspaceRoot) writeFile(rel string, blob []byte, mode os.FileMode) error {
	// The write path needs the same rule as the read path, and did not have it.
	// os.Root.WriteFile opens O_WRONLY|O_CREATE|O_TRUNC, and on a FIFO that
	// blocks forever waiting for a reader -- so one `mkfifo .brig-workspace`
	// inside the sandbox hung `brig run`, `brig shell` and `brig exec` on the
	// host, with no output and no way for the user to tell why. No race
	// required.
	//
	// O_CREATE|O_EXCL first, which is the common case and is atomic; if the
	// entry already exists, re-open it through openRegular so a non-regular
	// file is refused instead of blocked on.
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return r.refuse("write", rel, err)
		}
		f, err = r.openRegular(rel, os.O_WRONLY|os.O_TRUNC)
		if err != nil {
			return err
		}
	}
	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		return r.refuse("write", rel, err)
	}
	if err := f.Close(); err != nil {
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
