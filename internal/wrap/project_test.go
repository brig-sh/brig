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

// The project a run names is mounted at /work/<basename>, OUTSIDE the guest
// home, and the agent starts in it.
//
// Outside is the whole property: the guest home is the agent's home, dotfiles
// and state included, so a project directory landing under it could be
// mistaken for home state or collide with the agent's own files. Under /work
// it cannot, by layout rather than by convention.
func TestLoadMountsANamedProjectOutsideTheGuestHome(t *testing.T) {
	isolateState(t)
	home, project := t.TempDir(), filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	c := mustLoad(t, Options{Workspace: home, Project: project})

	if c.Project != project {
		t.Errorf("Project = %q, want %q", c.Project, project)
	}
	if want := "/work/myproject"; c.GuestProject != want {
		t.Errorf("GuestProject = %q, want %q", c.GuestProject, want)
	}
	if c.GuestCwd != c.GuestProject {
		t.Errorf("GuestCwd = %q, want the project at %q", c.GuestCwd, c.GuestProject)
	}
	if strings.HasPrefix(c.GuestProject, c.Profile.GuestHome+"/") {
		t.Errorf("the project landed inside the guest home: %q is under %q",
			c.GuestProject, c.Profile.GuestHome)
	}
	// The home is the session's, and naming a project does not move it.
	if c.Workspace != home {
		t.Errorf("Workspace = %q, want the home %q", c.Workspace, home)
	}
}

// A relative project is resolved against the directory the command was invoked
// from, because `brig run claude .` is the line this exists for.
func TestARelativeProjectIsResolvedAgainstTheCwd(t *testing.T) {
	isolateState(t)
	c := mustLoad(t, Options{Workspace: t.TempDir(), Project: "."})

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != cwd {
		t.Errorf("Project = %q, want the cwd %q", c.Project, cwd)
	}
	if want := "/work/" + filepath.Base(cwd); c.GuestProject != want {
		t.Errorf("GuestProject = %q, want %q", c.GuestProject, want)
	}
}

// The regression that matters most: with no project named, nothing moves. The
// guest working directory is derived from the cwd and the home exactly as it
// was before the positional existed, and no second mount is resolved.
func TestLoadWithoutAProjectIsUnchanged(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	c := mustLoad(t, Options{Workspace: home})

	if c.Project != "" || c.GuestProject != "" {
		t.Errorf("a run that named no project resolved one: %q at %q", c.Project, c.GuestProject)
	}
	if want := GuestCwd(c.Cwd, c.Workspace, c.Profile.GuestHome); c.GuestCwd != want {
		t.Errorf("GuestCwd = %q, want the cwd-under-home derivation %q", c.GuestCwd, want)
	}
}

// A word that names no directory is refused, and the refusal names the escape.
//
// This is the breaking change made legible: the word after the ref used to
// reach the agent, so a line that passed one through now fails here rather
// than mounting something that is not there. The message has to carry the way
// past it, which is `--`.
func TestAProjectThatIsNotADirectoryIsRefused(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("the claude-code profile is not loaded")
	}
	for _, project := range []string{filepath.Join(dir, "absent"), file} {
		_, err := Load(p, Options{Workspace: dir, Project: project}, nil)
		if err == nil {
			t.Errorf("%q was accepted as a project", project)
			continue
		}
		if !strings.Contains(err.Error(), project) {
			t.Errorf("the refusal does not name the directory: %v", err)
		}
		if !strings.Contains(err.Error(), "--") {
			t.Errorf("the refusal does not name the way past it: %v", err)
		}
	}

	// The filesystem root is a directory and still has no name to mount it
	// under, so it is refused on its own terms rather than becoming /work//.
	if _, err := Load(p, Options{Workspace: dir, Project: "/"}, nil); err == nil {
		t.Error("/ was accepted as a project")
	} else if !strings.Contains(err.Error(), "filesystem root") {
		t.Errorf("the refusal does not say why / cannot be mounted: %v", err)
	}
}

