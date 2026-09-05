package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/profile"
)

// gh's hosts.yml is a host file, so os.Root does not cover it and nothing
// checked what it was before opening it. A FIFO there hung brig on every verb
// that loads a profile: resolveGitIdentity runs at the end of Load, the open
// blocks waiting for a writer, and the user sees no output at all.
//
// The assertion is that the call RETURNS, not merely that it errors. Before
// the fix this test does not fail, it hangs, and `within` is what turns that
// into a failure the test binary can report.
func TestGhHostsUserRefusesAFifoInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yml")
	mkfifo(t, path)
	t.Setenv("GH_CONFIG_DIR", dir)

	var (
		user string
		err  error
	)
	if !within(t, 5*time.Second, func() { user, err = ghHostsUser("github.com") }) {
		t.Fatalf("brig blocked reading a FIFO at %s", path)
	}
	if err == nil {
		t.Fatalf("a FIFO was read as if it were hosts.yml, returning %q", user)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the path: %v", err)
	}
	// In words. fs.FileMode.Type() prints "p---------", which is not something
	// to put in front of a person.
	if !strings.Contains(err.Error(), "named pipe") {
		t.Errorf("the refusal does not say what it found there: %v", err)
	}
	if strings.Contains(err.Error(), "p---------") {
		t.Errorf("the refusal renders the file type as a mode string: %v", err)
	}
}

// A directory does not hang, it fails somewhere else: os.Open succeeds on one,
// the read fails with EISDIR, and the scanner's error was dropped -- so brig
// silently fell back to x-access-token and the "Invalid username or token"
// that followed pointed at nothing. Say what is there instead.
func TestGhHostsUserRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)

	_, err := ghHostsUser("github.com")
	if err == nil {
		t.Fatal("a directory was accepted as hosts.yml")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "directory") {
		t.Errorf("the refusal does not name the path and what it is: %v", err)
	}
}

// An absent hosts.yml is the ordinary case -- gh is simply not set up -- and
// must stay a fallback rather than becoming a refusal.
func TestGhHostsUserIsQuietWhenThereIsNoHostsFile(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())

	user, err := ghHostsUser("github.com")
	if err != nil {
		t.Fatalf("a missing hosts.yml was refused: %v", err)
	}
	if user != "" {
		t.Errorf("a missing hosts.yml produced a user: %q", user)
	}
}

// The refusal has to reach the person who typed the command, which means Load
// fails rather than warning and carrying on with a username it did not read.
func TestLoadRefusesAFifoWhereHostsYmlBelongs(t *testing.T) {
	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "hosts.yml"))
	t.Setenv("GH_CONFIG_DIR", dir)
	// github.user in the host's git config short-circuits the hosts.yml read,
	// so the lookup only happens with none set.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig"))

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	if u := (&Config{}).gitConfigValue("github.user"); u != "" {
		t.Skipf("this machine's git config sets github.user to %q, so hosts.yml is "+
			"never read", u)
	}

	var err error
	if !within(t, 10*time.Second, func() { _, err = Load(p, Options{}, nil) }) {
		t.Fatal("Load blocked on a FIFO at hosts.yml")
	}
	if err == nil {
		t.Fatal("Load accepted a FIFO at hosts.yml")
	}
	if !strings.Contains(err.Error(), "named pipe") {
		t.Errorf("Load's error does not say what it found: %v", err)
	}
}
