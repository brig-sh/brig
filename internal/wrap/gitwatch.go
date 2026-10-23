package wrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/brig-sh/brig/internal/notice"
)

// The project is mounted read-write with its .git, and git on the host runs
// programs the repository names: a hook in .git/hooks, core.fsmonitor and the
// other commands .git/config can name. So a file the guest writes there runs on
// the host, as the user, at their next git command in that directory, and
// neither git diff nor git status shows it, because .git is not part of the
// working tree. See docs/security.md, "What the host runs from the project".
//
// brig cannot stop the write without taking .git away from the agent, which
// then cannot commit. What it can do is notice. Each command that reaches a
// session's sandbox records what those files hold, and the next one compares
// and names what changed, before the user is likely to run git on them.
//
// It is a warning and never a refusal, and it is a look between two commands,
// not a guard: the guest can write after the look, and a foreground run hands
// the terminal over with exec, so brig is not there when it ends. The next
// brig command for the session is the earliest point brig can say anything.

// gitWatchIndexName is the state file that holds the last look at each
// sandbox's project, keyed by sandbox.
const gitWatchIndexName = "git-watch.json"

// gitWatchEntry is one sandbox's last look: the project it was taken in, and
// what each watched path held then. See describeGitEntry for the values.
type gitWatchEntry struct {
	Project string            `json:"project"`
	Files   map[string]string `json:"files"`
}

// The bounds of one look. A guest can make .git as large as it likes, and a
// look that read all of it would let the guest stall every brig command.
//
// Three bounds, because any one of them alone leaves the stall: a count of
// entries, the bytes hashed of one file, and the bytes hashed in the whole
// look, which is what keeps 4096 files of 8 MiB from being 32 GiB a command.
// Within them a change is seen. Past them a file is recorded by its mode and
// size only, so a change that keeps both, or one past the first
// maxGitWatchHash bytes of a file, is not; gitWatchTruncated marks a look that
// ran out of either the entries or the bytes. A hook is a few hundred bytes,
// so a real one is never near any of this.
//
// A file is hashed whole or not at all. Hashing what was left of the budget
// would make the hash of a file depend on the files before it, so a guest that
// shrank one of those would get an untouched file named as changed whenever
// the look ran short. Whether a file is hashed still depends on them: past the
// budget, an earlier file that shrinks or grows enough moves a later one
// between hashed and not hashed, and that untouched file is named as changed.
// That only over-warns, and only after the guest changed something, so it is
// left as it is.
const (
	maxGitWatchEntries = 4096
	maxGitWatchHash    = 8 << 20
	gitWatchTruncated  = ".git/…"
)

// maxGitWatchBytes is the bytes one look may hash in all. A variable so a test
// can spend it without writing 64 MiB, which also means a test that sets it
// must not run in parallel with another look.
var maxGitWatchBytes int64 = 64 << 20

// The paths git reads programs from. config.worktree joins config under
// extensions.worktreeConfig, and info holds attributes, which select the
// filter and diff drivers config names. commondir moves the rest: git reads
// config, hooks and info from the directory it names instead, which none of
// the others would show. Submodules keep theirs under .git/modules and are
// not looked at.
var (
	gitWatchFiles = []string{"config", "config.worktree", "commondir"}
	gitWatchDirs  = []string{"hooks", "info"}
)