// The refusal names the link's target, because typing that target is the way
// past it. A link of the user's own is refused as well: brig cannot tell one
// from a planted one, so the rule is about links, not about who made them.
func TestALinkAtTheProjectIsRefusedAndNamesItsTarget(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()

	root := filepath.Join(dir, "myroot")
	if err := os.Symlink("/", root); err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if err := c.mountProject(root); err == nil {
		t.Fatalf("a link to / was accepted, and mounted at %q", c.GuestProject)
	} else if !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not say what was typed and why it fails: %v", err)
	}

	// An ordinary link is refused on the same terms, and the message carries
	// the target.
	target := filepath.Join(dir, "elsewhere")
	mustMkdir(t, target)
	link := filepath.Join(dir, "myproject")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c = &Config{}
	err := c.mountProject(link)
	if err == nil {
		t.Fatalf("a link was accepted as a project, exporting %q", c.Project)
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the refusal is not a planted-link refusal: %v", err)
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("the refusal does not name the target to type instead: %v", err)
	}

	// The real directory is what mounts, under its own name.
	c = &Config{}
	mustMountProject(t, c, target)
	if want := "/work/elsewhere"; c.GuestProject != want {
		t.Errorf("the project mounted at %q, want %q", c.GuestProject, want)
	}
	if c.Project != target {
		t.Errorf("the share exports %q, want %q", c.Project, target)
	}
}

// The link need not be the last component. A sandbox given a monorepo
// read-write can replace any directory under it, so refusing only the last
// component would leave the same escape one directory deeper.
func TestALinkOnTheWayToTheProjectIsRefused(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()

	escape := filepath.Join(dir, "escape-target")
	mustMkdir(t, escape)
	mustMkdir(t, filepath.Join(escape, "src"))

	monorepo := filepath.Join(dir, "monorepo")
	mustMkdir(t, monorepo)
	planted := filepath.Join(monorepo, "frontend")
	if err := os.Symlink(escape, planted); err != nil {
		t.Fatal(err)
	}

	c := &Config{}
	err := c.mountProject(filepath.Join(planted, "src"))
	if err == nil {
		t.Fatalf("a project reached through a planted link was accepted, exporting %q", c.Project)
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the refusal is not a planted-link refusal: %v", err)
	}
	if !strings.Contains(err.Error(), planted) {
		t.Errorf("the refusal does not name the link on the way: %v", err)
	}
}

// The property that matters is the share string the VMM resolves, not the
// refusal above it. This drives a run and reads the spec the runtime got, so a
// change that lets the path through mountProject still fails here.
func TestAPlantedLinkNeverReachesTheRuntimeAsAShare(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)
	dir := t.TempDir()

	escape := filepath.Join(dir, "escape-target")
	mustMkdir(t, escape)
	monorepo := filepath.Join(dir, "monorepo")
	mustMkdir(t, monorepo)

	// The operator's first run gives the agent the monorepo.
	frontend := filepath.Join(monorepo, "frontend")
	mustMkdir(t, frontend)
	mustMountProject(t, c, frontend)

	// The agent's one move, with the write access it was given.
	// The link dangles in the guest and means something only on the host.
	if err := os.Remove(frontend); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escape, frontend); err != nil {
		t.Fatal(err)
	}

	// The operator's second run, naming the same path as before. Either check
	// may refuse it: mountProject when the path is read, or the one at boot.
	c2 := livenessConfig(t, rt)
	err := c2.mountProject(frontend)
	if err == nil {
		err = c2.EnsureRunning(creds.Set{})
	}
	for _, s := range rt.spec.Shares {
		if s.Host == frontend || s.Host == escape {
			t.Fatalf("the runtime was handed %q as a share, which resolves to %q",
				s.Host, escape)
		}
	}
	if err == nil {
		t.Fatal("a planted link was accepted as a project")
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the run was refused, but not for the planted link: %v", err)
	}
	if rt.boots != 0 {
		t.Errorf("booted %d sandboxes over a planted link", rt.boots)
	}
}

