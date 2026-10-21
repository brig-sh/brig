package wrap

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"

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
// filter and diff drivers config names. Submodules keep theirs under
// .git/modules and are not looked at.
var (
	gitWatchFiles = []string{"config", "config.worktree"}
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
			c.warnf("%s", notice.Newf("could not read %s/.git to see what the sandbox changed there", prev.Project).
				Note("%v", err))
			return
		}
		changes := diffGitWatch(prev.Files, now)
		if len(changes) > 0 {
			if c.Verbosity < Normal {
				return
			}
			c.warnf("%s", gitChangeNotice(c.VMName, prev.Project, changes))
		}
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
			Note("%v", err))
		return
	}
	c.recordGitWatch(index, project, now)
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
			out = append(out, "removed "+p)
		case now != was:
			out = append(out, "changed "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, "added "+p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return gitWatchPath(out[i]) < gitWatchPath(out[j]) })
	return out
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
		// Looked at before the walk, because WalkDir stats its root and so
		// follows a link there: .git/hooks swapped for a link would be walked as
		// the directory it points at, and the swap itself never recorded.
		info, err := git.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			look.files[".git/"+dir] = look.describe(git, dir, info)
			continue
		}
		err = fs.WalkDir(git.FS(), dir, func(p string, d fs.DirEntry, err error) error {
			// An entry removed between the listing and the look at it is not
			// there to run, and failing the whole look over it would trade the
			// changes this look did see for a "could not read".
			if errors.Is(err, fs.ErrNotExist) && p != dir {
				return nil
			}
			if err != nil {
				return err
			}
			if len(look.files) >= maxGitWatchEntries {
				look.files[gitWatchTruncated] = "truncated"
				return fs.SkipAll
			}
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			// WalkDir names paths with slashes on every platform.
			look.files[".git/"+p] = look.describe(git, p, info)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return look.files, nil
}

// gitLook is one look in progress: what it has recorded, and how many bytes
// it may still hash before it records files by mode and size only. See
// maxGitWatchBytes.
type gitLook struct {
	files map[string]string
	left  int64
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
