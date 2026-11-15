package wrap

import (
	"bytes"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// memoryRuntime is hull on hvi as far as memory goes: it boots, it answers
// whether the sandbox is up, and it routes brig.internal.
type memoryRuntime struct {
	*livenessRuntime
}

func (r *memoryRuntime) ProbeBrigInternal() error { return nil }
func (r *memoryRuntime) BrigInternalAddr(string) (netip.Addr, error) {
	return netip.MustParseAddr("198.18.1.1"), nil
}

// memorySession is the ubuntu profile on hvi, named claude-code, with nothing
// set that could turn memory on behind a case's back, and a home for it.
// Named claude-code because brig wires memory to that agent alone (see
// memoryWired); the ubuntu spec boots without the volumes and secrets of the
// real one, which no case here is about.
func memorySession(t *testing.T) (profile.Profile, string) {
	t.Helper()
	isolateState(t)
	// The divert socket of brig.internal is under the state directory, and
	// a unix socket path fits in 103 bytes.
	shortStateDir(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	for _, key := range []string{"BRIG_MEMORY", "BRIG_CLAUDE_CODE_MEMORY"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	p, ok := profile.Lookup("ubuntu")
	if !ok {
		t.Fatal("the ubuntu profile is not loaded")
	}
	p.Name = "claude-code"
	return p, t.TempDir()
}

func loadMemory(t *testing.T, p profile.Profile, o Options, rt runtime.Runtime) *Config {
	t.Helper()
	c, err := Load(p, o, rt)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err, c.Progress = verify.Off, io.Discard, io.Discard, io.Discard
	// A run with memory registers it, and a case never starts the real
	// broker. A case that asks what was registered sets its own.
	c.broker = &fakeBroker{}
	return c
}

func TestAgentMemoryProjectAndNamed(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()

	c := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: "project"}, nil)
	if want := memory.ProjectKey(c.Project); c.MemoryKey() != want {
		t.Errorf("project memory resolved %q, want %q", c.MemoryKey(), want)
	}
	if want := "project (" + c.Project + ")"; c.MemoryLabel() != want {
		t.Errorf("project memory is shown as %q, want %q", c.MemoryLabel(), want)
	}

	c = loadMemory(t, p, Options{Workspace: home, Memory: "go-services"}, nil)
	if c.MemoryKey() != "go-services" || c.MemoryLabel() != "go-services" {
		t.Errorf("named memory resolved %q shown as %q", c.MemoryKey(), c.MemoryLabel())
	}

	c = loadMemory(t, p, Options{Workspace: home, Project: project}, nil)
	if c.MemoryKey() != "" || c.MemoryLabel() != "" {
		t.Errorf("a run that asked for nothing has memory %q (%q)", c.MemoryKey(), c.MemoryLabel())
	}
}

// --no-agent-memory beats the flag, the setting and the profile, which is
// the one way to say "not this time" when the setting or the profile turns
// memory on.
func TestNoAgentMemoryWins(t *testing.T) {
	p, home := memorySession(t)
	p.Memory = "from-profile"
	t.Setenv("BRIG_MEMORY", "from-setting")
	c := loadMemory(t, p, Options{Workspace: home, Memory: "from-flag", NoMemory: true}, nil)
	if c.MemoryKey() != "" {
		t.Errorf("--no-agent-memory left memory %q on", c.MemoryKey())
	}
	c = loadMemory(t, p, Options{Workspace: home, Memory: "from-flag"}, nil)
	if c.MemoryKey() != "from-flag" {
		t.Errorf("the flag lost to a lower source: %q", c.MemoryKey())
	}
}

func TestBrigMemoryEnv(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	p.Memory = "from-profile"
	for _, tc := range []struct{ value, want string }{
		{"from-setting", "from-setting"},
		{"project", "project"},
		// Off, and it beats the profile: the setting is the nearer source.
		{"0", ""},
		{"off", ""},
		// Empty is unset, so the profile answers.
		{"", "from-profile"},
	} {
		t.Setenv("BRIG_MEMORY", tc.value)
		c := loadMemory(t, p, Options{Workspace: home, Project: project}, nil)
		want := tc.want
		if want == "project" {
			want = memory.ProjectKey(c.Project)
		}
		if c.MemoryKey() != want {
			t.Errorf("BRIG_MEMORY=%q resolved %q, want %q", tc.value, c.MemoryKey(), want)
		}
	}
}

func TestProfileMemoryFieldTurnsMemoryOn(t *testing.T) {
	p, home := memorySession(t)
	p.Memory = "team"
	if c := loadMemory(t, p, Options{Workspace: home}, nil); c.MemoryKey() != "team" {
		t.Errorf("the profile's memory: resolved %q, want team", c.MemoryKey())
	}
}

// A store name the guest never sees still becomes a directory under the
// store root, and project- is ProjectKey's alone. Refused before boot, and
// not in Load: stop and rm load the session too.
func TestMemoryNameRefused(t *testing.T) {
	for _, name := range []string{"project-x", "../x", "a/b"} {
		t.Run(name, func(t *testing.T) {
			p, home := memorySession(t)
			rt := &memoryRuntime{&livenessRuntime{workspace: home}}
			c := loadMemory(t, p, Options{Workspace: home, Memory: name}, rt)
			err := c.EnsureRunning(creds.Set{})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("store name %q was not refused by name: %v", name, err)
			}
			if rt.boots != 0 {
				t.Errorf("booted with store name %q", name)
			}
			if c.MemoryKey() != "" {
				t.Errorf("a refused name left key %q", c.MemoryKey())
			}
		})
	}
}

