package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// agentRmHost gives an agent rm test a profile directory holding one file per
// name, a state directory of its own, and rt as the runtime every run-line
// detection returns. The state directory matters as much as the profiles: rm
// reads the session index to tell whose sandbox is whose, and the real index
// belongs to whoever runs the tests.
func agentRmHost(t *testing.T, rt runtime.Runtime, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	t.Setenv("BRIG_VERIFY", "off")
	t.Setenv("BRIG_RUNTIME", "")
	t.Setenv("BRIG_NAME", "")
	emptyPath(t)
	for _, name := range names {
		body := "name: " + name + "\nimage: img\nguestHome: /home/" + name +
			"\nbinary: agent\nmem: 1024\ncpus: 1\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) { return rt, nil })
	return dir
}

// stubDetectRuntimeFor swaps the run-line detection seam for the length of a
// test.
func stubDetectRuntimeFor(t *testing.T, fn func(runtime.Preference) (runtime.Runtime, error)) {
	t.Helper()
	prev := detectRuntimeFor
	detectRuntimeFor = fn
	t.Cleanup(func() { detectRuntimeFor = prev })
}

// noSandboxes answers run-line detection with a runtime that holds nothing,
// for an agent rm test that is not about sandboxes. Without it, rm asks the
// runtime on the PATH of the machine running the test, and a sandbox there
// decides whether the test passes.
func noSandboxes(t *testing.T) {
	t.Helper()
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		return &removeRuntime{}, nil
	})
}