// mountProject reads the path when the run is set up, and the sandbox can swap
// a component before the boot hands the path over. The check in EnsureRunning
// is the one that covers that window, so the link is planted after
// mountProject accepted the real directory.
func TestALinkPlantedAfterTheProjectWasReadIsRefusedAtBoot(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)
	dir := t.TempDir()

	escape := filepath.Join(dir, "escape-target")
	mustMkdir(t, escape)
	project := filepath.Join(dir, "project")
	mustMkdir(t, project)
	mustMountProject(t, c, project)

	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escape, project); err != nil {
		t.Fatal(err)
	}

	err := c.EnsureRunning(creds.Set{})
	if err == nil {
		t.Fatal("the boot handed over a project that became a link after it was read")
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the boot was refused, but not for the planted link: %v", err)
	}
	if rt.boots != 0 {
		t.Errorf("booted %d sandboxes over a planted link", rt.boots)
	}
}

// /tmp is a link to /private/tmp on every macOS host. A component whose parent
// this user cannot write is one the guest cannot redirect, so it is opened by
// name and the kernel follows it. Only the part below the first writable
// directory is walked link by link, and a planted link is always there.
func TestALinkAboveTheTrustedSplitIsFollowed(t *testing.T) {
	isolateState(t)

	info, err := os.Lstat("/tmp")
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Skip("/tmp is not a symlink on this host, so there is no prefix link to follow")
	}
	project, err := os.MkdirTemp("/tmp", "brig-prefix-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(project) }()

	c := &Config{}
	if err := c.mountProject(project); err != nil {
		t.Fatalf("a project under the symlinked /tmp was refused: %v", err)
	}
	// The share and the guest path keep the spelling that was typed, so /tmp
	// stays /tmp.
	if c.Project != project {
		t.Errorf("the share exports %q, want the path as typed %q", c.Project, project)
	}
	if want := "/work/" + filepath.Base(project); c.GuestProject != want {
		t.Errorf("the project mounted at %q, want %q", c.GuestProject, want)
	}
	// The reader is shown the resolved directory. A link above the split is the
	// one case where the two differ, and the system put that one there.
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProjectReal != real {
		t.Errorf("the PROJECT row reads %q, want the resolved %q", c.ProjectReal, real)
	}
	if c.ProjectReal == c.Project {
		t.Error("the resolved path equals the typed one, so this host proves nothing")
	}
}

// asRoot makes trustedPrefix take its root path for the rest of the test.
func asRoot(t *testing.T) {
	t.Helper()
	old := runningAsRoot
	t.Cleanup(func() { runningAsRoot = old })
	runningAsRoot = func() bool { return true }
}

// A sandbox run as root writes to the host as root, so a directory it had
// read-write looks just like one of the machine's own: root-owned, no group
// or other write bit. trustDirs makes the monorepo look like that. The link is
// on the way to the project, not the project itself, because a link at the
// project is refused whatever the prefix.
func TestAsRootALinkInARootOwnedProjectIsRefused(t *testing.T) {
	isolateState(t)
	asRoot(t)
	dir := t.TempDir()

	escape := filepath.Join(dir, "escape-target")
	mustMkdir(t, escape)
	mustMkdir(t, filepath.Join(escape, "src"))
	monorepo := filepath.Join(dir, "monorepo")
	mustMkdir(t, monorepo)
	planted := filepath.Join(monorepo, "frontend")
	if err := os.Symlink(escape, planted); err != nil {
		t.Fatal(err)
	}
	trustDirs(t, monorepo, escape)

	c := &Config{}
	err := c.mountProject(filepath.Join(planted, "src"))
	if err == nil {
		t.Fatalf("as root, a link in a root-owned directory was followed, exporting %q", c.Project)
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the refusal is not a planted-link refusal: %v", err)
	}
}