// CheckProjectGit compares the project's .git with the last look this sandbox
// took, warns about what changed, and records the new look.
//
// The baseline moves only when the warning was printed. -q and --json silence
// warnings, and a change they swallowed has to be named by the next command
// that can be heard, or nobody hears it at all.
//
// Nothing here fails a command. A project that cannot be read is said once and
// skipped: the warning is worth having, but not worth refusing a run over.
func (c *Config) CheckProjectGit() {
	if c.VMName == "" {
		return
	}
	index := readIndex[gitWatchEntry](gitWatchIndexName)
	prev, had := index[c.VMName]
	project := c.Project
	if project == "" {
		project = prev.Project
	}
	if project == "" {
		return
	}
	// The project the sandbox had is the one the guest could write. A run that
	// moves the session to another project compares the old one first.
	//
	// A project that is gone since has nothing left for git to run, and saying
	// so on every command would be a warning nobody can act on.
	if _, err := os.Lstat(prev.Project); had && prev.Project != "" && !errors.Is(err, fs.ErrNotExist) {
		now, err := snapshotProjectGit(prev.Project)
		if err != nil {
			// The error can name a path under .git, which the guest chose.
			c.warnf("%s", notice.Newf("could not read %s/.git to see what the sandbox changed there", prev.Project).
				Note("%s", guestText(err.Error())))
			return
		}
		changes := diffGitWatch(prev.Files, now)
		if len(changes) > 0 {
			if c.Verbosity < Normal {
				return
			}
			c.warnf("%s", gitChangeNotice(c.VMName, prev.Project, changes))
		}
		c.commondirNotice(prev.Project, now)
		if prev.Project == project {
			if len(changes) > 0 {
				c.recordGitWatch(index, project, now)
			}
			return
		}
	}
	// The same goes for the project this command would look at: a session
	// whose remembered project was deleted keeps the path, and the descent
	// reads a missing component as something moving it. There is nothing left
	// to look at, so the look is dropped rather than retried and refused on
	// every command.
	if _, err := os.Lstat(project); errors.Is(err, fs.ErrNotExist) {
		if had {
			forgetGitWatch(func(name string) bool { return name != c.VMName })
		}
		return
	}
	now, err := snapshotProjectGit(project)
	if err != nil {
		c.warnf("%s", notice.Newf("could not read %s/.git, so brig cannot say later what the sandbox changed there", project).
			Note("%s", guestText(err.Error())))
		return
	}
	c.commondirNotice(project, now)
	c.recordGitWatch(index, project, now)
}

// commondirNotice says that a project's .git has a commondir, on every look
// that finds one. git then reads config, hooks and info from the directory it
// names, which brig does not look at, so the four paths it does look at can
// stay the same while what git runs changes. A repository's own .git has no
// commondir: git writes one only in a worktree's directory under
// .git/worktrees, so one here is worth saying each time rather than once.
func (c *Config) commondirNotice(project string, files map[string]string) {
	if _, ok := files[".git/commondir"]; !ok {
		return
	}
	c.warnf("%s", notice.Newf("%s/.git has a commondir, so git reads its config and hooks from where that points", project).
		Note("brig does not look there, and a repository's own .git has no commondir").
		Do("read where it points", "cat .git/commondir"))
}

func (c *Config) recordGitWatch(index map[string]gitWatchEntry, project string, files map[string]string) {
	index[c.VMName] = gitWatchEntry{Project: project, Files: files}
	if err := writeIndex(gitWatchIndexName, index); err != nil {
		c.warnf("%s", notice.Newf("could not record what %s/.git holds: %v", project, err).
			Note("the next command cannot say what the sandbox changed there"))
	}
}

// forgetGitWatch drops the entries of every sandbox the keep function turns
// down. A sandbox created again under a removed one's name would otherwise be
// compared against a look at a project it never had.
func forgetGitWatch(keep func(vmName string) bool) {
	index := readIndex[gitWatchEntry](gitWatchIndexName)
	dropped := false
	for name := range index {
		if !keep(name) {
			delete(index, name)
			dropped = true
		}
	}
	if dropped {
		_ = writeIndex(gitWatchIndexName, index)
	}
}

// gitChangeNotice names what changed, and what to do before the next git
// command. A list is cut short, because the heading and the advice are the
// part that has to be read.
func gitChangeNotice(vmName, project string, changes []string) *notice.Block {
	b := notice.Newf("%s/.git changed while %s could write to it", project, vmName).
		Note("git on the host runs these files as you, at your next git command there")
	const shown = 10
	for i, ch := range changes {
		if i == shown {
			b.Note("and %d more", len(changes)-shown)
			break
		}
		b.Note("%s", ch)
	}
	return b.Do("read them with commands that run nothing", "git config --local --list; ls -la .git/hooks",
		"or continue from a fresh clone")
}