// The key is the cleaned path, so dir and dir/ are one store.
func TestProjectKeyFromFolder(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	a := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: "project"}, nil)
	b := loadMemory(t, p, Options{Workspace: home, Project: project + string(filepath.Separator), Memory: "project"}, nil)
	if a.MemoryKey() == "" || a.MemoryKey() != b.MemoryKey() {
		t.Errorf("%s and %s/ resolved %q and %q", project, project, a.MemoryKey(), b.MemoryKey())
	}
}

func TestAgentMemoryNeedsAProject(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true, Memory: "project"}, rt)
	err := c.EnsureRunning(creds.Set{})
	want := "--agent-memory needs a project folder, or name a store with --agent-memory=<name>"
	if err == nil || err.Error() != want {
		t.Fatalf("project memory with no project: %v, want %q", err, want)
	}
	if rt.boots != 0 {
		t.Error("booted project memory with no project")
	}
}

// vz takes its network from vmnet, so nothing answers brig.internal there,
// and memory is served at nowhere else. Refused before boot, naming where it
// does run.
func TestMemoryRefusedOnVz(t *testing.T) {
	p, home := memorySession(t)
	t.Setenv("BRIG_HYPERVISOR", "vz")
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	err := c.EnsureRunning(creds.Set{})
	if err == nil {
		t.Fatal("memory on vz was not refused")
	}
	for _, want := range []string{"brig.internal", "hvi", "nerdctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "policy") {
		t.Errorf("the refusal talks about a policy this run does not have: %v", err)
	}
	if rt.boots != 0 {
		t.Error("booted memory on vz")
	}
	// And the same run without memory is not refused for it.
	c = loadMemory(t, p, Options{Workspace: home}, rt)
	if err := c.memoryRefusal(); err != nil {
		t.Errorf("a run without memory was refused for it: %v", err)
	}
}

func TestMemoryRecordedAtBoot(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if got := MemoryOfSandboxes()[c.VMName]; got != "team" {
		t.Errorf("recorded %q for %s, want team", got, c.VMName)
	}
}

func TestNoMemoryNoRecord(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home}, rt)
	// A record left by an earlier boot of this name goes with the boot that
	// replaces it, or the next run would read memory this sandbox lacks.
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{c.VMName: {"team", memoryNamed}, "brig-other": {"x", memoryNamed}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	got := MemoryOfSandboxes()
	if _, ok := got[c.VMName]; ok {
		t.Errorf("a run without memory left a record: %v", got)
	}
	if got["brig-other"] != "x" {
		t.Errorf("another sandbox's record was dropped: %v", got)
	}
}

// A store is fixed for the life of a sandbox: the broker serves it by
// sandbox, and a guest that wrote to one store must not find the next run on
// another without being told. So a running sandbox asked for a
// different store, or for memory on or off, is refused, never restarted.
func TestMemoryChangeOnRunningSandboxRefused(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, asked, was, now string
	}{
		{"off to on", "", "team", "off", "team"},
		{"a to b", "a", "b", "a", "b"},
		// Off is asked for explicitly: a run that asks nothing inherits.
		{"on to off", "team", "", "team", "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, home := memorySession(t)
			if err := writeSessionIndex(map[string]sessionEntry{"claude-code": {Home: home, Sandbox: "brig-claude-code"}}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
				t.Fatal(err)
			}
			if tc.recorded != "" {
				if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {tc.recorded, memoryNamed}}); err != nil {
					t.Fatal(err)
				}
			}
			rt := &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
			c := loadMemory(t, p, Options{Workspace: home, NoProject: true, Memory: tc.asked,
				NoMemory: tc.asked == ""}, rt)
			err := c.EnsureRunning(creds.Set{})
			want := "brig-claude-code was created with memory " + tc.was + "; this run asks for " + tc.now +
				`. Remove it with "brig rm claude-code" and run again`
			if err == nil || err.Error() != want {
				t.Fatalf("got %v\nwant %s", err, want)
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Error("a memory change touched the running sandbox")
			}
			if got := MemoryOfSandboxes()["brig-claude-code"]; got != tc.recorded {
				t.Errorf("the refusal changed the record to %q", got)
			}
		})
	}
}

