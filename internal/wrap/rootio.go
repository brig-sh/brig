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

// errPlantedSymlink marks a refusal because the workspace, or the path to it,
// cannot be trusted: a symlink inside it that leads out, one on the way to it
// that the guest could have planted, or a component that moved while brig was
// looking. A caller that otherwise shrugs off an I/O error -- trustGuestCwd
// treats an unreadable state file as nothing to do -- can tell this one apart
// and fail the run instead of quietly carrying on. A plain configuration
// error, such as a regular file where a directory was named, does not carry
// it.
var errPlantedSymlink = errors.New("a symlink leads out of a directory brig is checking")

// afterWorkspaceCheck runs after the workspace has been opened and before the
// root is handed back. It does nothing outside tests; see openWorkspace.
var afterWorkspaceCheck = func() {}

// duringPathWalk runs at the start of the last step of the descent, before
// the directory itself is looked at: the workspace or the project, since both
// take this walk. It does nothing outside tests.
//
// This is the gap an attacker gets against a walk that resolves strings: a
// parent flipped here poisons everything a by-name walk does next, and then
// everything downstream agrees with itself about the wrong directory. Against
// the descent it is meant to do nothing, and the test that drives it says so.
var duringPathWalk = func() {}

// betweenLstatAndOpen runs inside one step of the descent, after the entry has
// been looked at and before it is opened, with the entry's name. It does
// nothing outside tests. This is the one window a step has, and the identity
// check after the open is what closes it.
var betweenLstatAndOpen = func(name string) {}

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
	heldDir
}

// pathSubject is the role a descent opens a directory for, in the words its
// refusals use.
//
// The walk is shared between the workspace and the project. The subject is
// what differs: what the directory is for, why a link there is refused, and
// what to do instead.
type pathSubject struct {
	// what completes "refusing to use <path> as ...".
	what string
	// leafWhy and pathWhy say why a link is refused at the end of the path and
	// on the way to it. The reader's mistake differs: a link they named
	// themselves, or one they walked through without knowing.
	leafWhy string
	pathWhy string
	// remedy tells the reader what to do instead.
	remedy string
}

var workspaceSubject = pathSubject{
	what: "the workspace",
	leafWhy: "the workspace is mounted read-write as the sandbox's home, so brig will not " +
		"write a guest's home through a link",
	pathWhy: "the sandbox's home would be created somewhere other than where you asked for it",
	remedy:  "Point --home (or BRIG_WORKSPACE) at the real directory",
}

// projectSubject is the same standard applied to the directory named on the
// command line.
//
// The project is mounted read-write, so every component at or below it is the
// sandbox's to replace. brig reads those components on the next run that names
// a path through them, running as the user and outside the sandbox. Refusing a
// link there keeps the directory the operator named and the directory the VMM
// exports the same one.
var projectSubject = pathSubject{
	what: "this run's project",
	leafWhy: "the project is mounted read-write into the sandbox, so brig will not hand a " +
		"guest a directory reached through a link",
	pathWhy: "the sandbox would be handed a directory other than the one you named",
	remedy:  "Name the real directory instead",
}

// refusef builds this subject's refusal for root, with whatever detail the
// descent found. The path and the role lead every one of them, so they are
// composed here.
func (s pathSubject) refusef(root, format string, a ...any) error {
	return fmt.Errorf("refusing to use %s as %s: "+format, append([]any{root, s.what}, a...)...)
}

// heldDir is a host directory reached by the descent below and held open.
// Every read-write share brig hands a runtime is made from one.
//
// The handle references the directory that was opened, not the name it was
// reached by, so it cannot be redirected after the descent. Every later check
// compares against it.
type heldDir struct {
	root *os.Root
	dir  string
	// ident is the directory the descent ended on, read from the handle it
	// holds. Before the path is handed to the runtime, verifyStillOurs resolves
	// it afresh and compares against this: a resolution can only disagree with
	// the handle, and disagreement refuses.
	ident os.FileInfo
	// subj is the role it was opened for, so a refusal raised after the descent
	// reads the same way as one raised during it.
	subj pathSubject
}

// openHeldDir reaches dir by the descent and holds it, for the role subj names.
func openHeldDir(dir string, subj pathSubject) (*heldDir, error) {
	root, ident, err := openPathHandle(dir, subj)
	if err != nil {
		return nil, err
	}
	return &heldDir{root: root, dir: dir, ident: ident, subj: subj}, nil
}

