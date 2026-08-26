package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// isolateState points brig's state directory at a scratch one and clears the
// workspace settings, so a case runs against the index it writes itself rather
// than against whatever the machine running the test happens to have.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	// Set, then removed. t.Setenv is what restores the caller's own value when
	// the case ends, and it cannot unset -- but a variable present and empty is
	// not the same as an absent one here: Get takes the first prefix that
	// exists, so an empty BRIG_CLAUDE_CODE_WORKSPACE would mask a
	// BRIG_WORKSPACE a case sets deliberately.
	for _, key := range []string{
		"BRIG_WORKSPACE", "BRIG_CLAUDE_CODE_WORKSPACE",
		"BRIG_NAME", "BRIG_CLAUDE_CODE_NAME",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// mustLoad resolves one invocation the way the command line does. These cases
// go through Load rather than building a Config, because the resolution order
// is the thing being tested.
func mustLoad(t *testing.T, o Options) *Config {
	t.Helper()
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("the claude-code profile is not loaded")
	}
	c, err := Load(p, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// removingRuntime answers the two calls Remove makes and nothing else:
// anything further panics through the embedded nil interface, which is louder
// than a stub that quietly succeeds.
type removingRuntime struct {
	runtime.Runtime
	removed []string
}

func (r *removingRuntime) Stop(string) error { return nil }

func (r *removingRuntime) Remove(name string) error {
	r.removed = append(r.removed, name)
	return nil
}

// The transcript in issue #72: a session created with --workspace, then a verb
// that names none. Before the index, the second one resolved the default
// ~/brig/<profile>-<name>, found the running sandbox mounting something else,
// called that a stale share and restarted it -- discarding the guest's
// memory-only state, an in-sandbox login included.
func TestAWorkspaceGivenOnceIsFoundAgainWithoutTheFlag(t *testing.T) {
	isolateState(t)
	base := t.TempDir()
	// --workspace names the base, and a named session suffixes its slug onto
	// whatever the base is, so this is the directory the session actually gets.
	ws := base + "-rc23test"

	created := mustLoad(t, Options{Name: "rc23test", Workspace: base})
	if created.Workspace != ws {
		t.Fatalf("--workspace resolved to %q, want %q", created.Workspace, ws)
	}
	created.rememberWorkspace()

	next := mustLoad(t, Options{Name: "rc23test"})
	if next.VMName != created.VMName {
		t.Fatalf("the two invocations addressed different sandboxes: %q and %q",
			created.VMName, next.VMName)
	}
	if next.Workspace != ws {
		t.Errorf("a flagless verb resolved %q, want the workspace the session was "+
			"created with (%q)", next.Workspace, ws)
	}
}

// An explicit directory is a request, not a guess, so it still beats what was
// recorded -- and still restarts the sandbox, because a share cannot be moved
// on a live guest. Both spellings, since either one is the user saying it.
func TestAnExplicitWorkspaceBeatsTheRememberedOne(t *testing.T) {
	isolateState(t)
	remembered, other := t.TempDir(), t.TempDir()

	created := mustLoad(t, Options{Name: "rc23test", Workspace: remembered})
	created.rememberWorkspace()

	byFlag := mustLoad(t, Options{Name: "rc23test", Workspace: other})
	if want := other + "-rc23test"; byFlag.Workspace != want {
		t.Errorf("--workspace resolved to %q, want %q", byFlag.Workspace, want)
	}

	// Either spelling names the base rather than the session's own directory,
	// so the slug is suffixed onto it exactly as it is without an index.
	t.Setenv("BRIG_WORKSPACE", other)
	bySetting := mustLoad(t, Options{Name: "rc23test"})
	if want := other + "-rc23test"; bySetting.Workspace != want {
		t.Errorf("BRIG_WORKSPACE resolved to %q, want %q", bySetting.Workspace, want)
	}
}

// An unnamed run records and reads back its own entry too: the default
// workspace is one a flag can override just as well.
func TestTheUnnamedSandboxIsRememberedToo(t *testing.T) {
	isolateState(t)
	ws := t.TempDir()

	created := mustLoad(t, Options{Workspace: ws})
	if created.VMName != "brig-claude-code" {
		t.Fatalf("the unnamed sandbox is %q", created.VMName)
	}
	created.rememberWorkspace()

	if got := mustLoad(t, Options{}).Workspace; got != ws {
		t.Errorf("a flagless verb resolved %q, want %q", got, ws)
	}
}

// `brig rm` drops the entry, so the next sandbox to take the name resolves its
// workspace the ordinary way rather than inheriting a directory chosen for a
// sandbox that is gone.
func TestRemoveDropsTheEntry(t *testing.T) {
	isolateState(t)
	base := t.TempDir()
	ws := base + "-rc23test"

	c := mustLoad(t, Options{Name: "rc23test", Workspace: base})
	c.rememberWorkspace()
	if got := RememberedWorkspace(c.VMName); got != ws {
		t.Fatalf("the workspace was recorded as %q, want %q", got, ws)
	}

	rt := &removingRuntime{}
	c.Runtime = rt
	if err := c.Remove(); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != c.VMName {
		t.Errorf("the runtime was asked to remove %v", rt.removed)
	}
	if got := RememberedWorkspace(c.VMName); got != "" {
		t.Errorf("rm left %q recorded for a sandbox it removed", got)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got == ws {
		t.Errorf("a later run still resolved the removed sandbox's workspace %q", got)
	}
}

// `brig reset` works from the runtime's instance list, so it prunes by name
// without a Config -- the entry has to go on that path too, or a reset leaves
// the index naming every sandbox it just removed.
func TestForgetWorkspacePrunesByName(t *testing.T) {
	isolateState(t)
	ws := t.TempDir()

	c := mustLoad(t, Options{Name: "rc23test", Workspace: ws})
	c.rememberWorkspace()

	// A name that was never recorded is not an error: reset walks every brig
	// sandbox, including ones created before the index existed.
	ForgetWorkspace("brig-claude-code-neverseen")
	ForgetWorkspace(c.VMName)

	if got := RememberedWorkspace(c.VMName); got != "" {
		t.Errorf("reset left %q recorded for %s", got, c.VMName)
	}
}

// The index is bookkeeping, so nothing about it is worth failing a command
// over: a missing file, a corrupt one and one that cannot be opened at all
// each resolve the default, which is what the release before it did.
func TestAnUnusableIndexIsIgnoredRatherThanFatal(t *testing.T) {
	dir := isolateState(t)
	path := filepath.Join(dir, workspaceIndexName)

	// Missing: nothing has been recorded yet, which is every first run.
	fallback := mustLoad(t, Options{Name: "rc23test"}).Workspace
	if fallback == "" {
		t.Fatal("no workspace was resolved at all")
	}

	// Corrupt: half a write, or a file somebody edited.
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RememberedWorkspace("brig-claude-code-rc23test"); got != "" {
		t.Errorf("a corrupt index returned %q", got)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got != fallback {
		t.Errorf("a corrupt index changed the resolved workspace to %q, want %q", got, fallback)
	}

	// Unreadable: a directory where the file should be is the shape that takes
	// on a host where the read cannot succeed whatever the permissions say.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got != fallback {
		t.Errorf("an unreadable index changed the resolved workspace to %q, want %q", got, fallback)
	}
	// And recording against one says so rather than failing the run: the
	// sandbox is up, and the note is what explains the restart that follows.
	c := mustLoad(t, Options{Name: "rc23test"})
	said := &bytes.Buffer{}
	c.Err = said
	c.rememberWorkspace()
	if !strings.Contains(said.String(), c.VMName) {
		t.Errorf("an index that cannot be written said %q, which does not name the sandbox",
			said.String())
	}
}

// Every entry survives a write, so recording one sandbox does not cost another
// its own -- the file is read, edited and written back whole.
func TestRecordingOneSandboxKeepsTheOthers(t *testing.T) {
	isolateState(t)
	first, second := t.TempDir(), t.TempDir()

	a := mustLoad(t, Options{Name: "one", Workspace: first})
	a.rememberWorkspace()
	b := mustLoad(t, Options{Name: "two", Workspace: second})
	b.rememberWorkspace()

	if want := first + "-one"; RememberedWorkspace(a.VMName) != want {
		t.Errorf("%s lost its entry: %q", a.VMName, RememberedWorkspace(a.VMName))
	}
	if want := second + "-two"; RememberedWorkspace(b.VMName) != want {
		t.Errorf("%s was recorded as %q, want %q", b.VMName, RememberedWorkspace(b.VMName), want)
	}
}