// The same store asked again reuses the running sandbox.
func TestSameMemoryOnRunningSandboxReused(t *testing.T) {
	p, home := memorySession(t)
	if err := writeSessionIndex(map[string]sessionEntry{"claude-code": {Home: home, Sandbox: "brig-claude-code"}}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {"team", memoryNamed}}); err != nil {
		t.Fatal(err)
	}
	rt := &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true, Memory: "team"}, rt)
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
		t.Error("the same store restarted the running sandbox")
	}
}

// The project store is named by its folder in the refusal, the way the user
// asked for it, rather than by its key.
func TestMemoryChangeNamesTheProject(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	first := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: "project"}, nil)
	if err := writeSessionIndex(map[string]sessionEntry{"claude-code": {Home: home, Sandbox: "brig-claude-code", Project: first.Project}}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {first.MemoryKey(), memoryProject}}); err != nil {
		t.Fatal(err)
	}
	rt := &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "created with memory project ("+first.Project+")") {
		t.Fatalf("the refusal does not name the project store by its folder: %v", err)
	}
}

func TestForgetSandboxDropsMemoryRecord(t *testing.T) {
	isolateState(t)
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-a": {"team", memoryNamed}, "brig-b": {"other", memoryNamed}}); err != nil {
		t.Fatal(err)
	}
	// No session entry names it: rm of a sandbox whose session was already
	// pruned still drops what memory it was created with.
	if _, err := ForgetSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	got := MemoryOfSandboxes()
	if _, ok := got["brig-a"]; ok {
		t.Errorf("ForgetSandbox left the memory record: %v", got)
	}
	if got["brig-b"] != "other" {
		t.Errorf("ForgetSandbox dropped another sandbox's record: %v", got)
	}
}

// runningMemorySandbox is a running claude-code sandbox the index knows, booted
// with store recorded ("" for none).
func runningMemorySandbox(t *testing.T, recorded string) (profile.Profile, string, *memoryRuntime) {
	t.Helper()
	p, home := memorySession(t)
	if err := writeSessionIndex(map[string]sessionEntry{"claude-code": {Home: home, Sandbox: "brig-claude-code"}}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
		t.Fatal(err)
	}
	if recorded != "" {
		if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {recorded, memoryNamed}}); err != nil {
			t.Fatal(err)
		}
	}
	return p, home, &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
}

// A run that says nothing about memory keeps the store the sandbox has, the
// way it keeps its network posture: `brig sh` after `brig run
// --agent-memory` is not asking for memory off.
func TestFlaglessRunInheritsMemory(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "team")
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
	if c.MemoryKey() != "team" || c.MemoryLabel() != "team" {
		t.Errorf("a flagless run resolved %q (%q), want the recorded team", c.MemoryKey(), c.MemoryLabel())
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
		t.Error("a flagless run recreated the memory sandbox")
	}
	if got := MemoryOfSandboxes()["brig-claude-code"]; got != "team" {
		t.Errorf("the record is %q after a flagless run, want team", got)
	}
}

// A record the session index does not vouch for is not inherited: the
// sandbox it described may have been removed outside brig.
func TestUnindexedMemoryRecordNotInherited(t *testing.T) {
	p, home := memorySession(t)
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {"team", memoryNamed}}); err != nil {
		t.Fatal(err)
	}
	if c := loadMemory(t, p, Options{Workspace: home}, nil); c.MemoryKey() != "" {
		t.Errorf("inherited %q from a record no session names", c.MemoryKey())
	}
}

// Off said out loud is an ask, and differs from the record.
func TestExplicitOffOnMemorySandboxRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
		env  string
	}{
		{"--no-agent-memory", Options{NoMemory: true}, ""},
		{"BRIG_MEMORY=off", Options{}, "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, home, rt := runningMemorySandbox(t, "team")
			if tc.env != "" {
				t.Setenv("BRIG_MEMORY", tc.env)
			}
			tc.o.Workspace, tc.o.NoProject = home, true
			c := loadMemory(t, p, tc.o, rt)
			err := c.EnsureRunning(creds.Set{})
			if err == nil || !strings.Contains(err.Error(), "created with memory team; this run asks for off") {
				t.Fatalf("explicit off on a memory sandbox: %v", err)
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Error("explicit off touched the running sandbox")
			}
		})
	}
}

// false is off, as --agent-memory=false is, and not a store called false.
func TestBrigMemoryFalseIsOff(t *testing.T) {
	p, home := memorySession(t)
	p.Memory = "from-profile"
	t.Setenv("BRIG_MEMORY", "false")
	c := loadMemory(t, p, Options{Workspace: home}, nil)
	if c.MemoryKey() != "" || c.memoryRefused != nil || !c.memoryAsked {
		t.Errorf("BRIG_MEMORY=false resolved %q, refused %v, asked %v", c.MemoryKey(), c.memoryRefused, c.memoryAsked)
	}
}

// nerdctlMemoryRuntime is nerdctl on urunc as far as memory goes.
type nerdctlMemoryRuntime struct{ *memoryRuntime }

func (r *nerdctlMemoryRuntime) Kind() string { return "nerdctl" }