// openWorkspace creates the workspace if it is not there yet and opens it as a
// root.
func (c *Config) openWorkspace() (*workspaceRoot, error) {
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
	// comparison then had two wrong answers to agree about. Walking the part
	// of the path the guest can reach by handle removes the class rather than
	// the instance; see openPathHandle for where that part begins.
	root, want, err := openPathHandle(c.Workspace, workspaceSubject)
	if err != nil {
		return nil, err
	}

	// A seam, so a test can swap the directory after brig is holding it. The
	// swap is meant to do nothing now: the handle references the directory it
	// opened, not the name it was reached by.
	afterWorkspaceCheck()

	return &workspaceRoot{heldDir{root: root, dir: c.Workspace, ident: want,
		subj: workspaceSubject}}, nil
}

// openWorkspaceHandle opens the workspace and hands back the directory it
// ended on together with that directory's identity.
//
// It splits the path in two at the point where an attacker could start acting
// on it. The guest writes as the user brig runs as. That user can rename an
// entry out of a directory it can write and plant a link in its place, and can
// do nothing of the kind to a directory it cannot write. So every component
// whose parent is not writable by us is one the guest cannot redirect, and the
// longest such prefix is opened by name in one call, letting the kernel resolve
// it and follow whatever links the system or an administrator put there. That
// needs only search permission on the way, which is what the shared-host
// layouts with a 0711 /home rely on.
//
// The rest of the path, from the first directory we can write, is where a swap
// is possible, and it is walked one component at a time against the directory
// already open above it. Each step looks at one name, refuses it if it is a
// symlink, then opens it and repeats, so nothing above a component is read
// again and there is no resolution left for a swap to poison. Every symlink in
// this part is refused, whoever owns it: a root-owned link here would have to
// have been placed inside a directory the user controls, which is not a layout
// worth following blind. The workspace itself is always in this part, since it
// is the string handed to the runtime as the share, so a link there is refused
// wherever it sits.
//
// Whether a directory is ours to write is the kernel's answer, plus ownership;
// see writableBy, which also says what the rule degrades to as root.
func openPathHandle(path string, subj pathSubject) (*os.Root, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, subj.refusef(path, "%w", err)
	}
	components := pathComponents(abs)

	k, err := trustedPrefix(components)
	if err != nil {
		return nil, nil, subj.refusef(abs, "%w", err)
	}
	prefix, tail := components[k], components[k+1:]

	root, err := os.OpenRoot(prefix)
	if err != nil {
		return nil, nil, subj.refusef(abs, "cannot open %s: %w", prefix, err)
	}
	for i, p := range tail {
		if i == len(tail)-1 {
			duringPathWalk()
		}
		name := filepath.Base(p)
		info, err := root.Lstat(name)
		if err != nil {
			// A component that is not there is a component being moved. The
			// leaf was created moments ago and every parent had to exist for
			// that to work, so absence here is not a race brig can wait out.
			_ = root.Close()
			return nil, nil, subj.refusef(abs, "%s could not be "+
				"read while brig was checking the path (%v), so something is moving it: %w", p, err, errPlantedSymlink)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := root.Readlink(name)
			_ = root.Close()
			return nil, nil, plantedSymlinkErr(subj, abs, p, target)
		}
		if !info.IsDir() {
			// Only a directory can be on the way to the workspace. The check
			// matters for what it refuses before the open below, not for
			// tidiness: os.Root.OpenRoot blocks on a fifo until a writer
			// arrives, so a component that is one stops the boot rather than
			// failing it. resolveTrusted and checkWorkspacePathBeforeCreate
			// both make this check already; the descent was the one that did
			// not.
			_ = root.Close()
			return nil, nil, subj.refusef(abs, "%s is %s, "+
				"not a directory", p, describeType(info.Mode()))
		}
		betweenLstatAndOpen(name)
		// One open, with O_DIRECTORY, and the root is built from the
		// descriptor it returns.
		//
		// O_DIRECTORY is what stops a fifo. Between the look above and this
		// open the entry can be swapped, and opening a fifo waits for a writer
		// with nothing to time it out, so a guest that can swap a parent could
		// stop the boot rather than redirect it. The kernel refuses a
		// non-directory at open time, so whatever the swap puts there cannot
		// block us.
		//
		// Carrying on from the descriptor is what keeps the name from being
		// looked up twice. Opening it again to get a root would put a second
		// lookup in the middle of a walk that exists to stop a name being
		// resolved more than once. See rootFromFile.
		opening, err := root.OpenFile(name, syscall.O_DIRECTORY, 0)
		if err != nil {
			_ = root.Close()
			return nil, nil, subj.refusef(abs, "%s could not be "+
				"opened as a directory (%v), so something changed it while brig was "+
				"looking: %w", p, err, errPlantedSymlink)
		}
		next, err := rootFromFile(opening)
		_ = opening.Close()
		_ = root.Close()
		if err != nil {
			return nil, nil, subj.refusef(abs, "cannot open %s: %w", p, err)
		}
		// The look and the open are two calls, and os.Root follows a link that
		// stays under the root, so a link swapped in between them was followed
		// just now. The handle cannot be redirected after the fact, though: ask
		// it what it holds and refuse anything but the directory the look saw.
		opened, err := next.Stat(".")
		if err != nil {
			_ = next.Close()
			return nil, nil, fmt.Errorf("cannot stat %s after opening it: %w", p, err)
		}
		if !os.SameFile(opened, info) {
			_ = next.Close()
			return nil, nil, subj.refusef(abs, "%s changed between "+
				"the look and the open, so something is moving it: %w", p, errPlantedSymlink)
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
		return nil, nil, subj.refusef(abs, "it could not be read "+
			"after brig opened it (%v), so something is moving it: %w", err, errPlantedSymlink)
	}
	if !os.SameFile(byPath, ident) {
		_ = root.Close()
		return nil, nil, subj.refusef(abs, "the name stopped "+
			"pointing at the directory brig opened, so the sandbox would be handed a "+
			"directory other than the one brig checked: %w", errPlantedSymlink)
	}
	return root, ident, nil
}