// diffGitWatch lists what differs between two looks, one line per path.
func diffGitWatch(before, after map[string]string) []string {
	var out []string
	for p, was := range before {
		now, ok := after[p]
		switch {
		case !ok:
			out = append(out, "removed "+guestText(p))
		case now != was:
			out = append(out, "changed "+guestText(p))
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, "added "+guestText(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return gitWatchPath(out[i]) < gitWatchPath(out[j]) })
	return out
}

// guestText is s as brig prints it, when s is a name the guest chose. brig
// prints the warning itself, so nothing filters it the way hull filters what
// the guest prints: a hook named with an escape sequence would reach the
// terminal as one, able to erase the lines above it or write the clipboard
// with OSC 52, and a newline in a name would add a line that reads as brig's
// own. A name that is not all printable is quoted, which shows every such
// byte as an escape instead. Printable names, accented ones among them, are
// left as they are.
func guestText(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.Quote(s)
}

// gitWatchPath is the path a diffGitWatch line is about, so the list sorts by
// path and not by verb.
func gitWatchPath(line string) string {
	_, p, _ := strings.Cut(line, " ")
	return p
}

// snapshotProjectGit records what the watched paths under project/.git hold.
//
// The project is reached by the same descent the boot uses, so a link planted
// on the way to it is refused rather than followed. Below it everything goes
// through the os.Root, which cannot be walked out of, and nothing is followed:
// a link is recorded by its target, and only a regular file is read. A project
// with no .git records nothing, so one that gains a .git later shows its files
// as added.
func snapshotProjectGit(project string) (map[string]string, error) {
	held, err := openHeldDir(project, projectSubject)
	if err != nil {
		return nil, err
	}
	defer func() { _ = held.Close() }()
	look := &gitLook{files: map[string]string{}, left: maxGitWatchBytes}
	info, err := held.root.Lstat(".git")
	if errors.Is(err, fs.ErrNotExist) {
		return look.files, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		// A .git file points at a repository elsewhere: a worktree's or a
		// submodule's. That repository is not the guest's to write unless it is
		// under the project too, and following the pointer would be reading a
		// path the guest chose. So the pointer itself is what is watched.
		look.files[".git"] = look.describe(held.root, ".git", info)
		return look.files, nil
	}
	git, err := held.root.OpenRoot(".git")
	if err != nil {
		return nil, err
	}
	defer func() { _ = git.Close() }()
	for _, name := range gitWatchFiles {
		if info, err := git.Lstat(name); err == nil {
			look.files[".git/"+name] = look.describe(git, name, info)
		}
	}
	for _, dir := range gitWatchDirs {
		// Looked at before the walk, because opening it follows a link there:
		// .git/hooks swapped for a link would be walked as the directory it
		// points at, and the swap itself never recorded.
		info, err := git.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		look.files[".git/"+dir] = look.describe(git, dir, info)
		if !info.IsDir() {
			continue
		}
		if err := look.walk(git, dir, ".git/"+dir); err != nil {
			return nil, err
		}
	}
	// A hooksPath the repository already had, pointing into the working tree,
	// is where git runs hooks from instead of .git/hooks. husky sets one, and
	// ignores everything in it, so git status shows a change there no more
	// than one under .git. A hooksPath the guest sets changes .git/config,
	// which is named already; this is the one that was there before.
	if rel, ok := hooksPathInProject(git, held.dir); ok {
		info, err := held.root.Lstat(rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			look.files[rel] = look.describe(held.root, rel, info)
			if info.IsDir() {
				if err := look.walk(held.root, rel, rel); err != nil {
					return nil, err
				}
			}
		}
	}
	return look.files, nil
}

// maxGitConfig is the most of .git/config hooksPathInProject reads. A real one
// is a few kilobytes.
const maxGitConfig = 1 << 20

// hooksPathInProject is the directory the repository's core.hooksPath names,
// relative to project, when it names one inside the project and outside .git,
// whose hooks are looked at already.
//
// .git/config is read through the root, the way every other path under .git
// is, and git parses the bytes from stdin with includes off: git config would
// otherwise read whatever file an include.path names, and a guest can name any
// file of the user's. git runs in a directory with no repository, so it finds
// none to read beside the bytes it is given, and it runs nothing it reads.
//
// A relative hooksPath is relative to the top of the working tree, which is
// the project. One that leads out of the project is not the guest's to write.
// Without git, or with a config git refuses, there is no hooksPath to watch,
// and git on the host could not run one either.
func hooksPathInProject(git *os.Root, project string) (string, bool) {
	info, err := git.Lstat("config")
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := git.OpenFile("config", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	config, err := io.ReadAll(io.LimitReader(f, maxGitConfig))
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--no-includes", "--file", "-", "--type=path", "--get", "core.hooksPath")
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(config)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(project, dir)
	}
	rel, err := filepath.Rel(project, filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || rel == ".git" || rel == ".git/hooks" {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// gitLook is one look in progress: what it has recorded, and how many bytes
// it may still hash before it records files by mode and size only. See
// maxGitWatchBytes.
type gitLook struct {
	files map[string]string
	left  int64
}

// gitWatchBatch is how many entries walk reads from a directory at a time.
const gitWatchBatch = 256

// gitWatchReadBatch is called for each batch readDir reads, so a test can
// count them.
var gitWatchReadBatch = func() {}

// walk records what is under dir in root, with key in place of dir in the
// names it records them under, and stops at maxGitWatchEntries.
//
// A directory is read a batch at a time and no further than the entries a look
// has left. fs.WalkDir read a whole directory before the bound applied, so a
// guest that put 200k names in .git/hooks cost every command a second while
// the look recorded 4096 of them. What a batch read is sorted before it is
// looked at: the byte budget goes to files in the order they are looked at,
// and the order a directory lists in is the filesystem's, so sorting is what
// keeps two looks at the same files giving them the same budget.
//
// Each directory is opened non-blocking and as a directory, so one swapped for
// a fifo after the walk saw it fails the open rather than hanging it. An entry
// removed between the listing and the look at it is not there to run, and is
// left out rather than failing the look.
func (l *gitLook) walk(root *os.Root, dir, key string) error {
	type pending struct{ dir, key string }
	queue := []pending{{dir, key}}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		entries, err := l.readDir(root, d.dir)
		if errors.Is(err, fs.ErrNotExist) && d.dir != dir {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			p, k := d.dir+"/"+e.Name(), d.key+"/"+e.Name()
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if len(l.files) >= maxGitWatchEntries {
				l.files[gitWatchTruncated] = "truncated"
				return nil
			}
			l.files[k] = l.describe(root, p, info)
			if info.IsDir() {
				queue = append(queue, pending{p, k})
			}
		}
	}
	return nil
}

// readDir reads dir no further than the entries the look has left, and one
// past them, so a directory that holds more is still seen to. The entries come
// back sorted by name.
func (l *gitLook) readDir(root *os.Root, dir string) ([]fs.DirEntry, error) {
	f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var entries []fs.DirEntry
	for len(l.files)+len(entries) <= maxGitWatchEntries {
		gitWatchReadBatch()
		batch, err := f.ReadDir(gitWatchBatch)
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) || len(batch) == 0 {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// describe is what a look records for one path: its type and mode, and for a
// regular file its size and the SHA-256 of its first maxGitWatchHash bytes,
// for a link its target. The mode is in it because chmod +x on a hook that was
// there all along is what makes git run it.
//
// The file is opened non-blocking and checked to be regular after the open,
// as openRegular does: the walk saw a regular file, but the guest can put a
// fifo in its place before the open, and a read of that would wait forever.
// A file the rest of the look's bytes cannot cover is not opened at all.
func (l *gitLook) describe(root *os.Root, name string, info os.FileInfo) string {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return "symlink, unreadable"
		}
		return "symlink to " + target
	case mode.IsDir():
		return fmt.Sprintf("directory %04o", mode.Perm())
	case !mode.IsRegular():
		return describeType(mode)
	}
	if min(info.Size(), maxGitWatchHash) > l.left {
		return l.notHashed(mode, info.Size())
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Sprintf("file %04o, unreadable", mode.Perm())
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Sprintf("replaced while brig read it (%04o)", mode.Perm())
	}
	// Asked again of the open file: it can have grown since the walk saw it.
	want := min(fi.Size(), maxGitWatchHash)
	if want > l.left {
		return l.notHashed(fi.Mode(), fi.Size())
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, want))
	l.left -= n
	if err != nil {
		return fmt.Sprintf("file %04o, unreadable", fi.Mode().Perm())
	}
	return fmt.Sprintf("file %04o %d %x", fi.Mode().Perm(), fi.Size(), h.Sum(nil))
}

// notHashed records a file the look's bytes cannot cover, and marks the look
// as one that ran out.
func (l *gitLook) notHashed(mode os.FileMode, size int64) string {
	l.files[gitWatchTruncated] = "truncated"
	return fmt.Sprintf("file %04o %d, not hashed", mode.Perm(), size)
}
