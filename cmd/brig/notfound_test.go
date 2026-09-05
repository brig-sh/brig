package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// listRuntime answers what rm and logs ask before they act: whether a sandbox
// of a given name exists, and -- once they know it does -- the removal or the
// log stream. The interface is embedded rather than implemented, the repo's
// pattern: a call to any other method is one these verbs have no business
// making, and the nil panic says that louder than a stub returning nothing.
type listRuntime struct {
	runtime.Runtime
	instances []runtime.Instance
	listErr   error
	lists     int
	stopped   bool
	removed   bool
	logged    bool
}

func (r *listRuntime) List() ([]runtime.Instance, error) {
	r.lists++
	return r.instances, r.listErr
}

func (r *listRuntime) Stop(string) error           { r.stopped = true; return nil }
func (r *listRuntime) Remove(string) error         { r.removed = true; return nil }
func (r *listRuntime) Logs(runtime.LogsSpec) error { r.logged = true; return nil }

// present is a runtime that has brig-claude-code, absent one that has nothing.
func present() *listRuntime {
	return &listRuntime{instances: []runtime.Instance{{Name: "brig-claude-code", State: "running"}}}
}
func absent() *listRuntime { return &listRuntime{} }

// With no sandbox for the ref, rm is a name that resolves to nothing: exit 3,
// naming the ref the reader typed rather than the sandbox name they never chose.
// It used to hand back the runtime's own "instance not found" and exit 1.
func TestRemoveSandboxOnMissingIsNotFound(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	rt := absent()
	cfg := &wrap.Config{VMName: "brig-claude-code", Runtime: rt}

	err := removeSandbox(cfg, "claude")
	if err == nil {
		t.Fatal("rm of a missing sandbox was reported as success")
	}
	if _, ok := err.(*notFoundError); !ok {
		t.Errorf("rm of a missing sandbox is not a not-found error: %T: %v", err, err)
	}
	if exitCode(err) != exitNotFound {
		t.Errorf("rm of a missing sandbox exits %d, want %d", exitCode(err), exitNotFound)
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("the error does not name the ref: %v", err)
	}
	if rt.removed {
		t.Error("rm reached the runtime's Remove for a sandbox that is not there")
	}
}

// A sandbox that is there is removed as before: the check is a gate, not a
// detour.
func TestRemoveSandboxProceedsWhenPresent(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	rt := present()
	cfg := &wrap.Config{VMName: "brig-claude-code", Runtime: rt}

	if err := removeSandbox(cfg, "claude"); err != nil {
		t.Fatalf("rm of a present sandbox failed: %v", err)
	}
	if !rt.removed {
		t.Error("rm did not reach the runtime's Remove for a sandbox that is there")
	}
}

// A List that fails is a runtime that could not be asked, not a sandbox that is
// gone: the error comes back as is, so exitCode reads it as the runtime class
// rather than not-found. Turning "could not ask" into "not there" would erase a
// fact the README's exit table keeps apart.
func TestRemoveSandboxPropagatesAListError(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	boom := errors.New("cannot connect to the daemon")
	rt := &listRuntime{listErr: boom}
	cfg := &wrap.Config{VMName: "brig-claude-code", Runtime: rt}

	err := removeSandbox(cfg, "claude")
	if !errors.Is(err, boom) {
		t.Fatalf("a List error was not returned as is: %v", err)
	}
	if _, ok := err.(*notFoundError); ok {
		t.Error("a List error was misreported as not-found")
	}
	if rt.removed {
		t.Error("rm removed a sandbox after failing to list them")
	}
}

// The missing path still prunes: the index entry names a sandbox the user asked
// to be rid of, so rm clears it and exits 3 even though there was nothing for
// the runtime to remove. Otherwise a name nothing holds keeps a stale record.
func TestRemoveSandboxPrunesTheIndexOnMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	sessions := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(sessions,
		[]byte(`{"claude@refactor":{"home":"/ws","sandbox":"brig-claude-code"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := removeSandbox(&wrap.Config{VMName: "brig-claude-code", Runtime: absent()}, "claude@refactor")
	if exitCode(err) != exitNotFound {
		t.Fatalf("rm of a missing sandbox exits %d, want %d: %v", exitCode(err), exitNotFound, err)
	}
	blob, readErr := os.ReadFile(sessions)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(blob), "brig-claude-code") {
		t.Errorf("the index entry for the removed sandbox was not pruned: %s", blob)
	}
}

// logs mirrors rm: nothing to read from a sandbox that is not there is a
// not-found (exit 3) naming the ref, not a log stream that started and failed.
func TestLogsForOnMissingIsNotFound(t *testing.T) {
	rt := absent()
	err := logsFor(rt, "brig-claude-code", "claude", logsOptions{tail: -1}, io.Discard)
	if err == nil {
		t.Fatal("logs of a missing sandbox was reported as success")
	}
	if _, ok := err.(*notFoundError); !ok {
		t.Errorf("logs of a missing sandbox is not a not-found error: %T: %v", err, err)
	}
	if exitCode(err) != exitNotFound {
		t.Errorf("logs of a missing sandbox exits %d, want %d", exitCode(err), exitNotFound)
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("the error does not name the ref: %v", err)
	}
	if rt.logged {
		t.Error("logs streamed from a sandbox that is not there")
	}
}

// A sandbox that is there streams as before.
func TestLogsForProceedsWhenPresent(t *testing.T) {
	rt := present()
	if err := logsFor(rt, "brig-claude-code", "claude", logsOptions{tail: -1}, io.Discard); err != nil {
		t.Fatalf("logs of a present sandbox failed: %v", err)
	}
	if !rt.logged {
		t.Error("logs did not stream from a sandbox that is there")
	}
}

// A List that fails comes back as is here too, never as not-found.
func TestLogsForPropagatesAListError(t *testing.T) {
	boom := errors.New("cannot connect to the daemon")
	rt := &listRuntime{listErr: boom}
	err := logsFor(rt, "brig-claude-code", "claude", logsOptions{tail: -1}, io.Discard)
	if !errors.Is(err, boom) {
		t.Fatalf("a List error was not returned as is: %v", err)
	}
	if _, ok := err.(*notFoundError); ok {
		t.Error("a List error was misreported as not-found")
	}
	if rt.logged {
		t.Error("logs streamed after failing to list the sandboxes")
	}
}

// stop is the verb this change must not widen: stopping a sandbox that is not
// running is the end state stop asks for, not a failure, so it stays exit 0 and
// never consults the list. Pinned with the same double, whose List would flag it
// if stop grew the gate rm and logs have.
func TestStopIsNotGatedOnTheSandboxExisting(t *testing.T) {
	rt := absent()
	cfg := &wrap.Config{VMName: "brig-claude-code", Runtime: rt}
	if err := cfg.Stop(); err != nil {
		t.Errorf("stop of a missing sandbox was reported as a failure: %v", err)
	}
	if rt.lists != 0 {
		t.Errorf("stop consulted the sandbox list %d times; it must not gate on existence", rt.lists)
	}
}
