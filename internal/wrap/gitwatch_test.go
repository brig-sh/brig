package wrap

import (
	"bytes"
	"os"
	"path/filepath"
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

	PruneSessions([]string{"brig-unrelated"})
	if _, ok := readIndex[gitWatchEntry](gitWatchIndexName)["brig-other"]; ok {
		t.Error("ls kept the look at a sandbox the runtime no longer has")
	}
}