// Memory is served on the broker's own address, as a route is, so a run with
// it is isolated whatever posture it asked for, and nerdctl, which routes
// brig.internal only on a network of the sandbox's own, serves it.
func TestAgentMemoryRunsIsolated(t *testing.T) {
	p, home := memorySession(t)
	p.Network = "shared"
	rt := &nerdctlMemoryRuntime{&memoryRuntime{&livenessRuntime{workspace: home}}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	if c.Network != NetIsolated {
		t.Errorf("a run with memory resolved %s, want isolated", c.Network)
	}
	if err := c.memoryRefusal(); err != nil {
		t.Errorf("memory on nerdctl was refused: %v", err)
	}
	// And the posture recorded is the one asked for, as with a route, so a
	// later run without memory is not a posture change.
	if c.askedNetwork != NetShared {
		t.Errorf("asked posture became %s", c.askedNetwork)
	}
	if c = loadMemory(t, p, Options{Workspace: home}, rt); c.Network != NetShared {
		t.Errorf("a run without memory resolved %s, want shared", c.Network)
	}
}

func TestAgentMemoryOfflineRefused(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Network: "offline", Memory: "team"}, rt)
	if c.Network != NetOffline {
		t.Errorf("memory moved an offline run to %s", c.Network)
	}
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "an offline run cannot reach") || !strings.Contains(err.Error(), "agent memory") {
		t.Fatalf("memory on an offline run: %v", err)
	}
	if strings.Contains(err.Error(), "policy") {
		t.Errorf("the refusal talks about a policy this run does not have: %v", err)
	}
	if rt.boots != 0 {
		t.Error("booted memory with no network")
	}
}

// projectMemorySandbox is a running claude-code sandbox booted on project with
// that project's store, as `brig run --agent-memory claude-code <project>` leaves
// it.
func projectMemorySandbox(t *testing.T, p profile.Profile, home, project string) *memoryRuntime {
	t.Helper()
	first := loadMemory(t, p, Options{Workspace: home, Project: project, Memory: "project"}, nil)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: home, Sandbox: "brig-claude-code", Project: first.Project},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{
		"brig-claude-code": {first.MemoryKey(), memoryProject},
	}); err != nil {
		t.Fatal(err)
	}
	return &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
}

// An inherited project's store follows the project. A flagless run on B
// after a memory run on A gets B's store, never A's memory: the project
// change recreates the sandbox, and its boot records B's key.
func TestInheritedProjectStoreFollowsProject(t *testing.T) {
	p, home := memorySession(t)
	a, b := t.TempDir(), t.TempDir()
	rt := projectMemorySandbox(t, p, home, a)
	c := loadMemory(t, p, Options{Workspace: home, Project: b}, rt)
	if want := memory.ProjectKey(c.Project); c.MemoryKey() != want {
		t.Errorf("a flagless run on B resolved %q, want B's store %q", c.MemoryKey(), want)
	}
	if want := "project (" + c.Project + ")"; c.MemoryLabel() != want {
		t.Errorf("label %q, want %q", c.MemoryLabel(), want)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.boots != 1 {
		t.Fatalf("booted %d times, want the one recreate the project change asks for", rt.boots)
	}
	if got := readMemoryIndex()["brig-claude-code"]; got != (memoryRecord{c.MemoryKey(), memoryProject}) {
		t.Errorf("the boot recorded %+v, want B's key in project mode", got)
	}
}

// The same project again inherits the store and reuses the sandbox.
func TestInheritedProjectStoreSameProjectReused(t *testing.T) {
	p, home := memorySession(t)
	a := t.TempDir()
	rt := projectMemorySandbox(t, p, home, a)
	c := loadMemory(t, p, Options{Workspace: home}, rt)
	if want := memory.ProjectKey(c.Project); c.Project == "" || c.MemoryKey() != want {
		t.Errorf("a flagless run on the remembered project resolved %q (project %q)", c.MemoryKey(), c.Project)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
		t.Error("the same project recreated the sandbox")
	}
}

// With no project there is no store to follow, so the run is refused rather
// than booted without the memory it had or with another project's.
func TestInheritedProjectStoreWithoutProjectRefused(t *testing.T) {
	p, home := memorySession(t)
	rt := projectMemorySandbox(t, p, home, t.TempDir())
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
	err := c.EnsureRunning(creds.Set{})
	want := "--agent-memory needs a project folder, or name a store with --agent-memory=<name>"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
		t.Error("the refusal touched the running sandbox")
	}
}

// A named store is the store whatever the project: a project change keeps it.
func TestInheritedNamedStoreKept(t *testing.T) {
	p, home := memorySession(t)
	a, b := t.TempDir(), t.TempDir()
	first := loadMemory(t, p, Options{Workspace: home, Project: a}, nil)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: home, Sandbox: "brig-claude-code", Project: first.Project},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", NetIsolated.RuntimeNet()); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": {"team", memoryNamed}}); err != nil {
		t.Fatal(err)
	}
	rt := &memoryRuntime{&livenessRuntime{running: true, workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Project: b}, rt)
	if c.MemoryKey() != "team" || c.MemoryLabel() != "team" {
		t.Errorf("a project change resolved %q (%q), want the named store team", c.MemoryKey(), c.MemoryLabel())
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if got := readMemoryIndex()["brig-claude-code"]; got != (memoryRecord{"team", memoryNamed}) {
		t.Errorf("the boot recorded %+v, want team in named mode", got)
	}
}