// trustedPrefix returns the index of the last component an attacker cannot
// redirect: for every i up to it, opening components[i] by name resolves
// through directories none of which is ours to write.
//
// Trust runs from the top and never resumes. Once a directory we can write is
// reached, every entry below it can be swapped out wholesale at that level,
// however the lower directories are owned, so the walk stops there for good.
//
// The judgement is made on the directories the kernel will pass through, not
// on the spelling. A link met on the way is resolved here by the same rule, so
// a root-owned /data that points into a user's home ends trust above /data
// rather than extending it through the home: the link itself cannot be
// touched, but the entry it names can, and a by-name open would resolve
// through it. The link is then refused by the walk below, like any other link
// in territory the guest can reach, and the message says to name the real
// directory instead.
//
// The workspace itself is never part of the prefix. It is the string handed to
// the runtime as the share, resolved by another process later, so a link
// there is refused wherever it sits. A component that does not exist yet ends
// the prefix too: it is about to be created.
//
// As root, ownership cannot tell the machine's directories from ones a sandbox
// wrote, because a sandbox run as root writes to the host as root. Trust then
// stops at the entries of /, which brig never mounts. The system's own links
// there, such as /tmp or /home, still resolve, and every component below them
// is walked link by link.
func trustedPrefix(components []string) (int, error) {
	k, dir := 0, components[0]
	for i := 1; i < len(components)-1; i++ {
		if i > 1 && runningAsRoot() {
			break
		}
		real, ok, err := resolveTrusted(dir, filepath.Base(components[i]))
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		k, dir = i, real
	}
	return k, nil
}

// resolveTrusted follows name inside dir the way the kernel would and returns
// the real directory it lands on. It reports ok=false the moment the
// resolution passes through a directory this user can write, or reaches an
// entry that is not there. dir must itself be real, which is what lets a
// relative link target and a ".." be applied to it lexically.
//
// Everything here is checked by name, which is fine for the same reason the
// prefix is opened by name: every directory looked at has just been found to
// be one the guest cannot write, so there is nothing for a race to swap.
func resolveTrusted(dir, name string) (real string, ok bool, err error) {
	names := []string{name}
	hops := 0
	for len(names) > 0 {
		n := names[0]
		names = names[1:]
		switch n {
		case "", ".":
			continue
		case "..":
			dir = filepath.Dir(dir)
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			return "", false, fmt.Errorf("%s could not be read while brig was checking the "+
				"path (%v): %w", dir, err, errPlantedSymlink)
		}
		if dirWritableByUs(dir, info) {
			return "", false, nil
		}
		p := filepath.Join(dir, n)
		entry, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("%s could not be read while brig was checking the "+
				"path (%v): %w", p, err, errPlantedSymlink)
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 40 {
				return "", false, fmt.Errorf("%s: too many levels of symbolic links", p)
			}
			target, err := os.Readlink(p)
			if err != nil {
				return "", false, fmt.Errorf("%s could not be read while brig was checking "+
					"the path (%v): %w", p, err, errPlantedSymlink)
			}
			if filepath.IsAbs(target) {
				dir = filepath.VolumeName(target) + string(filepath.Separator)
			}
			names = append(strings.Split(filepath.ToSlash(filepath.Clean(target)), "/"), names...)
			continue
		}
		if !entry.IsDir() {
			return "", false, fmt.Errorf("%s is not a directory", p)
		}
		dir = p
	}
	return dir, true, nil
}