// stillThere fails the test unless name still resolves and its file is still
// on disk, read back through a fresh load.
func stillThere(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name+".yaml")); err != nil {
		t.Errorf("%s.yaml is gone: %v", name, err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := profile.Lookup(name); !ok {
		t.Errorf("%s no longer resolves", name)
	}
}

// The bug in #367: agent rm deleted the file while a sandbox of the profile
// was up, and after that every ref'd verb answered "unknown profile" to it.
// Removing the sandboxes first is the order that works, so rm refuses and
// names the `brig rm` for each. -y answers the question about files. It does
// not skip this.
func TestRemoveProfileRefusesWhileItsSandboxesExist(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{
		{Name: "brig-mytool", State: "running"},
		{Name: "brig-mytool-x", State: "stopped"},
	}}
	dir := agentRmHost(t, rt, "mytool")
	_, err := run2(t, []string{"agent", "rm", "mytool", "-y"})
	if err == nil {
		t.Fatal("agent rm deleted a profile two sandboxes still use")
	}
	for _, want := range []string{"brig rm mytool ", "brig rm mytool@x", "brig-mytool-x", "stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	stillThere(t, dir, "mytool")
	if len(rt.removed) != 0 {
		t.Errorf("agent rm removed sandboxes %v", rt.removed)
	}
}

// A stopped sandbox is stranded the same way a running one is: `brig rm` needs
// the profile to reach either.
func TestRemoveProfileRefusesForAStoppedSandbox(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-mytool", State: "stopped"}}}
	dir := agentRmHost(t, rt, "mytool")
	if _, err := run2(t, []string{"agent", "rm", "mytool", "-y"}); err == nil {
		t.Fatal("agent rm deleted a profile a stopped sandbox still uses")
	}
	stillThere(t, dir, "mytool")
}

// The recorded ref wins over the name, the same as in `brig ls`. brig-mine-two
// reads as the mine-two profile by name alone, but the index says it is
// mine@two. So removing mine is refused, and the command the refusal names
// reaches that sandbox.
func TestRemoveProfileRefusesWhenTheIndexNamesTheSandbox(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-mine-two", State: "running"}}}
	dir := agentRmHost(t, rt, "mine", "mine-two")
	index := `{"mine@two": {"home": "/ws/two", "sandbox": "brig-mine-two"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run2(t, []string{"agent", "rm", "mine", "-y"})
	if err == nil {
		t.Fatal("agent rm deleted a profile the index says a sandbox belongs to")
	}
	if !strings.Contains(err.Error(), "brig rm mine@two") {
		t.Errorf("the refusal does not name `brig rm mine@two`: %v", err)
	}
	stillThere(t, dir, "mine")

	out, err := run2(t, []string{"rm", "mine@two", "--dry-run"})
	if err != nil {
		t.Fatalf("the command the refusal names does not work: %v", err)
	}
	if !strings.Contains(out, "brig-mine-two") {
		t.Errorf("`brig rm mine@two` does not reach brig-mine-two:\n%s", out)
	}
}

// listFailsRuntime is a runtime that cannot say what it holds.
type listFailsRuntime struct{ removeRuntime }

func (r *listFailsRuntime) List() ([]runtime.Instance, error) {
	return nil, errors.New("ps failed")
}

// When brig cannot ask the runtime, it cannot tell whether a sandbox is left
// stranded, and deleting the file is the step that cannot be undone. So rm
// fails, the same way `brig rm <ref>` fails on the same runtime.
func TestRemoveProfileFailsClosedWhenTheRuntimeCannotList(t *testing.T) {
	dir := agentRmHost(t, &listFailsRuntime{}, "mytool")
	_, err := run2(t, []string{"agent", "rm", "mytool", "-y"})
	if err == nil || !strings.Contains(err.Error(), "ps failed") {
		t.Fatalf("agent rm with a runtime that cannot list: %v, want its error", err)
	}
	stillThere(t, dir, "mytool")
}

// A sandbox of another profile whose name starts with this one's is not this
// profile's sandbox. brig-mine-two with no index entry is mine-two's, the
// longest match, as `brig ls` reads it.
func TestRemoveProfileLeavesAnotherProfilesSandboxAlone(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-mine-two", State: "running"}}}
	dir := agentRmHost(t, rt, "mine", "mine-two")
	if _, err := run2(t, []string{"agent", "rm", "mine", "-y"}); err != nil {
		t.Fatalf("agent rm refused over another profile's sandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); !os.IsNotExist(err) {
		t.Error("mine.yaml is still there")
	}
	stillThere(t, dir, "mine-two")
	if len(rt.removed) != 0 {
		t.Errorf("agent rm removed sandboxes %v", rt.removed)
	}
}

// No runtime on PATH means no sandboxes, which is how `brig ls` reads it too.
// rm goes ahead.
func TestRemoveProfileProceedsWithNoRuntime(t *testing.T) {
	dir := agentRmHost(t, nil, "mytool")
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		return nil, runtime.ErrNoRuntime
	})
	if _, err := run2(t, []string{"agent", "rm", "mytool", "-y"}); err != nil {
		t.Fatalf("agent rm with no runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytool.yaml")); !os.IsNotExist(err) {
		t.Error("mytool.yaml is still there")
	}
}

// Removing an override of a built-in leaves the name resolving to the
// built-in, so `brig rm claude-code` still reaches the sandbox. There is
// nothing to strand, and rm does not ask the runtime at all.
func TestRemoveProfileOfAnOverrideIgnoresTheSandbox(t *testing.T) {
	dir := agentRmHost(t, nil)
	blob := []byte("name: claude-code\nimage: docker.io/me/pinned:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 1\ncpus: 1\n")
	path := filepath.Join(dir, "claude-code.yaml")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		t.Error("removing an override asked the runtime")
		return &removeRuntime{list: []runtime.Instance{{Name: "brig-claude-code", State: "running"}}}, nil
	})
	if _, err := run2(t, []string{"agent", "rm", "claude-code", "-y"}); err != nil {
		t.Fatalf("agent rm of an override: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the override file is still there")
	}
}

// The check asks the runtime `brig rm <ref>` uses for this profile, which
// is the profile's own runtimeBin when it sets one. Asking the one on PATH
// instead looks where the suggested command does not act, and a sandbox of a
// profile pinned to another build is stranded without a word. The index holds
// a session of the profile, so it has been run and its runtimeBin with it.
func TestRemoveProfileAsksTheProfilesOwnRuntime(t *testing.T) {
	dir := agentRmHost(t, nil)
	index := `{"mytool": {"home": "/ws/mytool", "sandbox": "brig-mytool"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	const bin = "/opt/hull-dev/hull"
	body := "name: mytool\nimage: img\nguestHome: /home/mytool\nbinary: agent\n" +
		"mem: 1024\ncpus: 1\nruntimeBin: " + bin + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mytool.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubDetectRuntimeFor(t, func(pref runtime.Preference) (runtime.Runtime, error) {
		if pref.Bin != bin {
			t.Errorf("agent rm asked the runtime %q, want the profile's %q", pref.Bin, bin)
		}
		return &removeRuntime{list: []runtime.Instance{{Name: "brig-mytool", State: "running"}}}, nil
	})
	if _, err := run2(t, []string{"agent", "rm", "mytool", "-y"}); err == nil {
		t.Fatal("agent rm deleted a profile a sandbox on its own runtime still uses")
	}
	stillThere(t, dir, "mytool")
}

// A profile carries its runtimeBin as a plain field, and one imported from
// someone else can name any executable on this host. `brig run` of that
// profile executes it, and that is the run the person asked for. agent rm is
// how someone gets rid of a profile they chose not to run, so it must not
// execute the program the file names. With no session of the profile in the
// index, rm asks the runtime on PATH, the one `brig ls` asks.
func TestRemoveProfileDoesNotRunTheRuntimeBinOfAProfileNeverRun(t *testing.T) {
	dir := agentRmHost(t, nil)
	stubDetectRuntimeFor(t, runtime.DetectFor)
	t.Setenv("BRIG_RUNTIME", "hull")
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "planted")
	script := "#!/bin/sh\n: > " + marker + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: mytool\nimage: img\nguestHome: /home/mytool\nbinary: agent\n" +
		"mem: 1024\ncpus: 1\nruntimeBin: " + bin + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mytool.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	if _, err := run2(t, []string{"agent", "rm", "mytool", "-y"}); err != nil {
		t.Fatalf("agent rm of a profile never run, with no runtime on PATH: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("agent rm executed the runtimeBin named by the profile it was removing")
	}
	if _, err := os.Stat(filepath.Join(dir, "mytool.yaml")); !os.IsNotExist(err) {
		t.Error("mytool.yaml is still there")
	}
}

// A runtime that is named and broken, such as an unknown BRIG_RUNTIME or a
// runtimeBin that is not there, is a mistake to fix, the same as in `brig ls`.
// It is not an empty list. So rm refuses with the runtime's exit code, and says
// why it asked the runtime at all, or the refusal reads as unrelated to
// removing a file.
func TestRemoveProfileFailsClosedOnABrokenRuntime(t *testing.T) {
	dir := agentRmHost(t, nil, "mytool")
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		return nil, fmt.Errorf("%w: unknown BRIG_RUNTIME %q", runtime.ErrBadRuntime, "bogus")
	})
	_, err := run2(t, []string{"agent", "rm", "mytool", "-y"})
	if err == nil {
		t.Fatal("agent rm went ahead on a runtime it cannot ask")
	}
	for _, want := range []string{"bogus", "cannot tell whether a sandbox of mytool exists"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if got := exitCode(err); got != exitRuntime {
		t.Errorf("exit code %d, want %d", got, exitRuntime)
	}
	stillThere(t, dir, "mytool")
}

// The question about files waits for as long as the person takes, and a
// sandbox of the profile can boot in that time. The check before the question
// saw nothing, so without a second one after it the yes deletes the file under
// the new sandbox, which is #367 again. other.yaml declaring mytool makes the
// question come up, because the file is not named after the argument.
func TestRemoveProfileChecksAgainAfterThePrompt(t *testing.T) {
	dir := agentRmHost(t, nil)
	path := filepath.Join(dir, "other.yaml")
	body := "name: mytool\nimage: img\nguestHome: /home/mytool\nbinary: agent\nmem: 1024\ncpus: 1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubDetectRuntimeFor(t, func(runtime.Preference) (runtime.Runtime, error) {
		calls++
		if calls == 1 {
			return &removeRuntime{}, nil
		}
		return &removeRuntime{list: []runtime.Instance{{Name: "brig-mytool", State: "running"}}}, nil
	})
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() {
		_, err = run2(t, []string{"agent", "rm", "mytool"})
	})
	if err == nil {
		t.Fatal("agent rm deleted a profile a sandbox booted during the prompt uses")
	}
	if !strings.Contains(err.Error(), "brig rm mytool") {
		t.Errorf("the refusal does not name `brig rm mytool`: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("other.yaml is gone: %v", err)
	}
}

