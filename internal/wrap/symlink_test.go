package wrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
)

// These tests are the sandbox's side of the bargain. The workspace is the
// guest's home, mounted read-write, and brig writes into it from the host on
// every single invocation -- so every file brig touches in there is a file the
// guest gets to replace with a symlink first. Following one would have brig,
// which runs as you and outside the sandbox, write to (or read out of) a host
// path the guest cannot reach itself: the whole point of the sandbox, undone
// by one `ln -s`.
//
// Each test plants the link the guest would plant, runs the real code path,
// and asserts two things: brig said no, and the victim outside the workspace
// is exactly as it was. Every victim lives under the test's own t.TempDir().

// plantLink puts a symlink at rel inside the workspace, the way a guest with a
// shell in its own home would.
func plantLink(t *testing.T, ws, rel, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, rel)); err != nil {
		t.Fatal(err)
	}
}

// wantRefused checks that the refusal is the loud one: a real error, tied to
// errPlantedSymlink so callers can tell it from an ordinary I/O failure, and
// naming the file so the user knows what the guest tried.
func wantRefused(t *testing.T, err error, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the planted symlink at %s was followed, not refused", name)
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Fatalf("refused with an unrelated error: %v", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("the error does not name %s: %v", name, err)
	}
}

// The marker is the default-on primitive: PrepareWorkspace writes it on every
// invocation, before anything checks whether a sandbox is even running, so a
// link planted here is followed by `brig env` as surely as by `brig create`.
func TestMarkerWriteRefusesAPlantedSymlink(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(victim, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantLink(t, ws, markerFile, victim)

	c := testConfig(t, ws, ws)
	err := c.PrepareWorkspace()
	wantRefused(t, err, markerFile)

	if blob, rerr := os.ReadFile(victim); rerr != nil || string(blob) != "mine\n" {
		t.Errorf("the host file was truncated and overwritten: %q, %v", blob, rerr)
	}
	if st, _ := os.Stat(victim); st != nil && st.Mode().Perm() != 0o600 {
		t.Errorf("the host file's mode was changed to %v", st.Mode().Perm())
	}
}

// A DANGLING link is the nastier half: nothing is there to stat, so the
// absent-file check reads as "not seeded yet" and the seed is created at the
// far end -- a new file at any path the host user can write. Refused, and
// refused loudly rather than skipped: a workspace that grows a symlink where
// its state file belongs is not a thing to pass over in silence.
func TestOnboardingSeedRefusesADanglingSymlink(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "created-by-brig.json")
	plantLink(t, ws, ".claude.json", victim)

	c := testConfig(t, ws, ws)
	err := c.seedOnboarding(mustRoot(t, c))
	wantRefused(t, err, ".claude.json")

	if _, serr := os.Lstat(victim); !os.IsNotExist(serr) {
		t.Errorf("brig created a file outside the workspace: %v", serr)
	}
}

// trustGuestCwd is the one that reads. os.Rename cannot be made to land
// outside a directory, which is what made this look safe -- but the read at
// the front follows the link out, and the rename then puts the parsed result
// back INSIDE the guest's home as a regular file. That is a copy in: aim the
// link at any JSON the host user can read and the sandbox is handed it.
func TestTrustGuestCwdRefusesToReadOutOfTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "config.json")
	secret := `{"auths":{"registry.example":{"auth":"c3VwZXItc2VjcmV0"}}}`
	if err := os.WriteFile(victim, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	plantLink(t, ws, ".claude.json", victim)

	c := testConfig(t, ws, ws)
	err := c.trustGuestCwd(mustRoot(t, c))
	wantRefused(t, err, ".claude.json")

	if blob, _ := os.ReadFile(victim); string(blob) != secret {
		t.Errorf("the host file was rewritten: %s", blob)
	}
	// The exfiltration leg: nothing inside the workspace may hold what the
	// link pointed at. Before the fix the symlink was replaced by a regular
	// file carrying exactly this content, which the guest then reads at leisure.
	for _, name := range mustReadDirNames(t, ws) {
		info, ierr := os.Lstat(filepath.Join(ws, name))
		if ierr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if blob, _ := os.ReadFile(filepath.Join(ws, name)); strings.Contains(string(blob), "super-secret") ||
			strings.Contains(string(blob), "c3VwZXItc2VjcmV0") {
			t.Errorf("a host secret was copied into the guest's home as %s: %s", name, blob)
		}
	}
}