// verifyStillOurs reports whether the path still names the directory this
// root holds.
//
// This is NOT a narrow window. The share handed to the runtime is a path
// string, and hull resolves it on its own time -- after the image digest is
// resolved, after the pull, after the VM starts. On a cold image that is
// minutes. An earlier comment here claimed this moved the window "from the
// whole boot to two statements", which was wrong: it moves nothing, because
// the resolution that matters happens in another process later.
//
// What it does buy is that brig refuses to hand over a path that has ALREADY
// stopped meaning what it checked. Resolving the path afresh is safe here
// because the value it is compared with came from the handle: a poisoned
// resolution can only disagree, and disagreement only refuses. Closing the
// rest needs hull to accept a descriptor rather than a path.
func (r *heldDir) verifyStillOurs() error {
	now, err := os.Stat(r.dir)
	if err != nil {
		return fmt.Errorf("refusing to hand %s to the runtime: it could not be read (%v), so "+
			"something is moving it: %w", r.dir, err, errPlantedSymlink)
	}
	if !os.SameFile(now, r.ident) {
		return fmt.Errorf("refusing to hand %s to the runtime: it no longer names the "+
			"directory brig checked, so the sandbox would be handed somewhere else as %s: %w",
			r.dir, r.subj.what, errPlantedSymlink)
	}
	return nil
}

// checkWorkspacePathBeforeCreate walks the path before the workspace exists.
//
// It is the same refusal as openPathHandle with one difference: a
// component that is simply absent is fine here, because MkdirAll is about to
// create it. A symlink below the trusted prefix, or a stat that fails for any
// other reason, is a refusal, so nothing is ever created through a planted
// link. It walks by string, which leaves one thing on the table: an empty
// directory created through a link planted between this pass and the descent,
// in a directory the attacker could write anyway, before the run is refused.
func (c *Config) checkWorkspacePathBeforeCreate() error {
	subj := workspaceSubject
	components := pathComponents(c.Workspace)
	workspace := components[len(components)-1]
	k, err := trustedPrefix(components)
	if err != nil {
		return subj.refusef(workspace, "%w", err)
	}
	for _, p := range components[k+1:] {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return subj.refusef(workspace, "%s could not be read "+
				"(%v): %w", p, err, errPlantedSymlink)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			return plantedSymlinkErr(subj, workspace, p, target)
		}
		if !info.IsDir() {
			return subj.refusef(workspace, "%s is not a directory", p)
		}
	}
	return nil
}

// plantedSymlinkErr is the refusal for a symlink at or on the way to the
// directory subj names, in the part of the path the guest can reach. Both
// walks of the workspace produce it, so the advice stays the same whether or
// not the workspace existed yet.
func plantedSymlinkErr(subj pathSubject, root, component, target string) error {
	if component == root {
		return subj.refusef(root, "it is a symlink to %q, and %s. %s: %w",
			target, subj.leafWhy, subj.remedy, errPlantedSymlink)
	}
	return subj.refusef(root, "%s on the way to it is a symlink to %q, so %s. %s: %w",
		component, target, subj.pathWhy, subj.remedy, errPlantedSymlink)
}

// pathComponents returns the path and every ancestor, outermost first, so a
// message names the outermost link rather than a deeper one that only exists
// because of it.
func pathComponents(dir string) []string {
	path := filepath.Clean(dir)
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

// Close releases the directory handle the root holds open.
func (r *heldDir) Close() error { return r.root.Close() }

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