// A sandbox booted under BRIG_NAME carries that name, and `brig rm <ref>`
// builds the name from BRIG_NAME again, not from the index. So for such a
// sandbox the refusal names the BRIG_NAME to set, and the command it names
// reaches the sandbox.
func TestRemoveProfileNamesTheBrigNameASandboxWasBootedWith(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-custom-x", State: "running"}}}
	dir := agentRmHost(t, rt, "mytool")
	withRuntime(t, rt)
	index := `{"mytool@x": {"home": "/ws/x", "sandbox": "brig-custom-x"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run2(t, []string{"agent", "rm", "mytool", "-y"})
	if err == nil {
		t.Fatal("agent rm deleted a profile a renamed sandbox still uses")
	}
	if !strings.Contains(err.Error(), "BRIG_NAME=brig-custom brig rm mytool@x") {
		t.Errorf("the refusal does not name the BRIG_NAME to set: %v", err)
	}
	stillThere(t, dir, "mytool")

	t.Setenv("BRIG_NAME", "brig-custom")
	out, err := run2(t, []string{"rm", "mytool@x", "--dry-run"})
	if err != nil {
		t.Fatalf("the command the refusal names does not work: %v", err)
	}
	if !strings.Contains(out, "brig-custom-x") {
		t.Errorf("`BRIG_NAME=brig-custom brig rm mytool@x` does not reach brig-custom-x:\n%s", out)
	}
}

// The line the refusal prints runs in the shell that ran agent rm, and
// `brig rm <ref>` builds the name from that shell's naming variables. A
// BRIG_NAME or BRIG_<AGENT>_NAME exported there makes the plain command miss a
// sandbox with the default name, so the refusal sets the variable that wins,
// to the value the sandbox was booted with.
func TestRemoveProfileNamesTheVariableTheShellWouldOverride(t *testing.T) {
	for _, tc := range []struct{ name, variable string }{
		{"global", "BRIG_NAME"},
		{"per-agent", "BRIG_MYTOOL_NAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &removeRuntime{list: []runtime.Instance{{Name: "brig-mytool", State: "running"}}}
			dir := agentRmHost(t, rt, "mytool")
			withRuntime(t, rt)
			t.Setenv(tc.variable, "brig-foo")
			_, err := run2(t, []string{"agent", "rm", "mytool", "-y"})
			if err == nil {
				t.Fatal("agent rm deleted a profile a sandbox still uses")
			}
			want := tc.variable + "=brig-mytool brig rm mytool "
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q: %v", want, err)
			}
			stillThere(t, dir, "mytool")

			t.Setenv(tc.variable, "brig-mytool")
			out, err := run2(t, []string{"rm", "mytool", "--dry-run"})
			if err != nil {
				t.Fatalf("the command the refusal names does not work: %v", err)
			}
			if !strings.Contains(out, "brig-mytool") {
				t.Errorf("`%sbrig rm mytool` does not reach brig-mytool:\n%s", want, out)
			}
		})
	}
}

// Only a sandbox of this profile blocks the rm. A container that is not
// brig's, even one named after the profile, is not a sandbox of it, and a
// sandbox whose profile is already gone belongs to no profile brig can name.
func TestRemoveProfileIgnoresSandboxesItDoesNotOwn(t *testing.T) {
	rt := &removeRuntime{list: []runtime.Instance{
		{Name: "mytool", State: "running"},
		{Name: "brig-gone", State: "stopped"},
	}}
	dir := agentRmHost(t, rt, "mytool")
	if _, err := run2(t, []string{"agent", "rm", "mytool", "-y"}); err != nil {
		t.Fatalf("agent rm refused over sandboxes that are not mytool's: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytool.yaml")); !os.IsNotExist(err) {
		t.Error("mytool.yaml is still there")
	}
	if len(rt.removed) != 0 {
		t.Errorf("agent rm removed sandboxes %v", rt.removed)
	}
}
