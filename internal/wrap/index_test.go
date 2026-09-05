package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
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

// mustLoadAs resolves one invocation of the profile a spelling reaches. The
// spelling is a parameter because `claude` and `claude-code` are one profile
// through an alias, and a session started under either has to be found under
// the other.
func mustLoadAs(t *testing.T, agent string, o Options) *Config {
	t.Helper()
	p, ok := profile.Lookup(agent)
	if !ok {
		t.Fatalf("the %s profile is not loaded", agent)
	}
	c, err := Load(p, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustLoad resolves one invocation the way the command line does. These cases
// go through Load rather than building a Config, because the resolution order
// is the thing being tested.
func mustLoad(t *testing.T, o Options) *Config {
	t.Helper()
	return mustLoadAs(t, "claude-code", o)
}

// indexKeys is what the file on disk is keyed by, sorted so a case can name
// the keys it expects rather than one of several orders.
func indexKeys(t *testing.T) []string {
	t.Helper()
	index := readSessionIndex()
	keys := make([]string, 0, len(index))
	for key := range index {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// removingRuntime answers the two calls Remove makes and nothing else:
// anything further panics through the embedded nil interface, which is louder
// than a stub that quietly succeeds.
type removingRuntime struct {
	runtime.Runtime
	removed []string
	stopped []string
}

func (r *removingRuntime) Stop(name string) error {
	r.stopped = append(r.stopped, name)
	return nil
}

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
	created.rememberSession()

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
	created.rememberSession()

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
// workspace is one a flag can override just as well. Its ref has no label, so
// it is filed under the bare agent name.
func TestTheUnnamedSandboxIsRememberedToo(t *testing.T) {
	isolateState(t)
	ws := t.TempDir()

	created := mustLoad(t, Options{Workspace: ws})
	if created.VMName != "brig-claude-code" {
		t.Fatalf("the unnamed sandbox is %q", created.VMName)
	}
	created.rememberSession()

	if got := indexKeys(t); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("the unnamed session is filed under %v, want [claude-code]", got)
	}
	if got := mustLoad(t, Options{}).Workspace; got != ws {
		t.Errorf("a flagless verb resolved %q, want %q", got, ws)
	}
}

// The key is a ref, so the label is what separates the sessions of one agent.
// A bare agent name and a labelled ref are two entries and two homes: reading
// either as the other would hand a labelled session the default one's home.
func TestABareAgentDoesNotCollideWithALabelledSession(t *testing.T) {
	isolateState(t)
	bare, labelled := t.TempDir(), t.TempDir()

	mustLoad(t, Options{Workspace: bare}).rememberSession()
	mustLoad(t, Options{Name: "rc23test", Workspace: labelled}).rememberSession()

	want := []string{"claude-code", "claude-code@rc23test"}
	got := indexKeys(t)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the index is keyed %v, want %v", got, want)
	}
	if got := mustLoad(t, Options{}).Workspace; got != bare {
		t.Errorf("the default session resolved %q, want %q", got, bare)
	}
	if got, want := mustLoad(t, Options{Name: "rc23test"}).Workspace, labelled+"-rc23test"; got != want {
		t.Errorf("the labelled session resolved %q, want %q", got, want)
	}
}

// `brig run claude` and `brig run claude-code` are one profile through an
// alias, so a session started under either spelling has to be the entry the
// other one finds -- one entry, not two homes that drift apart.
func TestAnAliasFindsTheSameEntry(t *testing.T) {
	isolateState(t)
	base := t.TempDir()
	ws := base + "-refactor"

	mustLoadAs(t, "claude", Options{Name: "refactor", Workspace: base}).rememberSession()

	// The key is the resolved profile name rather than the token that was
	// typed, which is what makes this one entry.
	if got := indexKeys(t); len(got) != 1 || got[0] != "claude-code@refactor" {
		t.Fatalf("the index is keyed %v, want [claude-code@refactor]", got)
	}
	for _, agent := range []string{"claude", "claude-code"} {
		if got := mustLoadAs(t, agent, Options{Name: "refactor"}).Workspace; got != ws {
			t.Errorf("addressed as %s, a flagless verb resolved %q, want %q", agent, got, ws)
		}
	}

	// And recording again under the other spelling stays one entry, so the two
	// spellings cannot end up with a home each.
	mustLoadAs(t, "claude-code", Options{Name: "refactor"}).rememberSession()
	if got := indexKeys(t); len(got) != 1 {
		t.Errorf("recording under the other spelling left %v", got)
	}
}

// --name is lenient where a ref is strict: it accepts a name that is not
// slug-clean and Slug sanitises it, so the raw name is not a stable
// identifier. The slug is what names the sandbox and the home, so it is what
// the entry is filed under.
func TestTheKeyIsTheSlugAndNotTheNameAsTyped(t *testing.T) {
	isolateState(t)
	base := t.TempDir()

	created := mustLoad(t, Options{Name: "RC23 Test", Workspace: base})
	if created.Slug != "rc23-test" {
		t.Fatalf("%q slugged to %q, and this case is written around rc23-test",
			created.RawName, created.Slug)
	}
	created.rememberSession()

	if got := indexKeys(t); len(got) != 1 || got[0] != "claude-code@rc23-test" {
		t.Fatalf("the index is keyed %v, want [claude-code@rc23-test]", got)
	}
	if got, want := mustLoad(t, Options{Name: "RC23 Test"}).Workspace, base+"-rc23-test"; got != want {
		t.Errorf("a flagless verb resolved %q, want %q", got, want)
	}
}

// A stop leaves the session alone -- it is the same session, stopped, and its
// home is where the work is. A remove is the one that drops the entry, so the
// next sandbox to take the name resolves its workspace the ordinary way rather
// than inheriting a directory chosen for a sandbox that is gone.
func TestAnEntrySurvivesAStopAndGoesOnARemove(t *testing.T) {
	isolateState(t)
	base := t.TempDir()
	ws := base + "-rc23test"

	c := mustLoad(t, Options{Name: "rc23test", Workspace: base})
	c.rememberSession()
	if got := WorkspaceOfSandbox(c.VMName); got != ws {
		t.Fatalf("the workspace was recorded as %q, want %q", got, ws)
	}

	rt := &removingRuntime{}
	c.Runtime = rt
	if err := c.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if got := WorkspaceOfSandbox(c.VMName); got != ws {
		t.Errorf("a stop dropped the entry: %q", got)
	}

	if err := c.Remove(); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	if len(rt.removed) != 1 || rt.removed[0] != c.VMName {
		t.Errorf("the runtime was asked to remove %v", rt.removed)
	}
	if got := WorkspaceOfSandbox(c.VMName); got != "" {
		t.Errorf("rm left %q recorded for a sandbox it removed", got)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got == ws {
		t.Errorf("a later run still resolved the removed sandbox's workspace %q", got)
	}
}

// `brig reset` works from the runtime's instance list, so it prunes by sandbox
// name without a Config -- the entry has to go on that path too, or a reset
// leaves the index naming every sandbox it just removed.
func TestForgetSandboxPrunesBySandboxName(t *testing.T) {
	isolateState(t)
	ws := t.TempDir()

	c := mustLoad(t, Options{Name: "rc23test", Workspace: ws})
	c.rememberSession()

	// A sandbox that was never recorded is not an error: reset walks every brig
	// sandbox, including ones created before the index existed.
	ForgetSandbox("brig-claude-code-neverseen")
	ForgetSandbox(c.VMName)

	if got := WorkspaceOfSandbox(c.VMName); got != "" {
		t.Errorf("reset left %q recorded for %s", got, c.VMName)
	}
}

// `brig ls` asks the runtime what exists, so it is the verb that can tell an
// entry whose sandbox is gone from one whose sandbox is merely stopped. A
// sandbox removed outside brig never reaches Remove, and its entry would
// otherwise sit in the file naming a directory for a sandbox nobody has.
func TestPruningDropsTheSessionsWhoseSandboxIsGone(t *testing.T) {
	isolateState(t)
	live, gone := t.TempDir(), t.TempDir()

	kept := mustLoad(t, Options{Name: "kept", Workspace: live})
	kept.rememberSession()
	dropped := mustLoad(t, Options{Name: "gone", Workspace: gone})
	dropped.rememberSession()

	// The list is every instance the runtime has, brig's own and anybody
	// else's, which is what `brig ls` has in hand.
	PruneSessions([]string{kept.VMName, "some-other-container"})

	if got := WorkspaceOfSandbox(kept.VMName); got != live+"-kept" {
		t.Errorf("pruning dropped a session whose sandbox is still there: %q", got)
	}
	if got := WorkspaceOfSandbox(dropped.VMName); got != "" {
		t.Errorf("pruning kept %q for %s, whose sandbox is gone", got, dropped.VMName)
	}
}

// The index is bookkeeping, so nothing about it is worth failing a command
// over: a missing file, a corrupt one and one that cannot be opened at all
// each resolve the default, which is what the release before it did.
func TestAnUnusableIndexIsIgnoredRatherThanFatal(t *testing.T) {
	dir := isolateState(t)
	path := filepath.Join(dir, sessionIndexName)

	// Missing: nothing has been recorded yet, which is every first run.
	fallback := mustLoad(t, Options{Name: "rc23test"}).Workspace
	if fallback == "" {
		t.Fatal("no workspace was resolved at all")
	}

	// Corrupt: half a write, or a file somebody edited.
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := WorkspaceOfSandbox("brig-claude-code-rc23test"); got != "" {
		t.Errorf("a corrupt index returned %q", got)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got != fallback {
		t.Errorf("a corrupt index changed the resolved workspace to %q, want %q", got, fallback)
	}
	// A file of the right shape but the wrong type is the same class: this is
	// what an old claim index, which was a map of strings under this name,
	// looks like to the reader that now expects a session.
	if err := os.WriteFile(path, []byte(`{"claude-code@rc23test": "/somewhere"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, Options{Name: "rc23test"}).Workspace; got != fallback {
		t.Errorf("an index of the wrong shape changed the resolved workspace to %q, want %q",
			got, fallback)
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
	c.rememberSession()
	if !strings.Contains(said.String(), c.VMName) {
		t.Errorf("an index that cannot be written said %q, which does not name the sandbox",
			said.String())
	}
}

// The JSON literal `null` is the one corrupt file the unmarshal accepts: it
// parses without error and leaves the map nil, so a reader that only checks
// the error hands back a map that cannot be written to. Both writes above are
// then a panic -- which is a louder version of exactly the failure the
// tolerant read exists to avoid, since a stray file in ~/.brig would stop
// every brig command on the host.
func TestAnIndexHoldingJSONNullIsIgnoredRatherThanFatal(t *testing.T) {
	dir := isolateState(t)

	for _, name := range []string{sessionIndexName, slugClaimIndexName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("null\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The read itself, which is where the nil would come from.
	if index := readSessionIndex(); index == nil {
		t.Error("a session index holding null read back as a nil map")
	}
	if index := readIndex[string](slugClaimIndexName); index == nil {
		t.Error("a claim index holding null read back as a nil map")
	}

	// And the two writes that would take the nil, which is where the cost of
	// it actually lands.
	c := mustLoad(t, Options{Name: "rc23test"})
	c.rememberSession()
	if got := WorkspaceOfSandbox(c.VMName); got != c.Workspace {
		t.Errorf("the session was recorded as %q, want %q", got, c.Workspace)
	}
	if err := c.claimSlug(); err != nil {
		t.Errorf("the claim over a null index was refused: %v", err)
	}
	if got := readIndex[string](slugClaimIndexName)[c.VMName]; got != c.RawName {
		t.Errorf("the sandbox was claimed by %q, want %q", got, c.RawName)
	}
}

// Every entry survives a write, so recording one session does not cost another
// its own -- the file is read, edited and written back whole.
func TestRecordingOneSessionKeepsTheOthers(t *testing.T) {
	isolateState(t)
	first, second := t.TempDir(), t.TempDir()

	a := mustLoad(t, Options{Name: "one", Workspace: first})
	a.rememberSession()
	b := mustLoad(t, Options{Name: "two", Workspace: second})
	b.rememberSession()

	if want := first + "-one"; WorkspaceOfSandbox(a.VMName) != want {
		t.Errorf("%s lost its entry: %q", a.VMName, WorkspaceOfSandbox(a.VMName))
	}
	if want := second + "-two"; WorkspaceOfSandbox(b.VMName) != want {
		t.Errorf("%s was recorded as %q, want %q", b.VMName, WorkspaceOfSandbox(b.VMName), want)
	}
}

// The sandbox-keyed file it replaces is deleted rather than migrated: a
// brig-claude-code-refactor key cannot be read back into a ref without
// guessing which of the dashes was the one between the agent and its label.
// Each session in it costs one restart, which is what an absent entry has
// always cost.
func TestTheSandboxKeyedIndexIsDeletedRatherThanMigrated(t *testing.T) {
	dir := isolateState(t)
	legacy := filepath.Join(dir, legacyWorkspaceIndexName)
	body := `{"brig-claude-code-rc23test": "/private/tmp/ws-rc23"}`
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Nothing is read out of it: the session resolves the default it would
	// have resolved with no file at all.
	fresh := mustLoad(t, Options{Name: "rc23test"})
	if fresh.Workspace == "/private/tmp/ws-rc23" {
		t.Fatal("the old sandbox-keyed file was read back")
	}
	fresh.rememberSession()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("recording a session left %s behind (%v)", legacyWorkspaceIndexName, err)
	}

	// And its absence is not an error, on either path that clears it: a host
	// that never ran the older release has no such file.
	mustLoad(t, Options{Name: "rc23test"}).rememberSession()
	PruneSessions(nil)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("%s came back (%v)", legacyWorkspaceIndexName, err)
	}
}
