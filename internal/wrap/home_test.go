package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// isolateHome is isolateState with HOME moved too, so the check for a legacy
// ~/brig directory reads a scratch home rather than the real one.
func isolateHome(t *testing.T) (state, home string) {
	t.Helper()
	state = isolateState(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	return state, home
}

// bootHome does what a run does to the home before the sandbox boots: create
// it, record the session, and leave some guest state in it.
func bootHome(t *testing.T, c *Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(c.Workspace, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace, ".local", "bin", "claude"),
		[]byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.rememberSession()
}

func TestNoWorkspaceGivesAnEphemeralHomeUnderTheStateDir(t *testing.T) {
	state, _ := isolateHome(t)
	for _, name := range []string{"", "refactor"} {
		c := mustLoad(t, Options{Name: name})
		want := filepath.Join(state, "homes", "brig-claude-code")
		if name != "" {
			want += "-" + name
		}
		if c.Workspace != want {
			t.Errorf("--name %q: home %q, want %q", name, c.Workspace, want)
		}
		if !c.EphemeralHome {
			t.Errorf("--name %q: a home brig chose is not ephemeral", name)
		}
	}
}

func TestANamedWorkspaceIsNeverEphemeral(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	if c := mustLoad(t, Options{Workspace: ws}); c.EphemeralHome {
		t.Error("a -w home is ephemeral")
	}
	t.Setenv("BRIG_WORKSPACE", ws)
	if c := mustLoad(t, Options{}); c.EphemeralHome {
		t.Error("a BRIG_WORKSPACE home is ephemeral")
	}
}

// A session an older release started has an index entry with no ephemeral
// flag and a home under ~/brig. A flagless verb resolves that home and has to
// keep it.
func TestAnOlderSessionKeepsItsHome(t *testing.T) {
	isolateHome(t)
	legacy := t.TempDir()
	c := mustLoad(t, Options{Workspace: legacy})
	c.rememberSession()

	again := mustLoad(t, Options{})
	if again.Workspace != legacy || again.EphemeralHome {
		t.Fatalf("resolved %q (ephemeral %v), want %q kept", again.Workspace,
			again.EphemeralHome, legacy)
	}
	again.Runtime = &removingRuntime{}
	if err := again.Remove(); err != nil {
		t.Fatal(err)
	}
	if again.RemovedHome != "" {
		t.Errorf("rm reported deleting %q", again.RemovedHome)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("rm deleted the home of an older session: %v", err)
	}
}

func TestRemoveDeletesAnEphemeralHome(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)

	if got := EphemeralHomeOf(c.VMName); got != c.Workspace {
		t.Fatalf("EphemeralHomeOf = %q, want %q", got, c.Workspace)
	}
	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil {
		t.Fatal(err)
	}
	if c.RemovedHome != c.Workspace {
		t.Errorf("RemovedHome = %q, want %q", c.RemovedHome, c.Workspace)
	}
	if _, err := os.Lstat(c.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the home is still there after rm: %v", err)
	}

	// The same command again starts from an empty home.
	next := mustLoad(t, Options{})
	if _, err := os.Lstat(filepath.Join(next.Workspace, ".local")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a new run found the old guest state: %v", err)
	}
}

func TestRemoveKeepsANamedHome(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	c := mustLoad(t, Options{Workspace: ws})
	bootHome(t, c)

	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil {
		t.Fatal(err)
	}
	if c.RemovedHome != "" {
		t.Errorf("rm reported deleting %q", c.RemovedHome)
	}
	if _, err := os.Stat(filepath.Join(ws, ".local", "bin", "claude")); err != nil {
		t.Errorf("rm deleted a -w home: %v", err)
	}
}

// failingRuntime refuses to remove the sandbox.
type failingRuntime struct{ removingRuntime }

func (r *failingRuntime) Remove(string) error { return errors.New("busy") }

// A sandbox the runtime could not remove may still have the home mounted.
func TestRemoveKeepsTheHomeWhenTheSandboxStays(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)

	c.Runtime = &failingRuntime{}
	if err := c.Remove(); err == nil {
		t.Fatal("a failed removal was reported as success")
	}
	if _, err := os.Stat(c.Workspace); err != nil {
		t.Errorf("the home went with a sandbox that is still there: %v", err)
	}
}