// The other half: as root, the entries of / are still trusted, so the links
// the machine is made of keep working.
func TestAsRootALinkAtTheTopOfThePathIsFollowed(t *testing.T) {
	isolateState(t)
	asRoot(t)

	info, err := os.Lstat("/tmp")
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Skip("/tmp is not a symlink on this host, so there is no prefix link to follow")
	}
	project, err := os.MkdirTemp("/tmp", "brig-prefix-root-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(project) }()

	c := &Config{}
	if err := c.mountProject(project); err != nil {
		t.Fatalf("as root, a project under the symlinked /tmp was refused: %v", err)
	}
	if c.Project != project {
		t.Errorf("the share exports %q, want the path as typed %q", c.Project, project)
	}
}

// The project is a SECOND share. The home share is what makes the session the
// session, so it stays exactly where it was -- first, and mounting the
// workspace at the guest home.
func TestTheProjectIsASecondShareAndTheHomeIsUnchanged(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)
	mustMountProject(t, c, project)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	want := []runtime.Share{
		{Host: c.Workspace, Guest: c.Profile.GuestHome},
		{Host: project, Guest: "/work/myproject"},
	}
	if len(rt.spec.Shares) != len(want) {
		t.Fatalf("shares = %+v, want %+v", rt.spec.Shares, want)
	}
	for i, w := range want {
		if rt.spec.Shares[i] != w {
			t.Errorf("share %d = %+v, want %+v", i, rt.spec.Shares[i], w)
		}
	}
}

// And with no project the mount set is the one share it always was.
func TestWithoutAProjectTheMountSetIsUnchanged(t *testing.T) {
	rt := &livenessRuntime{}
	c := livenessConfig(t, rt)

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	want := []runtime.Share{{Host: c.Workspace, Guest: c.Profile.GuestHome}}
	if len(rt.spec.Shares) != 1 || rt.spec.Shares[0] != want[0] {
		t.Errorf("shares = %+v, want %+v", rt.spec.Shares, want)
	}
}

// A share cannot be attached to a live sandbox -- shares are --shared-dir
// arguments to `hull run`, and the Runtime interface has no add-share method --
// so a run that names a different project has to recreate the sandbox. That is
// cheap precisely because the home is a host directory which survives it.
func TestASandboxIsRecreatedWhenTheProjectChanges(t *testing.T) {
	rt := &livenessRuntime{running: true}
	c := livenessConfig(t, rt)
	first := filepath.Join(t.TempDir(), "first")
	mustMkdir(t, first)
	mustMountProject(t, c, first)

	// A sandbox brig has no project recorded for is not one it can assume has
	// this project mounted, so the first run on a project recreates it. Wrong
	// in the cheap direction: a boot that was not needed, rather than an agent
	// working in a directory nothing mounted.
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}
	if rt.boots != 1 {
		t.Fatalf("a running sandbox with no project recorded booted %d times, want 1", rt.boots)
	}

	// The same project again: nothing to change, so nothing is torn down.
	rt.boots, rt.stops, rt.removes = 0, 0, 0
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	if rt.boots != 0 || rt.stops != 0 || rt.removes != 0 {
		t.Errorf("the same project restarted the sandbox (%d boots, %d stops, %d removes)",
			rt.boots, rt.stops, rt.removes)
	}

	second := filepath.Join(t.TempDir(), "second")
	mustMkdir(t, second)
	mustMountProject(t, c, second)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run on a new project failed: %v", err)
	}
	if rt.boots != 1 {
		t.Errorf("a new project booted %d sandboxes, want 1", rt.boots)
	}
	if rt.stops == 0 || rt.removes == 0 {
		t.Errorf("the old sandbox was not torn down (%d stops, %d removes)", rt.stops, rt.removes)
	}
	if rt.spec.Shares[1].Host != second {
		t.Errorf("the recreated sandbox mounts %q, want %q", rt.spec.Shares[1].Host, second)
	}

	// And the other direction: a later run of the same session that names no
	// project has to lose the mount, or the agent would keep a host directory
	// the line said nothing about. These are the fields Load leaves empty for
	// such a run.
	rt.boots, rt.stops, rt.removes = 0, 0, 0
	c.Project, c.GuestProject = "", ""
	c.GuestCwd = GuestCwd(c.Cwd, c.Workspace, c.Profile.GuestHome)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run naming no project failed: %v", err)
	}
	if rt.boots != 1 {
		t.Errorf("dropping the project booted %d sandboxes, want 1", rt.boots)
	}
	if len(rt.spec.Shares) != 1 {
		t.Errorf("the recreated sandbox still has %+v mounted", rt.spec.Shares)
	}
}

