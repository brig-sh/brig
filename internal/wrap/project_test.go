package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
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
