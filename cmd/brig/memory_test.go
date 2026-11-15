package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
)

// memoryHost points the state directory at a temp dir and returns the memory
// root under it, so a test never reads or removes the stores of whoever runs
// it.
func memoryHost(t *testing.T) (state, root string) {
	t.Helper()
	state = t.TempDir()
	t.Setenv("BRIG_STATE_DIR", state)
	return state, filepath.Join(state, "memory")
}

// putMemories fills the store key with n memories and, for a project store,
// records the project path.
func putMemories(t *testing.T, root, key, project string, n int) {
	t.Helper()
	s, err := memory.OpenFiles(root, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if project != "" {
		if err := s.SetProject(project); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		m := memory.Memory{Name: "m" + string(rune('a'+i)), Description: "d", Body: "b"}
		if err := s.Put(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
}

// useStore records that the sandbox vm, which the session index names, was
// booted with the store key.
func useStore(t *testing.T, state, vm, key string, indexed bool) {
	t.Helper()
	rec := `{"` + vm + `": {"key": "` + key + `", "mode": "named"}}`
	if err := os.WriteFile(filepath.Join(state, "memory.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		return
	}
	idx := `{"claude": {"home": "/h", "sandbox": "` + vm + `"}}`
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryLsListsStores(t *testing.T) {
	state, root := memoryHost(t)
	dir := t.TempDir()
	pk := memory.ProjectKey(dir)
	putMemories(t, root, "team", "", 2)
	putMemories(t, root, pk, dir, 1)
	useStore(t, state, "brig-claude", "team", true)

	out, err := captureStdout(t, func() error { return run([]string{"memory", "ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header and two rows, got:\n%s", out)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "STORE PROJECT MEMORIES IN USE" {
		t.Errorf("header = %q", lines[0])
	}
	// Sorted by store: project-<hex> before team.
	if f := strings.Fields(lines[1]); f[0] != pk || f[1] != dir || f[2] != "1" || f[3] != "-" {
		t.Errorf("project row = %q", lines[1])
	}
	if f := strings.Fields(lines[2]); strings.Join(f, " ") != "team - 2 brig-claude" {
		t.Errorf("named row = %q", lines[2])
	}
}

func TestMemoryLsEmpty(t *testing.T) {
	memoryHost(t)
	out, err := captureStdout(t, func() error { return run([]string{"memory", "ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "STORE") {
		t.Errorf("an empty listing printed a header:\n%s", out)
	}
	out, err = captureStdout(t, func() error { return run([]string{"memory", "ls", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Data []memoryJSON `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if doc.Data == nil || len(doc.Data) != 0 {
		t.Errorf("empty --json data = %v, want []", doc.Data)
	}
}

func TestMemoryLsJSON(t *testing.T) {
	state, root := memoryHost(t)
	putMemories(t, root, "team", "", 2)
	putMemories(t, root, "solo", "", 0)
	useStore(t, state, "brig-claude", "team", true)

	for _, args := range [][]string{{"memory", "ls", "--json"}, {"--json", "memory", "ls"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var doc struct {
			Kind string       `json:"kind"`
			Data []memoryJSON `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: %v:\n%s", args, err, out)
		}
		if doc.Kind != "MemoryList" || len(doc.Data) != 2 {
			t.Fatalf("%v: kind %q, data %+v", args, doc.Kind, doc.Data)
		}
		solo, team := doc.Data[0], doc.Data[1]
		if solo.Store != "solo" || solo.Memories != 0 || len(solo.InUse) != 0 {
			t.Errorf("solo = %+v", solo)
		}
		if team.Store != "team" || team.Memories != 2 || len(team.InUse) != 1 || team.InUse[0] != "brig-claude" {
			t.Errorf("team = %+v", team)
		}
		// The keys are the documented lowercase ones.
		for _, k := range []string{`"store"`, `"project"`, `"memories"`, `"inUse"`} {
			if !strings.Contains(out, k) {
				t.Errorf("%v: output has no %s:\n%s", args, k, out)
			}
		}
	}
}

func TestMemoryRmRemoves(t *testing.T) {
	_, root := memoryHost(t)
	putMemories(t, root, "team", "", 1)
	putMemories(t, root, "other", "", 1)

	out, err := captureStdout(t, func() error { return run([]string{"memory", "rm", "team"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed store team") {
		t.Errorf("output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "team")); !os.IsNotExist(err) {
		t.Error("the store is still there")
	}
	if _, err := os.Stat(filepath.Join(root, "other")); err != nil {
		t.Errorf("another store was touched: %v", err)
	}
}

func TestMemoryRmRefusesInUse(t *testing.T) {
	state, root := memoryHost(t)
	putMemories(t, root, "team", "", 1)
	useStore(t, state, "brig-claude", "team", true)

	err := run([]string{"memory", "rm", "team"})
	if err == nil || err.Error() != "store team is in use by brig-claude; remove that sandbox first" {
		t.Fatalf("err = %v", err)
	}
	if exitCode(err) == exitOK {
		t.Error("a refusal exited 0")
	}
	if _, err := os.Stat(filepath.Join(root, "team")); err != nil {
		t.Errorf("a refused rm removed the store: %v", err)
	}
}

// A memory.json entry whose sandbox no session names is a leftover and never
// blocks the removal.
func TestMemoryRmIgnoresStaleRecord(t *testing.T) {
	state, root := memoryHost(t)
	putMemories(t, root, "team", "", 1)
	useStore(t, state, "brig-gone", "team", false)
	if err := run([]string{"memory", "rm", "team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "team")); !os.IsNotExist(err) {
		t.Error("the store is still there")
	}
}

func TestMemoryRmByProject(t *testing.T) {
	_, root := memoryHost(t)
	dir := t.TempDir()
	pk := memory.ProjectKey(dir)
	putMemories(t, root, pk, dir, 1)

	if _, err := captureStdout(t, func() error { return run([]string{"memory", "rm", "--project", dir}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, pk)); !os.IsNotExist(err) {
		t.Error("the project's store is still there")
	}
	// A project key names the same store.
	putMemories(t, root, pk, dir, 1)
	if _, err := captureStdout(t, func() error { return run([]string{"memory", "rm", pk}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, pk)); !os.IsNotExist(err) {
		t.Error("the store named by its key is still there")
	}
	// A relative folder is resolved against the working directory.
	t.Chdir(dir)
	putMemories(t, root, memory.ProjectKey(dir), dir, 1)
	if _, err := captureStdout(t, func() error { return run([]string{"memory", "rm", "--project", "."}) }); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryRmRefusesTraversal(t *testing.T) {
	state, root := memoryHost(t)
	putMemories(t, root, "team", "", 1)
	victim := filepath.Join(state, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../victim", "../x", "a/b", "..", ".", "project-xyz"} {
		err := run([]string{"memory", "rm", name})
		if err == nil {
			t.Errorf("rm %q succeeded", name)
			continue
		}
		if !errors.Is(err, memory.ErrInvalidName) && exitCode(err) != exitUsage {
			t.Errorf("rm %q = %v, want a refusal of the name", name, err)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a traversal name reached a file outside the store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "team")); err != nil {
		t.Errorf("a refused name removed a store: %v", err)
	}
}

func TestMemoryRmUnknownStore(t *testing.T) {
	memoryHost(t)
	err := run([]string{"memory", "rm", "nope"})
	if err == nil || exitCode(err) != exitNotFound {
		t.Fatalf("err = %v (exit %d), want not found", err, exitCode(err))
	}
}

func TestMemoryRmNeedsOneStore(t *testing.T) {
	memoryHost(t)
	for _, args := range [][]string{
		{"memory", "rm"},
		{"memory", "rm", "a", "b"},
		{"memory", "rm", "a", "--project", "."},
	} {
		if err := run(args); err == nil || exitCode(err) != exitUsage {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
}

// `memory` in verb position is the verb; --memory on a run line is still the
// RAM alias.
func TestMemoryVerbAndRAMFlagStayApart(t *testing.T) {
	memoryHost(t)
	if err := run([]string{"memory"}); err == nil || !strings.Contains(err.Error(), "memory needs a subcommand") {
		t.Errorf("bare memory = %v", err)
	}
	if err := run([]string{"memory", "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown memory subcommand") {
		t.Errorf("memory bogus = %v", err)
	}
	opts, _, _, err := parse("run", []string{"--memory", "2048", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.load.Mem != 2048 || opts.load.Memory != "" {
		t.Errorf("--memory 2048 = RAM %d, store %q; want RAM 2048 and no store", opts.load.Mem, opts.load.Memory)
	}
}

func TestMemoryHelp(t *testing.T) {
	for _, args := range [][]string{{"memory", "--help"}, {"help", "memory"}, {"memory", "ls", "--help"}, {"memory", "rm", "-h"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil || !strings.Contains(out, "brig memory") {
			t.Errorf("%v: %q, %v", args, out, err)
		}
	}
}

// fakeMemoryBroker stands for a running broker that serves memory to the
// sandboxes in memory: a pid file naming this process and a control socket
// that answers status. The state directory is short, so the socket path
// fits.
func fakeMemoryBroker(t *testing.T, memory ...string) (state, root string) {
	t.Helper()
	state, err := os.MkdirTemp("", "bm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	t.Setenv("BRIG_STATE_DIR", state)
	dir, err := injector.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start, err := injector.ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, injector.PIDName), []byte(injector.PIDLine(os.Getpid(), start)), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, injector.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(injector.Status{PID: os.Getpid(), Port: 1, Memory: memory})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(st)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return state, filepath.Join(state, "memory")
}

// A lost memory.json must not let rm remove a store a running agent uses:
// a sandbox the broker serves memory to and brig has no record of may be on
// any store, so rm refuses, naming it. One whose record names another store
// does not stop the removal.
func TestMemoryRmRefusesBrokerSandboxWithoutRecord(t *testing.T) {
	state, root := fakeMemoryBroker(t, "brig-lost")
	putMemories(t, root, "team", "", 1)
	err := run([]string{"memory", "rm", "team"})
	if err == nil || !strings.Contains(err.Error(), "brig-lost") {
		t.Fatalf("rm with an unrecorded memory sandbox = %v, want a refusal naming brig-lost", err)
	}
	if _, err := os.Stat(filepath.Join(root, "team")); err != nil {
		t.Errorf("a refused rm removed the store: %v", err)
	}
	// Recorded with this store but not named by a session: still refused,
	// because the broker serves it.
	useStore(t, state, "brig-lost", "team", false)
	if err := run([]string{"memory", "rm", "team"}); err == nil || !strings.Contains(err.Error(), "brig-lost") {
		t.Fatalf("rm with a served sandbox recorded on the store = %v, want a refusal naming brig-lost", err)
	}
	useStore(t, state, "brig-lost", "other", false)
	if err := run([]string{"memory", "rm", "team"}); err != nil {
		t.Fatalf("rm with the served sandbox on another store: %v", err)
	}
}

// memory is a verb, and a verb is always a verb: a profile of yours called
// memory does not take `brig memory` from the store commands. It runs as any
// profile does, with `brig run memory`.
func TestAProfileNamedMemoryRunsWithRun(t *testing.T) {
	scratchHost(t)
	blob := "name: memory\nimage: i\nguestHome: /home/memory\nbinary: m\nmem: 1\ncpus: 1\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_PROFILE_DIR"), "memory.yaml"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"memory"}); err == nil || !strings.Contains(err.Error(), "memory needs a subcommand") {
		t.Errorf("brig memory with a profile called memory: %v, want the memory commands", err)
	}
	_, err := captureStdout(t, func() error { return run([]string{"run", "memory"}) })
	if !took(err) {
		t.Errorf("brig run memory did not reach the profile called memory: %v", err)
	}
	// And the ref is the profile's: a store command it is not.
	if err != nil && strings.Contains(err.Error(), "subcommand") {
		t.Errorf("brig run memory reached the memory commands: %v", err)
	}
}