// Trust follows the project. The key is the guest directory the agent will be
// in, so with a project mounted it is the project -- otherwise the agent starts
// in /work/<basename> and asks whether you trust a directory brig just handed
// it.
func TestTheTrustKeyFollowsTheProject(t *testing.T) {
	home := t.TempDir()
	c := testConfig(t, home, home)
	if want := TrustKey(c.Cwd, c.Workspace, c.Profile.GuestHome); c.trustKey() != want {
		t.Errorf("without a project the key is %q, want the home's %q", c.trustKey(), want)
	}

	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, filepath.Join(project, ".git"))
	mustMountProject(t, c, project)
	// A repository root, and the mount root, and the directory the agent
	// starts in: all three are the project, so the key is its guest path.
	if want := "/work/myproject"; c.trustKey() != want {
		t.Errorf("with a project the key is %q, want %q", c.trustKey(), want)
	}
}

// The index records the project the session last ran with, and leaves the key
// out when there was none: most sessions have no project, and the field is a
// record of the last invocation rather than part of the session's identity.
func TestTheIndexRecordsTheProjectAndOmitsItWhenAbsent(t *testing.T) {
	dir := isolateState(t)
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: t.TempDir(), Project: project}).rememberSession()
	if blob := mustReadIndex(t, dir); !strings.Contains(blob, `"project": "`+project+`"`) {
		t.Errorf("the index does not record the project:\n%s", blob)
	}

	bare := isolateState(t)
	mustLoad(t, Options{Workspace: t.TempDir()}).rememberSession()
	if blob := mustReadIndex(t, bare); strings.Contains(blob, "project") {
		t.Errorf("a session with no project still has the key:\n%s", blob)
	}
}

// mustMountProject applies to a hand-built Config what Load does for a run
// that named a project, so a case about the mount does not have to go through
// the whole of resolution to get one.
func mustMountProject(t *testing.T, c *Config, dir string) {
	t.Helper()
	if err := c.mountProject(dir); err != nil {
		t.Fatal(err)
	}
}

// mustReadIndex is the session index as it was written, read back as text: the
// cases above are about the shape of the file, `project` present or absent,
// which a decoded struct cannot tell apart from a zero value.
func mustReadIndex(t *testing.T, dir string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(dir, sessionIndexName))
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

// The defect real-runtime testing found. A session created with a project and
// then addressed by a verb that names none -- `brig sh claude@x`, or a bare
// `brig run` -- presented as project-less: the mount was destroyed, the guest's
// in-memory state with it, and the index entry was overwritten without the key.
//
// The home already worked this way and the project did not. An invocation that
// names no directory is not asking for the default one, so the path the sandbox
// was started with is read back -- exactly what rememberedWorkspace does for
// the home, and for the reason index.go's header gives.
func TestAProjectGivenOnceIsFoundAgainWithoutThePositional(t *testing.T) {
	dir := isolateState(t)
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()

	next := mustLoad(t, Options{})
	if next.Project != project {
		t.Errorf("a flagless verb resolved the project %q, want %q", next.Project, project)
	}
	if want := "/work/myproject"; next.GuestProject != want || next.GuestCwd != want {
		t.Errorf("guest project %q and cwd %q, want %q for both",
			next.GuestProject, next.GuestCwd, want)
	}
	// So the running sandbox is carrying what this run wants, and nothing is
	// torn down.
	if stale := next.projectShareStale(); stale != "" {
		t.Errorf("a flagless verb read the sandbox as stale: %s", stale)
	}
	// And recording again keeps the key rather than dropping it, which is what
	// the flagless verb was doing to the entry.
	next.rememberSession()
	if blob := mustReadIndex(t, dir); !strings.Contains(blob, `"project": "`+project+`"`) {
		t.Errorf("a flagless verb dropped the project from the index:\n%s", blob)
	}
}