// A store is in use by the sandboxes that recorded it and that the session
// index still names. A record whose sandbox no session names is a leftover,
// and must not keep `brig memory rm` from removing the store.
func TestMemoryUsersNeedsALiveSession(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"a": {Home: t.TempDir(), Sandbox: "brig-a"},
		"b": {Home: t.TempDir(), Sandbox: "brig-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{
		"brig-a":     {"team", memoryNamed},
		"brig-b":     {"team", memoryNamed},
		"brig-stale": {"team", memoryNamed},
		"brig-other": {"other", memoryNamed},
	}); err != nil {
		t.Fatal(err)
	}
	got := MemoryUsers("team")
	if want := []string{"brig-a", "brig-b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("MemoryUsers(team) = %v, want %v", got, want)
	}
	if got := MemoryUsers("none"); len(got) != 0 {
		t.Errorf("MemoryUsers(none) = %v, want nothing", got)
	}
}

// `brig ls` prunes sessions whose sandbox is gone; the memory record of such a
// sandbox goes with it, so memory.json does not collect names nobody has.
func TestPruneSessionsDropsMemoryRecordOfPrunedSandbox(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"a": {Home: t.TempDir(), Sandbox: "brig-a"},
		"b": {Home: t.TempDir(), Sandbox: "brig-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(memoryIndexName, map[string]memoryRecord{
		"brig-a": {"team", memoryNamed},
		"brig-b": {"team", memoryNamed},
	}); err != nil {
		t.Fatal(err)
	}
	PruneSessions([]string{"brig-a"})
	got := MemoryOfSandboxes()
	if _, ok := got["brig-b"]; ok {
		t.Errorf("PruneSessions left the memory record of a pruned sandbox: %v", got)
	}
	if got["brig-a"] != "team" {
		t.Errorf("PruneSessions dropped the record of a live sandbox: %v", got)
	}
}

// The words a switch is written with mean off or the project's store from
// BRIG_MEMORY and from the profile's memory:, raw or as YAML 1.1 parsed
// them. None attaches a store of that name.
func TestMemorySwitchWords(t *testing.T) {
	cases := []struct{ value, want string }{
		{"off", ""}, {"no", ""}, {"false", ""}, {"0", ""}, {"none", ""}, {"OFF", ""},
		{"on", "project"}, {"yes", "project"}, {"true", "project"}, {"1", "project"},
	}
	for _, src := range []string{"BRIG_MEMORY", "profile"} {
		for _, tc := range cases {
			t.Run(src+"="+tc.value, func(t *testing.T) {
				p, home := memorySession(t)
				project := t.TempDir()
				if src == "profile" {
					p.Memory = tc.value
				} else {
					p.Memory = "from-profile"
					t.Setenv("BRIG_MEMORY", tc.value)
				}
				c := loadMemory(t, p, Options{Workspace: home, Project: project}, nil)
				want := tc.want
				if want == "project" {
					want = memory.ProjectKey(c.Project)
				}
				if c.MemoryKey() != want || c.memoryRefused != nil {
					t.Errorf("%s %q resolved %q (refused %v), want %q", src, tc.value, c.MemoryKey(), c.memoryRefused, want)
				}
			})
		}
	}
}

// A value that is neither a switch word, project nor a store name is
// refused, naming where it came from.
func TestMemoryBadValueNamesSource(t *testing.T) {
	p, home := memorySession(t)
	p.Memory = "Bad"
	if c := loadMemory(t, p, Options{Workspace: home}, nil); c.memoryRefused == nil ||
		!strings.Contains(c.memoryRefused.Error(), "profile") {
		t.Errorf("profile memory: Bad = %v, want a refusal naming the profile", c.memoryRefused)
	}
	t.Setenv("BRIG_MEMORY", "Bad")
	if c := loadMemory(t, p, Options{Workspace: home}, nil); c.memoryRefused == nil ||
		!strings.Contains(c.memoryRefused.Error(), "BRIG_MEMORY") {
		t.Errorf("BRIG_MEMORY=Bad = %v, want a refusal naming BRIG_MEMORY", c.memoryRefused)
	}
}

// A record that names a switch word, as one written before the words were
// reserved could, is read as the setting it spells and not as a store.
func TestInheritedSwitchWordRecord(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "false")
	if c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt); c.MemoryKey() != "" {
		t.Errorf("a record of \"false\" was inherited as store %q", c.MemoryKey())
	}
}