// The write end of the same function, on its own: the temporary file is
// renamed into place, and that rename may not be steered out of the workspace
// by a symlinked directory in the path. Exercised through the helper because
// the full path refuses at the read before it ever gets here.
func TestTrustGuestCwdRenameCannotLandOutsideTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	plantLink(t, ws, "elsewhere", outside)

	r := mustRoot(t, testConfig(t, ws, ws))
	f, name, err := r.createTemp(".brig-trust-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"trusted":true}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = r.rename(name, filepath.Join("elsewhere", "state.json"))
	wantRefused(t, err, "state.json")
	if _, serr := os.Lstat(filepath.Join(outside, "state.json")); !os.IsNotExist(serr) {
		t.Errorf("the rename landed outside the workspace: %v", serr)
	}
}

// seedHostConfig walks the host's real ~/.claude into the workspace. Replace
// the destination .claude with a link and the copy -- directories and all --
// is made wherever the guest chose, out of the user's own configuration.
func TestSeedHostConfigRefusesASymlinkedDestination(t *testing.T) {
	home, ws, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".claude", "skills", "ananos")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	plantLink(t, ws, ".claude", outside)

	tm, _ := profile.Lookup("claude-code")
	c := testConfig(t, ws, ws)
	c.Profile = tm
	c.HostConfig = hostProjections(tm, true)

	err := c.seedHostConfig(mustRoot(t, c))
	wantRefused(t, err, ".claude")

	if names := mustReadDirNames(t, outside); len(names) != 0 {
		t.Errorf("the host's configuration was copied outside the workspace: %v", names)
	}
}

// A link AT the workspace is the one an os.Root cannot see, because opening a
// root resolves the path it is handed: every write below it then looks
// perfectly contained while landing somewhere else entirely. Checked before
// the root is opened, and before the directory is created.
func TestSymlinkedWorkspaceRootIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	c := testConfig(t, link, link)
	err := c.PrepareWorkspace()
	wantRefused(t, err, "link")

	if names := mustReadDirNames(t, real); len(names) != 0 {
		t.Errorf("brig wrote through the symlinked workspace: %v", names)
	}
}

// The credential helper is written mode 0755. A link planted at it turns
// "regenerate our helper" into making an arbitrary host file executable and
// filling it with a script -- the worst of the git pair, and why both go
// through the root even though this half needs BRIG_GIT_CONFIG=1.
func TestSetupGitRefusesASymlinkedCredentialHelper(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(victim, []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantLink(t, ws, credentialHelper, victim)

	c := gitConfig(t, ws)
	var set creds.Set
	err := c.SetupGit(&set)
	wantRefused(t, err, credentialHelper)

	blob, _ := os.ReadFile(victim)
	if string(blob) != "notes\n" {
		t.Errorf("the host file was overwritten with the helper script: %s", blob)
	}
	if st, _ := os.Stat(victim); st != nil && st.Mode()&0o111 != 0 {
		t.Errorf("the host file was made executable: %v", st.Mode())
	}
}

// wireInclude appends, which needs no truncation to do damage: two lines of
// git configuration land at the end of whatever the link names. It also hands
// the path to `git config -f`, which resolves symlinks itself, so the check
// has to happen before git is invoked, not after.
func TestSetupGitRefusesASymlinkedGitconfig(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim.cfg")
	if err := os.WriteFile(victim, []byte("[user]\n\tname = someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantLink(t, ws, ".gitconfig", victim)

	c := gitConfig(t, ws)
	var set creds.Set
	err := c.SetupGit(&set)
	wantRefused(t, err, ".gitconfig")

	blob, _ := os.ReadFile(victim)
	if strings.Contains(string(blob), "[include]") {
		t.Errorf("the host file was appended to: %s", blob)
	}
}

// And the ordinary case still works, because a fix that refuses everything is
// not a fix: a clean workspace gets its marker, its seed and its trust key,
// and nothing about `brig env` or `brig create` changes for anyone who has not
// been attacked.
func TestPrepareWorkspaceStillWorksWithoutAnAttack(t *testing.T) {
	ws := t.TempDir()
	c := testConfig(t, ws, ws)
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(ws, markerFile))
	if err != nil {
		t.Fatalf("no marker was written: %v", err)
	}
	if !strings.Contains(string(marker), ws) {
		t.Errorf("marker = %q", marker)
	}
	state, err := os.ReadFile(filepath.Join(ws, ".claude.json"))
	if err != nil {
		t.Fatalf("no onboarding seed was written: %v", err)
	}
	if !strings.Contains(string(state), "hasTrustDialogAccepted") {
		t.Errorf("the trust key was not set: %s", state)
	}
	// A second run is the one that exercises every "already there" path.
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatalf("a second run failed: %v", err)
	}
}

func mustReadDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