// The guest owns everything inside its home, including symlinks. Deleting the
// home removes a link and leaves its host target alone.
func TestDropEphemeralHomeDoesNotFollowGuestSymlinks(t *testing.T) {
	isolateHome(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("host data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, Options{})
	bootHome(t, c)
	if err := os.Symlink(outside, filepath.Join(c.Workspace, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := DropEphemeralHome(c.VMName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("deleting the home followed a guest symlink: %v", err)
	}
}

// Only a direct child of the homes directory is ever deleted, whatever the
// index says.
func TestDropEphemeralHomeRefusesAPathOutsideHomes(t *testing.T) {
	isolateHome(t)
	for _, home := range []string{t.TempDir(), filepath.Join(os.Getenv("BRIG_STATE_DIR"), "homes")} {
		index := map[string]sessionEntry{
			"claude-code": {Home: home, Sandbox: "brig-claude-code", Ephemeral: true},
		}
		if err := writeSessionIndex(index); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := DropEphemeralHome("brig-claude-code")
		if err == nil || got != "" {
			t.Errorf("home %q: deleted %q, err %v; want a refusal", home, got, err)
		}
		if _, err := os.Stat(home); err != nil {
			t.Errorf("home %q was deleted: %v", home, err)
		}
	}
}

func TestEphemeralNoticeOnceAndOnlyForBrigHomes(t *testing.T) {
	_, home := isolateHome(t)
	var errOut bytes.Buffer

	c := mustLoad(t, Options{})
	c.Err, c.Verbosity = &errOut, Normal
	c.ephemeralNotice()
	if !strings.Contains(errOut.String(), "brig rm claude-code") {
		t.Errorf("a new ephemeral home said nothing:\n%s", errOut.String())
	}

	errOut.Reset()
	bootHome(t, c)
	c.ephemeralNotice()
	if errOut.Len() != 0 {
		t.Errorf("an existing home repeated the notice:\n%s", errOut.String())
	}

	errOut.Reset()
	named := mustLoad(t, Options{Workspace: filepath.Join(home, "mine")})
	named.Err, named.Verbosity = &errOut, Normal
	named.ephemeralNotice()
	if errOut.Len() != 0 {
		t.Errorf("a -w home got the notice:\n%s", errOut.String())
	}
}

// A user who kept work in ~/brig/<profile> is told how to keep using it.
func TestEphemeralNoticeNamesTheLegacyHome(t *testing.T) {
	_, home := isolateHome(t)
	legacy := filepath.Join(home, "brig", "claude-code")
	if err := os.MkdirAll(legacy+"-refactor", 0o755); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	c := mustLoad(t, Options{Name: "refactor"})
	c.Err, c.Verbosity = &errOut, Normal
	c.ephemeralNotice()
	if want := "Pass --home " + legacy + " to keep"; !strings.Contains(errOut.String(), want) {
		t.Errorf("the notice does not say %q:\n%s", want, errOut.String())
	}
}

// listingRuntime is removingRuntime with a fixed instance list, for the
// checks that ask the runtime whether a sandbox exists.
type listingRuntime struct {
	removingRuntime
	names []string
}

func (r *listingRuntime) List() ([]runtime.Instance, error) {
	list := make([]runtime.Instance, 0, len(r.names))
	for _, n := range r.names {
		list = append(list, runtime.Instance{Name: n, State: "stopped"})
	}
	return list, nil
}

func (r *listingRuntime) Exists(name string) (bool, error) {
	for _, n := range r.names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// partialRuntime lists no stopped sandboxes and has no Exists, so it cannot
// say that a missing sandbox is gone.
type partialRuntime struct{ removingRuntime }

func (r *partialRuntime) List() ([]runtime.Instance, error) { return nil, nil }

// A stopped sandbox is missing from a running-only listing. The reap must not
// take that for a removed sandbox.
func TestAPartialListingReapsNothing(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)
	ForgetSandbox(c.VMName)

	next := mustLoad(t, Options{})
	next.Runtime = &partialRuntime{}
	next.reapOrphanHome()
	if _, err := os.Stat(filepath.Join(next.Workspace, ".local", "bin", "claude")); err != nil {
		t.Errorf("a running-only listing reaped the home: %v", err)
	}
}

// A home the run named is the user's, even inside the homes directory.
func TestANamedHomeIsNeverReaped(t *testing.T) {
	state, _ := isolateHome(t)
	home := filepath.Join(state, "homes", "brig-mine")
	c := mustLoad(t, Options{Workspace: home})
	bootHome(t, c)
	ForgetSandbox(c.VMName)

	next := mustLoad(t, Options{Workspace: home})
	next.Runtime = &listingRuntime{}
	next.reapOrphanHome()
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "claude")); err != nil {
		t.Errorf("a --home home was reaped: %v", err)
	}
}

// The ephemeral flag alone decides: a recorded home inside the homes
// directory without it is kept.
func TestANonEphemeralEntryInsideHomesIsKept(t *testing.T) {
	state, _ := isolateHome(t)
	home := filepath.Join(state, "homes", "brig-claude-code")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	index := map[string]sessionEntry{
		"claude-code": {Home: home, Sandbox: "brig-claude-code"},
	}
	if err := writeSessionIndex(index); err != nil {
		t.Fatal(err)
	}
	if got := EphemeralHomeOf("brig-claude-code"); got != "" {
		t.Errorf("EphemeralHomeOf = %q for a non-ephemeral entry", got)
	}
	if got, err := DropEphemeralHome("brig-claude-code"); got != "" || err != nil {
		t.Errorf("deleted %q, err %v", got, err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("a non-ephemeral home was deleted: %v", err)
	}
}

// rm --dry-run must not announce a home that was never created.
func TestEphemeralHomeOfIgnoresAMissingHome(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	c.rememberSession()
	if got := EphemeralHomeOf(c.VMName); got != "" {
		t.Errorf("EphemeralHomeOf = %q for a home that does not exist", got)
	}
}

// A guest leaves read-only trees behind, e.g. a Go module cache.
func TestRemoveDeletesAReadOnlyTree(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)
	ro := filepath.Join(c.Workspace, "go", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "f.go"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ro, filepath.Dir(ro)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil || c.HomeErr != nil {
		t.Fatalf("remove: %v, home: %v", err, c.HomeErr)
	}
	if _, err := os.Lstat(c.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a read-only tree kept the home: %v", err)
	}
}

// Two sandbox names for one session get two homes, so removing one never
// deletes the home the other is running on.
func TestBrigNameGetsAHomeOfItsOwn(t *testing.T) {
	isolateHome(t)
	first := mustLoad(t, Options{})
	t.Setenv("BRIG_NAME", "brig-alt")
	second := mustLoad(t, Options{})
	if first.Workspace == second.Workspace {
		t.Errorf("both sandboxes share the home %s", first.Workspace)
	}
}

// A home no session owns, whose sandbox the runtime no longer has, is wiped
// before the next run boots on it.
func TestAnOrphanHomeIsReapedBeforeBoot(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)
	ForgetSandbox(c.VMName) // what `brig ls` does after an outside removal

	next := mustLoad(t, Options{})
	next.Runtime = &listingRuntime{}
	next.reapOrphanHome()
	if _, err := os.Lstat(next.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphaned home survived: %v", err)
	}
}

// A stopped sandbox whose index entry was lost still owns its home.
func TestAHomeWithALiveSandboxIsNotReaped(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)
	ForgetSandbox(c.VMName)

	next := mustLoad(t, Options{})
	next.Runtime = &listingRuntime{names: []string{next.VMName}}
	next.reapOrphanHome()
	if _, err := os.Stat(filepath.Join(next.Workspace, ".local", "bin", "claude")); err != nil {
		t.Errorf("the home of a live sandbox was reaped: %v", err)
	}
	// A runtime that cannot be asked deletes nothing either.
	next.Runtime = nil
	next.reapOrphanHome()
	if _, err := os.Stat(next.Workspace); err != nil {
		t.Errorf("the home was reaped with no runtime to ask: %v", err)
	}
}

// A sandbox an older release booted on ~/brig, with no index entry, keeps
// that home instead of restarting onto an empty one.
func TestALiveLegacySandboxKeepsItsHome(t *testing.T) {
	_, home := isolateHome(t)
	legacy := filepath.Join(home, "brig", "claude-code")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := profile.Lookup("claude-code")
	c, err := Load(p, Options{}, &listingRuntime{names: []string{"brig-claude-code"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != legacy || c.EphemeralHome {
		t.Errorf("resolved %q (ephemeral %v), want %q kept", c.Workspace, c.EphemeralHome, legacy)
	}
}

func TestARelativeStateDirStillDeletes(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	t.Setenv("BRIG_STATE_DIR", "rel-state")
	c := mustLoad(t, Options{})
	bootHome(t, c)
	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil || c.HomeErr != nil {
		t.Fatalf("remove: %v, home: %v", err, c.HomeErr)
	}
	if c.RemovedHome == "" {
		t.Error("a relative BRIG_STATE_DIR kept the home")
	}
}

// A home the run named is the user's, even inside brig's homes directory, so
// rm keeps it.
func TestANamedHomeInsideHomesDirIsKept(t *testing.T) {
	state, _ := isolateHome(t)
	home := filepath.Join(state, "homes", "brig-x")
	c := mustLoad(t, Options{Workspace: home})
	if c.EphemeralHome {
		t.Error("a --home inside the homes directory is ephemeral")
	}
	bootHome(t, c)
	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "claude")); err != nil {
		t.Errorf("rm deleted a --home inside the homes directory: %v", err)
	}
}

// bootFailRuntime refuses every boot, the way hull does when it cannot create
// its store, and says whether a sandbox of the name exists.
type bootFailRuntime struct {
	livenessRuntime
	exists bool
}

func (r *bootFailRuntime) Run(runtime.RunSpec) error {
	r.boots++
	return errors.New("create store mountpoint: mkdir /root: read-only file system")
}

func (r *bootFailRuntime) Exists(string) (bool, error) { return r.exists, nil }

// failBoot runs c up to a boot the runtime refuses.
func failBoot(t *testing.T, c *Config, rt runtime.Runtime) {
	t.Helper()
	c.Runtime, c.Verify = rt, verify.Off
	c.Err, c.Verbosity = &bytes.Buffer{}, Normal
	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("the runtime refused the boot and EnsureRunning did not")
	}
}

// A first boot that fails leaves no sandbox and records no session, so neither
// `brig rm` nor `rm --all` finds anything to remove. The home brig created
// for it goes with the failure, and the next run has nothing to reap.
func TestAFailedFirstBootDeletesTheHomeItCreated(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	if _, err := os.Lstat(c.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the home exists before the first run: %v", err)
	}
	rt := &bootFailRuntime{}
	failBoot(t, c, rt)
	if rt.boots != 1 {
		t.Fatalf("booted %d times, want 1", rt.boots)
	}
	if _, err := os.Lstat(c.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed first boot left its home behind: %v", err)
	}

	var errOut bytes.Buffer
	next := mustLoad(t, Options{})
	next.Runtime, next.Err, next.Verbosity = &listingRuntime{}, &errOut, Normal
	next.reapOrphanHome()
	if errOut.Len() != 0 {
		t.Errorf("the next run reaped a home: %s", errOut.String())
	}
}

// The failure path deletes only a home brig created. A --home or
// BRIG_WORKSPACE directory is the user's even when this run created it, and
// even inside the homes directory.
func TestAFailedBootNeverDeletesANamedHome(t *testing.T) {
	state, _ := isolateHome(t)
	for _, tc := range []struct {
		name string
		home string
		env  bool
	}{
		{"--home, new", filepath.Join(t.TempDir(), "mine"), false},
		{"--home in the homes dir", filepath.Join(state, "homes", "brig-claude-code"), false},
		{"BRIG_WORKSPACE, new", filepath.Join(t.TempDir(), "env"), true},
	} {
		o := Options{Workspace: tc.home}
		if tc.env {
			o = Options{}
			t.Setenv("BRIG_WORKSPACE", tc.home)
		}
		c := mustLoad(t, o)
		if c.Workspace != tc.home || c.EphemeralHome {
			t.Fatalf("%s: resolved %q (ephemeral %v)", tc.name, c.Workspace, c.EphemeralHome)
		}
		failBoot(t, c, &bootFailRuntime{})
		if _, err := os.Stat(filepath.Join(tc.home, markerFile)); err != nil {
			t.Errorf("%s: a failed boot deleted the named home: %v", tc.name, err)
		}
		if tc.env {
			if err := os.Unsetenv("BRIG_WORKSPACE"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A later boot of a session that booted before is not a first boot. Its home
// holds the guest's state, and a failure keeps it for the next run.
func TestAFailedBootKeepsTheHomeOfAnEarlierBoot(t *testing.T) {
	isolateHome(t)
	c := mustLoad(t, Options{})
	bootHome(t, c)

	next := mustLoad(t, Options{})
	failBoot(t, next, &bootFailRuntime{})
	if _, err := os.Stat(filepath.Join(next.Workspace, ".local", "bin", "claude")); err != nil {
		t.Errorf("a failed boot deleted the home of an earlier boot: %v", err)
	}
}

// A runtime that holds a sandbox of the name, or cannot say whether it does,
// may have the home mounted, so a failure keeps it.
func TestAFailedBootKeepsAHomeTheRuntimeMayHold(t *testing.T) {
	for name, rt := range map[string]runtime.Runtime{
		"a sandbox exists": &bootFailRuntime{exists: true},
		"cannot say":       &livenessRuntime{runningErr: errors.New("cannot connect")},
	} {
		isolateHome(t)
		c := mustLoad(t, Options{})
		failBoot(t, c, rt)
		if _, err := os.Stat(filepath.Join(c.Workspace, markerFile)); err != nil {
			t.Errorf("%s: the home went: %v", name, err)
		}
	}
}