// A repository does not choose an agent memory store. A Brigfile profile
// with memory: would attach the sandbox to a store the user's other projects
// write, behind an approval that never named it, so the run is refused and
// told who can turn memory on.
func TestRepositoryProfileMemoryRefused(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_MEMORY", "")
	p := repoClaude(t)
	p.Memory = "work"
	c := loadTarget(t, p, Options{Target: repoTarget()})
	if c.MemoryKey() != "" {
		t.Errorf("a repository profile attached the store %q", c.MemoryKey())
	}
	if c.memoryRefused == nil || !strings.Contains(c.memoryRefused.Error(), "repository") ||
		!strings.Contains(c.memoryRefused.Error(), "--agent-memory") ||
		!strings.Contains(c.memoryRefused.Error(), "file:.brig/claude.yaml") {
		t.Errorf("a repository profile's memory: was not refused naming the profile and the flag: %v",
			c.memoryRefused)
	}
	// Off in the repository's profile asks for nothing, and is not refused.
	p.Memory = "off"
	if c := loadTarget(t, p, Options{Target: repoTarget()}); c.memoryRefused != nil || c.MemoryKey() != "" {
		t.Errorf("memory: off in a repository profile: key %q, refused %v", c.MemoryKey(), c.memoryRefused)
	}
}

// noSecrets lists no secret, so a plan answers without the keychain.
func noSecrets(c *Config) {
	reads := 0
	c.OpenStore = func() (creds.SecretReader, error) { return readCountingStore{reads: &reads}, nil }
}

// The plan names the store a run with memory attaches, as the user would
// name it, and where the ask came from.
func TestPlanNamesTheMemoryStore(t *testing.T) {
	p, home := memorySession(t)
	project := t.TempDir()
	for _, tc := range []struct {
		name, env, profile string
		o                  Options
		store, from        string
	}{
		{"flag", "", "", Options{Memory: "team"}, "team", "--agent-memory"},
		{"setting", "project", "", Options{}, "project (" + project + ")", "BRIG_MEMORY"},
		{"profile", "", "team", Options{}, "team", "the profile claude-code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIG_MEMORY", tc.env)
			p.Memory = tc.profile
			tc.o.Workspace, tc.o.Project = home, project
			c := loadMemory(t, p, tc.o, nil)
			noSecrets(c)
			d := c.Plan()
			if d.Memory == nil || d.Memory.Store != tc.store || d.Memory.From != tc.from {
				t.Fatalf("plan memory = %+v, want store %q from %q", d.Memory, tc.store, tc.from)
			}
			var text bytes.Buffer
			c.Out = &text
			c.PrintPlan(d)
			if !strings.Contains(text.String(), "MEMORY") || !strings.Contains(text.String(), tc.store) ||
				!strings.Contains(text.String(), tc.from) {
				t.Errorf("the plan text does not name the store %q from %q:\n%s", tc.store, tc.from, text.String())
			}
		})
	}
}

// A store kept from the sandbox's boot says so: nobody typed it this time.
func TestPlanNamesAnInheritedMemoryStore(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "team")
	c := loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
	c.Runtime = nil
	noSecrets(c)
	d := c.Plan()
	if d.Memory == nil || d.Memory.Store != "team" || !strings.Contains(d.Memory.From, "kept") {
		t.Errorf("plan memory = %+v, want team, kept from the sandbox", d.Memory)
	}
}

// A run without memory has no MEMORY row and no memory field, so its plan
// and its digest are the ones it had before memory existed.
func TestPlanWithoutMemoryHasNoMemoryRow(t *testing.T) {
	p, home := memorySession(t)
	c := loadMemory(t, p, Options{Workspace: home}, nil)
	noSecrets(c)
	d := c.Plan()
	if d.Memory != nil {
		t.Errorf("a run without memory planned %+v", d.Memory)
	}
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), `"memory"`) {
		t.Errorf("the plan of a run without memory has a memory field:\n%s", blob)
	}
	var text bytes.Buffer
	c.Out = &text
	c.PrintPlan(d)
	if strings.Contains(text.String(), "MEMORY") {
		t.Errorf("the plan of a run without memory has a MEMORY row:\n%s", text.String())
	}
}

// The approval of a target names the store, so the first yes to a run with
// memory is a yes to that store.
func TestApprovalNamesTheMemoryStore(t *testing.T) {
	p, _ := memorySession(t)
	tg := &Target{Name: "dev", ProfileAlias: "claude", ProfileSource: "builtin:claude-code", BuiltIn: true,
		Base: Origins{Image: "profile", Mem: "profile", CPUs: "profile", Network: "profile"}}
	c := loadTarget(t, p, Options{Target: tg, Memory: "team"})
	if c.memoryRefused != nil {
		t.Fatalf("memory on a builtin claude-code target was refused: %v", c.memoryRefused)
	}
	noSecrets(c)
	var text bytes.Buffer
	c.PrintApproval(&text, c.Plan(), "sha256:x")
	if !strings.Contains(text.String(), "MEMORY") || !strings.Contains(text.String(), "team") {
		t.Errorf("the approval does not name the store:\n%s", text.String())
	}
}