// An explicit positional still beats what was recorded, and still recreates:
// asking for a different directory is something a user is entitled to do,
// restart and all. Same rule the home follows.
func TestAnExplicitProjectBeatsTheRememberedOne(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	mustMkdir(t, first)
	mustMkdir(t, second)

	mustLoad(t, Options{Workspace: home, Project: first}).rememberSession()

	next := mustLoad(t, Options{Workspace: home, Project: second})
	if next.Project != second {
		t.Fatalf("the positional resolved %q, want %q", next.Project, second)
	}
	if stale := next.projectShareStale(); stale == "" {
		t.Error("a different project did not read as stale, so the sandbox would keep the old mount")
	} else if !strings.Contains(stale, first) || !strings.Contains(stale, second) {
		t.Errorf("the reason names neither directory clearly: %s", stale)
	}
}

// A remembered project that has since gone is dropped rather than refused. This
// line named no directory, so there is nothing here for the user to fix; the
// recreate that follows is what tells them the mount went, and it names the
// directory.
func TestARememberedProjectThatIsGoneIsDropped(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}

	next := mustLoad(t, Options{})
	if next.Project != "" {
		t.Errorf("a project that is no longer there was mounted: %q", next.Project)
	}
	if want := GuestCwd(next.Cwd, next.Workspace, next.Profile.GuestHome); next.GuestCwd != want {
		t.Errorf("GuestCwd = %q, want the home derivation %q", next.GuestCwd, want)
	}
	if stale := next.projectShareStale(); stale == "" {
		t.Error("the sandbox still mounting the vanished project did not read as stale")
	}
}

// A session remembered from before links were refused can name its project
// through one. Dropping that refusal like a vanished directory would restart
// the running sandbox without its project and never say why. Refusing it in
// Load would lock the session away from stop and rm, so the boot refuses it.
func TestARememberedProjectReachedThroughALinkRefusesTheRun(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	mustMkdir(t, target)
	project := filepath.Join(dir, "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, project); err != nil {
		t.Fatal(err)
	}

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("the claude-code profile is not loaded")
	}
	// Load itself goes through: stop, rm, info and logs load the session too,
	// and none of them can name a directory or take --no-project.
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatalf("loading the session was refused, so nothing can reach it by ref: %v", err)
	}
	if c.Project != "" {
		t.Fatalf("the remembered project was mounted through a link: %q", c.Project)
	}
	if c.KeptProject() != project {
		t.Errorf("brig rm would not say the project stays on the host: KeptProject is %q, "+
			"want %q", c.KeptProject(), project)
	}

	rt := &livenessRuntime{workspace: c.Workspace}
	c.Runtime, c.Verify = rt, verify.Off
	err = c.EnsureRunning(creds.Set{})
	if err == nil {
		t.Fatalf("a remembered project reached through a link was dropped, and the run went on "+
			"(%d boots)", rt.boots)
	}
	if !errors.Is(err, errPlantedSymlink) {
		t.Errorf("the refusal is not a planted-link refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "brig run") || !strings.Contains(err.Error(), "--no-project") {
		t.Errorf("the refusal does not say how to go on: %v", err)
	}
	if rt.boots != 0 || rt.removes != 0 {
		t.Errorf("the refused run still booted %d and removed %d sandboxes", rt.boots, rt.removes)
	}

	// brig info has no PROJECT row to show, so it says why. It runs with no
	// runtime here, because the stub answers nothing about isolation.
	c.Runtime = nil
	report, said := &bytes.Buffer{}, &bytes.Buffer{}
	c.Out, c.Err = report, said
	c.Info(creds.Set{})
	if !strings.Contains(said.String(), "remembered from an earlier run") {
		t.Errorf("brig info does not say why the project is missing:\n%s", said.String())
	}
	// A script reads the --json document, not the warning, so the document
	// carries the refusal too.
	doc := c.InfoData(creds.Set{})
	if doc.Project != nil {
		t.Errorf("brig info --json shows a project that was refused: %+v", doc.Project)
	}
	if !strings.Contains(doc.ProjectRefused, "remembered from an earlier run") {
		t.Errorf("brig info --json does not say why the project is missing: %q", doc.ProjectRefused)
	}
}

