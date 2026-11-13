package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadProject loads the session of target in root that a run would, with
// no runtime, and the repository's profile called claude-code.
func loadProject(t *testing.T, root, target, session string) *Config {
	t.Helper()
	id := ProjectIdentity(root, target, session, "")
	tg := repoTarget()
	tg.Name, tg.Session, tg.Root = target, session, root
	c, err := Load(repoClaude(t), Options{Identity: &id, Policies: &PolicySet{}, Target: tg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// bootProject does what the run of a target will do to its state before the
// sandbox boots: create the home, leave guest state in it, and record the
// session in both indexes.
func bootProject(t *testing.T, c *Config) ProjectSession {
	t.Helper()
	bootHome(t, c)
	ps := ProjectSession{Sandbox: c.VMName, Root: c.target.Root, Target: c.target.Name,
		Session: c.target.Session, Home: c.Workspace}
	if err := RecordProject(ps); err != nil {
		t.Fatal(err)
	}
	return ps
}

// guestState is the file bootHome leaves in a home.
func guestState(home string) string { return filepath.Join(home, ".local", "bin", "claude") }

// A repository profile called claude-code, run as a target, records nothing
// under your claude-code session and touches nothing of it. Your session's
// entry and home are what they were, and its key is the only ref completion
// offers.
func TestASessionOfATargetLeavesYourProfileSessionAlone(t *testing.T) {
	isolateHome(t)
	yours := mustLoad(t, Options{})
	bootHome(t, yours)

	c := loadProject(t, "/src/a", "dev", "")
	bootProject(t, c)
	if c.VMName == yours.VMName || c.Workspace == yours.Workspace {
		t.Fatalf("the target shares %s or %s with your session", c.VMName, c.Workspace)
	}
	if refs := SessionRefs(); len(refs) != 1 || refs[0] != "claude-code" {
		t.Errorf("SessionRefs = %q, want only your claude-code: no @p/ key is a ref", refs)
	}
	if _, ok := LookupProject(yours.VMName); ok {
		t.Error("your session is in the project index")
	}

	c.Runtime = &removingRuntime{}
	if err := c.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(guestState(yours.Workspace)); err != nil {
		t.Errorf("removing the target deleted your home: %v", err)
	}
	if home, _ := rememberedWorkspace("claude-code", yours.VMName); home != yours.Workspace {
		t.Errorf("removing the target dropped your session's entry: %q", home)
	}
}

// rm of a target's session removes its sandbox, its home and both its index
// entries, and leaves another target's sandbox, home and entry as they were.
func TestRemovingATargetSessionLeavesTheOtherTarget(t *testing.T) {
	isolateHome(t)
	dev, ci := loadProject(t, "/src/a", "dev", ""), loadProject(t, "/src/a", "ci", "")
	bootProject(t, dev)
	ciEntry := bootProject(t, ci)

	rt := &removingRuntime{}
	dev.Runtime = rt
	if err := dev.Remove(); err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != dev.VMName {
		t.Errorf("removed %q, want only %s", rt.removed, dev.VMName)
	}
	if dev.RemovedHome != dev.Workspace {
		t.Errorf("RemovedHome = %q, want %q", dev.RemovedHome, dev.Workspace)
	}
	if _, err := os.Lstat(dev.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dev's home is still there: %v", err)
	}
	if _, ok := LookupProject(dev.VMName); ok {
		t.Error("dev's project entry is still there")
	}
	if WorkspaceOfSandbox(dev.VMName) != "" {
		t.Error("dev's session entry is still there")
	}
	if got, ok := LookupProject(ci.VMName); !ok || got != ciEntry {
		t.Errorf("ci's entry is %+v (%v), want %+v", got, ok, ciEntry)
	}
	if _, err := os.Stat(guestState(ci.Workspace)); err != nil {
		t.Errorf("ci's home went with dev: %v", err)
	}
}

// A removal the runtime refused keeps the home and the project entry: the
// sandbox may still have the home mounted, and the entry is what says so.
func TestAFailedRemovalKeepsTheProjectEntryAndHome(t *testing.T) {
	isolateHome(t)
	c := loadProject(t, "/src/a", "dev", "")
	bootProject(t, c)
	c.Runtime = &failingRuntime{}
	if err := c.Remove(); err == nil {
		t.Fatal("a failed removal was reported as success")
	}
	if _, ok := LookupProject(c.VMName); !ok {
		t.Error("the project entry went with a sandbox that is still there")
	}
	if _, err := os.Stat(guestState(c.Workspace)); err != nil {
		t.Errorf("the home went with a sandbox that is still there: %v", err)
	}
}

// When the project entry cannot be dropped, the home stays too. Dropping the
// home alone would leave an entry that names a home that is gone.
func TestAHomeStaysWhenItsProjectEntryCannotBeDropped(t *testing.T) {
	state, _ := isolateHome(t)
	c := loadProject(t, "/src/a", "dev", "")
	bootProject(t, c)
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	home, err := DropEphemeralHome(c.VMName)
	if err == nil || home != "" {
		t.Fatalf("DropEphemeralHome = %q, %v, want an error and no home", home, err)
	}
	if _, err := os.Stat(guestState(c.Workspace)); err != nil {
		t.Errorf("the home was deleted while its entry stayed: %v", err)
	}
}

// `brig ls` prunes sessions.json against a listing that may leave out a
// stopped sandbox, and hull says "instance not found" for a sandbox whose
// metadata it cannot read. Neither may cost a project session its home or its
// entry, and a boot that fails on that home keeps it too.
func TestPruningAndReapingNeverTakeAProjectHome(t *testing.T) {
	isolateHome(t)
	c := loadProject(t, "/src/a", "dev", "")
	entry := bootProject(t, c)
	PruneSessions(nil)
	if WorkspaceOfSandbox(c.VMName) != "" {
		t.Fatal("the prune kept the sessions.json entry, so this case tests nothing")
	}

	next := loadProject(t, "/src/a", "dev", "")
	if next.homeRemembered {
		t.Fatal("the home is remembered through sessions.json, so this case tests nothing")
	}
	next.Runtime = &listingRuntime{}
	next.reapOrphanHome()
	if _, err := os.Stat(guestState(next.Workspace)); err != nil {
		t.Errorf("the reap deleted a project home: %v", err)
	}
	failBoot(t, next, &bootFailRuntime{})
	if _, err := os.Stat(guestState(next.Workspace)); err != nil {
		t.Errorf("a failed boot deleted a project home: %v", err)
	}
	if got, ok := LookupProject(c.VMName); !ok || got != entry {
		t.Errorf("the project entry is %+v (%v), want %+v", got, ok, entry)
	}
}

// Once the project entry is gone, the home of a sandbox the runtime does not
// have is an orphan like any other, and the next boot deletes it.
func TestAProjectHomeWithNoEntryIsReapedLikeAnyOther(t *testing.T) {
	isolateHome(t)
	c := loadProject(t, "/src/a", "dev", "")
	bootProject(t, c)
	PruneSessions(nil)
	if _, err := ForgetProject(c.VMName); err != nil {
		t.Fatal(err)
	}
	next := loadProject(t, "/src/a", "dev", "")
	next.Runtime = &listingRuntime{}
	next.Err, next.Verbosity = &bytes.Buffer{}, Normal
	next.reapOrphanHome()
	if _, err := os.Lstat(next.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an orphaned project home survived: %v", err)
	}
}

// ProjectConfig reaches a recorded session with nothing but its entry: the
// sandbox, the home, the project rm says it keeps, and a stop whose failure
// names a command that works.
func TestProjectConfigIsTheRecordedSession(t *testing.T) {
	isolateHome(t)
	loaded := loadProject(t, "/src/a", "dev", "fix")
	entry := bootProject(t, loaded)

	rt := &stubRuntime{stopErr: errors.New("busy"), running: true}
	c := ProjectConfig(entry, rt, Normal)
	if c.VMName != loaded.VMName || c.Workspace != loaded.Workspace || c.KeptProject() != "/src/a" {
		t.Errorf("ProjectConfig = %s %s %s, want %s %s /src/a",
			c.VMName, c.Workspace, c.KeptProject(), loaded.VMName, loaded.Workspace)
	}
	if c.sessionRef() != loaded.sessionRef() {
		t.Errorf("key %s, want %s", c.sessionRef(), loaded.sessionRef())
	}
	err := c.Stop()
	if err == nil {
		t.Fatal("a stop that failed was reported as success")
	}
	if strings.Contains(err.Error(), "brig logs") || !strings.Contains(err.Error(), "hull logs "+c.VMName) {
		t.Errorf("the stop failure names a log command no verb takes: %v", err)
	}
}

// Load refuses a target whose project holds brig's state directory, or lies
// inside it, before it reads any of that state.
func TestLoadRefusesATargetWhoseProjectHoldsTheState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", filepath.Join(root, ".brig"))
	id := ProjectIdentity(root, "dev", "", "")
	tg := repoTarget()
	tg.Root = root
	_, err := Load(repoClaude(t), Options{Identity: &id, Policies: &PolicySet{}, Target: tg}, nil)
	if err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Errorf("Load: %v", err)
	}
}