// A project store asked for out loud follows the project the way an
// inherited one does: the project change recreates the sandbox, and its boot
// records the new project's key. Asking for the project's store is not
// asking for another store.
func TestExplicitProjectAskFollowsProject(t *testing.T) {
	for _, tc := range []struct {
		name, env, profile string
		o                  Options
	}{
		{"flag", "", "", Options{Memory: memoryProject}},
		{"setting", "project", "", Options{}},
		{"profile", "", "project", Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, home := memorySession(t)
			a, b := t.TempDir(), t.TempDir()
			rt := projectMemorySandbox(t, p, home, a)
			t.Setenv("BRIG_MEMORY", tc.env)
			p.Memory = tc.profile
			tc.o.Workspace, tc.o.Project = home, b
			c := loadMemory(t, p, tc.o, rt)
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if rt.boots != 1 {
				t.Fatalf("booted %d times, want the one recreate the project change asks for", rt.boots)
			}
			if got := readMemoryIndex()["brig-claude-code"]; got != (memoryRecord{memory.ProjectKey(c.Project), memoryProject}) {
				t.Errorf("the boot recorded %+v, want B's key in project mode", got)
			}
		})
	}
}

// A store change rides on no project change: a named store asked for on a
// project store's sandbox, with another project, is still refused.
func TestNamedAskOnProjectChangeRefused(t *testing.T) {
	p, home := memorySession(t)
	a, b := t.TempDir(), t.TempDir()
	rt := projectMemorySandbox(t, p, home, a)
	c := loadMemory(t, p, Options{Workspace: home, Project: b, Memory: "team"}, rt)
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "this run asks for team") {
		t.Fatalf("a named store on a project change: %v", err)
	}
	if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
		t.Error("the refusal touched the running sandbox")
	}
}

// A record brig cannot read is not a store to inherit: a project key with no
// mode would otherwise attach the last project's store to this one, and any
// other key would be a named store nobody asked for. The run says so and is
// refused, and the sandbox is left alone.
func TestUnreadableMemoryRecordRefused(t *testing.T) {
	for _, rec := range []memoryRecord{
		{memory.ProjectKey("/elsewhere"), ""},
		{memory.ProjectKey("/elsewhere"), memoryNamed},
		{"team", ""},
		{"team", "sometimes"},
		{"Not A Store!", memoryNamed},
		{"project-zz", memoryProject},
	} {
		t.Run(rec.Key+"/"+rec.Mode, func(t *testing.T) {
			p, home, rt := runningMemorySandbox(t, "")
			if err := writeIndex(memoryIndexName, map[string]memoryRecord{"brig-claude-code": rec}); err != nil {
				t.Fatal(err)
			}
			broker := &fakeBroker{}
			c := loadMemory(t, p, Options{Workspace: home, Project: t.TempDir()}, rt)
			c.broker = broker
			if c.MemoryKey() != "" {
				t.Errorf("record %+v was inherited as %q", rec, c.MemoryKey())
			}
			err := c.EnsureRunning(creds.Set{})
			if err == nil || !strings.Contains(err.Error(), "brig-claude-code") ||
				!strings.Contains(err.Error(), "cannot read") || !strings.Contains(err.Error(), "brig rm claude-code") {
				t.Fatalf("record %+v: %v", rec, err)
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Error("the refusal touched the running sandbox")
			}
			if len(broker.calls) != 0 {
				t.Errorf("the refusal reached the broker: %v", broker.calls)
			}
		})
	}
}

// A sandbox the broker serves memory to, with no record of the store it was
// booted with, has lost its record. A run on it must not read that as memory
// off and remove the registration, which would revoke the memory token of an
// agent another exec started. It is refused, and the broker is not touched.
func TestLostMemoryRecordWhileBrokerServesRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"flagless", Options{}},
		{"asking for a store", Options{Memory: "team"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, home, rt := runningMemorySandbox(t, "")
			broker := &fakeBroker{running: true, extra: injector.Status{Memory: []string{"brig-claude-code"}}}
			tc.o.Workspace, tc.o.NoProject = home, true
			c := loadMemory(t, p, tc.o, rt)
			c.broker = broker
			err := c.EnsureRunning(creds.Set{})
			if err == nil || !strings.Contains(err.Error(), "brig-claude-code") ||
				!strings.Contains(err.Error(), "no record") || !strings.Contains(err.Error(), "brig rm claude-code") {
				t.Fatalf("got %v", err)
			}
			if len(broker.forgotten) != 0 || len(broker.routesForgotten) != 0 || len(broker.memRegistered) != 0 {
				t.Errorf("the refusal reached the broker: %v", broker.calls)
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Error("the refusal touched the running sandbox")
			}
		})
	}
}