// The staleness read itself, on the three answers it has. An entry with no
// project against a run that has one is the case that survives the inheritance
// above: nothing was recorded, so nothing establishes that the running sandbox
// has this mount, and recreating is the safe direction.
func TestProjectShareStaleReadsTheIndex(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	// Nothing recorded at all, and a project named: stale.
	c := mustLoad(t, Options{Workspace: home, Project: project})
	if c.projectShareStale() == "" {
		t.Error("an unrecorded session with a project did not read as stale")
	}
	// Nothing recorded and no project named: not stale, which is every
	// ordinary run of every session that has never had one.
	if bare := mustLoad(t, Options{Workspace: home}); bare.projectShareStale() != "" {
		t.Errorf("a session with no project read as stale: %s", bare.projectShareStale())
	}
	// Recorded and unchanged: not stale.
	c.rememberSession()
	if again := mustLoad(t, Options{Workspace: home, Project: project}); again.projectShareStale() != "" {
		t.Errorf("the recorded project read as stale: %s", again.projectShareStale())
	}
}

// --no-project detaches a project a session is carrying, and it is the only
// way to say so: a positional names a directory, and no directory names
// absence. Without it a session handed a project once keeps it for the rest of
// its life, because every later flagless verb inherits it.
func TestNoProjectDropsTheRememberedOne(t *testing.T) {
	dir := isolateState(t)
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()

	next := mustLoad(t, Options{NoProject: true})
	if next.Project != "" || next.GuestProject != "" {
		t.Errorf("--no-project kept project %q (guest %q)", next.Project, next.GuestProject)
	}
	// Back to the home, which is where a run with no project has always
	// started.
	if next.GuestCwd != next.Profile.GuestHome {
		t.Errorf("--no-project left the cwd at %q, want the guest home %q",
			next.GuestCwd, next.Profile.GuestHome)
	}
	// The running sandbox is still carrying the mount this run does not want,
	// so it has to be rebuilt -- projectShareStale's c.Project == "" branch,
	// reached deliberately rather than by an invocation that said nothing.
	if next.projectShareStale() == "" {
		t.Error("--no-project left the sandbox with its project mount standing")
	}
	// And the entry loses the key, so the detach outlives this one command.
	next.rememberSession()
	if blob := mustReadIndex(t, dir); strings.Contains(blob, `"project"`) {
		t.Errorf("--no-project left the project in the index:\n%s", blob)
	}
}

// The detach sticks: the flagless verb after it inherits nothing. This is the
// pair to TestAProjectGivenOnceIsFoundAgainWithoutThePositional -- one proves
// the project survives an invocation that says nothing, the other that it stays
// gone once it has been dropped.
func TestAFlaglessVerbAfterNoProjectInheritsNothing(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	mustMkdir(t, project)

	mustLoad(t, Options{Workspace: home, Project: project}).rememberSession()
	mustLoad(t, Options{NoProject: true}).rememberSession()

	next := mustLoad(t, Options{})
	if next.Project != "" {
		t.Errorf("a flagless verb resurrected the project %q", next.Project)
	}
	if stale := next.projectShareStale(); stale != "" {
		t.Errorf("a flagless verb after a detach read the sandbox as stale: %s", stale)
	}
}

// A session that never had a project is unaffected, so the flag is a no-op
// rather than an error where there is nothing to detach.
func TestNoProjectOnASessionWithoutOneChangesNothing(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	plain := mustLoad(t, Options{Workspace: home})
	with := mustLoad(t, Options{Workspace: home, NoProject: true})

	if with.Project != plain.Project || with.GuestCwd != plain.GuestCwd {
		t.Errorf("--no-project changed a project-less run: project %q cwd %q, want %q and %q",
			with.Project, with.GuestCwd, plain.Project, plain.GuestCwd)
	}
}
