package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// within runs fn and reports whether it finished. A FIFO opened without
// O_NONBLOCK never returns, so "did it finish" is the whole assertion.
func within(t *testing.T, d time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// The write path had no regular-file rule at all, so one mkfifo hung every brig
// invocation: PrepareWorkspace is the first thing EnsureRunning does, and
// os.Root.WriteFile opens O_WRONLY|O_CREATE|O_TRUNC, which waits for a reader.
// No race, no output, nothing for the user to diagnose.
func TestWriteFileRefusesAFifoInsteadOfBlocking(t *testing.T) {
	for _, name := range []string{".brig-workspace", ".brig-git-credential", ".gitconfig.brig"} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			mkfifo(t, filepath.Join(ws, name))
			c := &Config{Workspace: ws}
			r, err := c.openWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()

			var werr error
			if !within(t, 5*time.Second, func() { werr = r.writeFile(name, []byte("x"), 0o600) }) {
				t.Fatalf("brig blocked writing to a guest-planted FIFO at %s", name)
			}
			if werr == nil {
				t.Fatalf("a FIFO at %s was written to as if it were a file", name)
			}
		})
	}
}

// writeIfChanged is the path PrepareWorkspace actually takes. It used to call
// exists() (a FIFO is not a symlink, so "present, fine"), discard readFile's
// refusal with `err == nil`, and then block in the write.
func TestWriteIfChangedRefusesAFifoInsteadOfBlocking(t *testing.T) {
	ws := t.TempDir()
	mkfifo(t, filepath.Join(ws, ".brig-workspace"))
	c := &Config{Workspace: ws}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	var werr error
	if !within(t, 5*time.Second, func() { werr = r.writeIfChanged(".brig-workspace", "x", 0o600) }) {
		t.Fatal("writeIfChanged blocked on a guest-planted FIFO")
	}
	if werr == nil {
		t.Fatal("writeIfChanged accepted a FIFO")
	}
}

// The read side decides from the descriptor now, so a FIFO swapped in after any
// prior check still cannot block: the open itself is O_NONBLOCK.
func TestReadFileRefusesAFifoFromTheDescriptor(t *testing.T) {
	ws := t.TempDir()
	mkfifo(t, filepath.Join(ws, ".claude.json"))
	c := &Config{Workspace: ws}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	var rerr error
	if !within(t, 5*time.Second, func() { _, rerr = r.readFile(".claude.json") }) {
		t.Fatal("readFile blocked on a guest-planted FIFO")
	}
	if rerr == nil {
		t.Fatal("readFile accepted a FIFO")
	}
	if strings.Contains(rerr.Error(), "L---------") {
		t.Errorf("the refusal renders the file type as a mode string: %v", rerr)
	}
	if !strings.Contains(rerr.Error(), "named pipe") {
		t.Errorf("the refusal does not say what it found: %v", rerr)
	}
}

// The regression the type check introduced: Lstat answers about the LINK, so a
// dotfile symlinked to a file INSIDE the workspace -- something a person may
// well have done deliberately -- was refused, with a message claiming it "leads
// out of" the workspace. os.Root already refuses the links that escape; this
// one does not escape and must work.
func TestReadFileFollowsASymlinkThatStaysInsideTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "dotfiles-gitconfig"),
		[]byte("[user]\n\tname = Someone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dotfiles-gitconfig", filepath.Join(ws, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	c := &Config{Workspace: ws}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	blob, err := r.readFile(".gitconfig")
	if err != nil {
		t.Fatalf("a symlink pointing inside the workspace was refused: %v", err)
	}
	if !strings.Contains(string(blob), "Someone") {
		t.Errorf("read the wrong file through the symlink: %q", blob)
	}
}

// And the link that DOES escape is still refused, by os.Root.
func TestReadFileStillRefusesASymlinkOutOfTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("HOST SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ws, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	c := &Config{Workspace: ws}
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if blob, err := r.readFile(".gitconfig"); err == nil {
		t.Fatalf("read a host file through an escaping symlink: %q", blob)
	}
}

// wireInclude must not hand the workspace path to another process. It used to
// run `git config -f <path>`, which resolves the path itself: a FIFO swapped in
// after the read parked a git child on it that blocked forever and outlived
// brig.
func TestWireIncludeDoesNotShellOutToGit(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws)
	r, err := c.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if !within(t, 10*time.Second, func() { _ = c.wireInclude(r) }) {
		t.Fatal("wireInclude did not return")
	}
	blob, err := os.ReadFile(filepath.Join(ws, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if !includeIsWired(blob) {
		t.Fatalf("wireInclude did not wire the include:\n%s", blob)
	}
	// Second call must be a no-op, which is what the git subprocess used to
	// decide.
	if err := c.wireInclude(r); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(ws, ".gitconfig"))
	if strings.Count(string(again), includePath) != 1 {
		t.Errorf("the include was appended twice:\n%s", again)
	}
}

func TestIncludeIsWired(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob string
		want bool
	}{
		{"empty", "", false},
		{"ours", "[include]\n\tpath = " + includePath + "\n", true},
		{"ours with comment", "[include]\n\tpath = " + includePath + " # brig\n", true},
		{"another include", "[include]\n\tpath = /somewhere/else\n", false},
		{"right value, wrong section", "[user]\n\tpath = " + includePath + "\n", false},
		{"includeIf is not ours", "[includeIf \"gitdir:/x\"]\n\tpath = " + includePath + "\n", false},
		{"commented out", "#[include]\n#\tpath = " + includePath + "\n", false},
		{"case-insensitive section", "[INCLUDE]\n\tPath = " + includePath + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := includeIsWired([]byte(tc.blob)); got != tc.want {
				t.Errorf("includeIsWired(%q) = %v, want %v", tc.blob, got, tc.want)
			}
		})
	}
}