// Memory is refused before boot for an agent brig does not point at its
// store, naming the agent: the run would get a token and an isolated network
// and no memory. The rule is agentArgs': claude-code, or a target that runs
// builtin:claude-code.
func TestMemoryRefusedForAgentsBrigDoesNotWire(t *testing.T) {
	p, home := memorySession(t)
	p.Name = "ubuntu"
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, nil)
	if c.MemoryKey() != "" || c.memoryRefused == nil || !strings.Contains(c.memoryRefused.Error(), "ubuntu") {
		t.Errorf("--agent-memory on ubuntu: key %q, refused %v", c.MemoryKey(), c.memoryRefused)
	}
	p.Memory = "team"
	c = loadMemory(t, p, Options{Workspace: home}, nil)
	if c.MemoryKey() != "" || c.memoryRefused == nil || !strings.Contains(c.memoryRefused.Error(), "ubuntu") {
		t.Errorf("memory: on ubuntu's profile: key %q, refused %v", c.MemoryKey(), c.memoryRefused)
	}
	// A target whose profile is not brig's claude-code, even one the
	// repository calls claude-code.
	tc := loadTarget(t, repoClaude(t), Options{Target: repoTarget(), Memory: "team"})
	if tc.MemoryKey() != "" || tc.memoryRefused == nil || !strings.Contains(tc.memoryRefused.Error(), "file:.brig/claude.yaml") {
		t.Errorf("--agent-memory on a repository profile: key %q, refused %v", tc.MemoryKey(), tc.memoryRefused)
	}
}

// BRIG_MEMORY is exported for every run in the shell, so it does not refuse
// a run of an agent brig does not wire: it is ignored there, with one
// warning, and the run keeps its posture.
func TestBrigMemoryIgnoredForAgentsBrigDoesNotWire(t *testing.T) {
	p, home := memorySession(t)
	p.Name, p.Network = "ubuntu", "shared"
	t.Setenv("BRIG_MEMORY", "team")
	c := loadMemory(t, p, Options{Workspace: home}, nil)
	if c.MemoryKey() != "" || c.memoryRefused != nil {
		t.Errorf("BRIG_MEMORY on ubuntu: key %q, refused %v", c.MemoryKey(), c.memoryRefused)
	}
	if c.Network != NetShared {
		t.Errorf("an ignored BRIG_MEMORY narrowed the run to %s", c.Network)
	}
	var said []string
	for _, w := range c.envWarnings {
		if strings.Contains(w, "BRIG_MEMORY") {
			said = append(said, w)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "ubuntu") {
		t.Errorf("the warnings about BRIG_MEMORY: %q, want one naming ubuntu", said)
	}
}

// BRIG_MEMORY=off on an agent brig does not wire asks for nothing, so it
// draws no warning.
func TestBrigMemoryOffIsQuietForAgentsBrigDoesNotWire(t *testing.T) {
	p, home := memorySession(t)
	p.Name = "ubuntu"
	for _, v := range []string{"off", "0", "false", "None"} {
		t.Setenv("BRIG_MEMORY", v)
		c := loadMemory(t, p, Options{Workspace: home}, nil)
		for _, w := range c.envWarnings {
			if strings.Contains(w, "BRIG_MEMORY") {
				t.Errorf("BRIG_MEMORY=%s on ubuntu warned: %q", v, w)
			}
		}
	}
}

// Memory narrows a shared run to isolated, and the network line says memory
// did it, as it says a policy did.
func TestMemoryNarrowingIsNamed(t *testing.T) {
	p, home := memorySession(t)
	p.Network = "shared"
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, nil)
	if line := c.networkLine(); !strings.Contains(line, "agent memory narrows shared to isolated") {
		t.Errorf("the network line of a narrowed run: %q", line)
	}
	c = loadMemory(t, p, Options{Workspace: home}, nil)
	if line := c.networkLine(); strings.Contains(line, "memory") {
		t.Errorf("the network line of a run without memory: %q", line)
	}
	// Already isolated: memory narrowed nothing.
	p.Network = "isolated"
	c = loadMemory(t, p, Options{Workspace: home, Memory: "team"}, nil)
	if line := c.networkLine(); strings.Contains(line, "memory") {
		t.Errorf("the network line of an isolated run with memory: %q", line)
	}
}

// The store is named once, when the sandbox boots. An exec of the running
// sandbox says nothing about it: under brigd every line said lands in the
// request's warnings, and an ensure that did everything right would carry one.
func TestMemoryLineOncePerBoot(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMemory(t, p, Options{Workspace: home, Memory: "team"}, rt)
	warn := &bytes.Buffer{}
	c.Err = warn
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "memory: team\n") {
		t.Errorf("the boot said %q, want it to name the store", warn.String())
	}
	rt.running = true
	c = loadMemory(t, p, Options{Workspace: home, NoProject: true}, rt)
	warn.Reset()
	c.Err = warn
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if rt.boots != 1 {
		t.Fatalf("booted %d times, want the first run's boot alone", rt.boots)
	}
	if strings.Contains(warn.String(), "memory:") {
		t.Errorf("an exec of the running sandbox said %q", warn.String())
	}
}
