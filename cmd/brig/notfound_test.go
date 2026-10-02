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

	err := removeSandbox(cfg, "claude", false)
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

	if err := removeSandbox(cfg, "claude", false); err != nil {
		t.Fatalf("rm of a present sandbox failed: %v", err)
	}
	if !rt.removed {
		t.Error("rm did not reach the runtime's Remove for a sandbox that is there")
	}
}

// A List that fails is a runtime that could not be asked, not a sandbox that is
// gone: the error comes back as is, so exitCode reads it as the runtime class
// rather than not-found. Turning "could not ask" into "not there" would erase a
// fact docs/cli.md's exit table keeps apart.
func TestRemoveSandboxPropagatesAListError(t *testing.T) {
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	boom := errors.New("cannot connect to the daemon")
	rt := &listRuntime{listErr: boom}
	cfg := &wrap.Config{VMName: "brig-claude-code", Runtime: rt}

	err := removeSandbox(cfg, "claude", false)
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

// The missing path does not prune. "Not in the list" is not always "gone":
// hull.List falls back to a plain ps on a hull without ps -a, and that listing
// carries only the running instances, so a stopped sandbox reads as absent
// while it is still in hull's store. Its index entry stays until a removal
// actually happens, the runtime reports no sandbox of that name, or ls prunes.
func TestRemoveSandboxKeepsTheIndexOnMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	sessions := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(sessions,
		[]byte(`{"claude@refactor":{"home":"/ws","sandbox":"brig-claude-code"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := removeSandbox(&wrap.Config{VMName: "brig-claude-code", Runtime: absent()}, "claude@refactor", false)
	if exitCode(err) != exitNotFound {
		t.Fatalf("rm of a missing sandbox exits %d, want %d: %v", exitCode(err), exitNotFound, err)
	}
	blob, readErr := os.ReadFile(sessions)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(blob), "brig-claude-code") {
		t.Errorf("the index entry was pruned on a listing that may not have been complete: %s", blob)
	}
}

// existsRuntime is a listRuntime that can also say whether a sandbox exists
// when the listing leaves stopped ones out.
type existsRuntime struct {
	*listRuntime
	exists bool
	err    error
}

func (r *existsRuntime) Exists(string) (bool, error) { return r.exists, r.err }

// A sandbox removed outside brig leaves a session behind. Once the runtime
// reports no sandbox of that name, rm forgets the session, its slug claim and
// its network record, so the next run starts a new one. A sandbox the listing
// missed but the runtime still has keeps all three, and so does one the
// runtime cannot answer for: an error is not an absence. --dry-run says that
// rm would forget the session, and forgets nothing.
func TestRemoveSandboxForgetsTheSessionOfAReportedAbsence(t *testing.T) {
	for _, tt := range []struct {
		name       string
		exists     bool
		err        error
		dryRun     bool
		wantForget bool
		wantSay    string
	}{
		{"reported absent", false, nil, false, true, "so brig forgot its session"},
		{"stopped and missed by the listing", true, nil, false, false, "`brig ls` lists them"},
		{"runtime cannot say", false, errors.New("inspect: store locked"), false, false, "`brig ls` lists them"},
		{"dry run", false, nil, true, false, "so `brig rm claude@refactor` would forget its session"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := lostSession(t)
			sessions := filepath.Join(dir, "sessions.json")
			rt := &existsRuntime{listRuntime: absent(), exists: tt.exists, err: tt.err}
			err := removeSandbox(&wrap.Config{VMName: "brig-claude-code", Runtime: rt}, "claude@refactor", tt.dryRun)
			if exitCode(err) != exitNotFound {
				t.Fatalf("rm of a sandbox not in the listing exits %d, want %d: %v", exitCode(err), exitNotFound, err)
			}
			if forgot := strings.Contains(err.Error(), "forgot its session"); forgot != tt.wantForget {
				t.Errorf("rm said it forgot the session: %v, want %v: %v", forgot, tt.wantForget, err)
			}
			if !strings.Contains(err.Error(), tt.wantSay) {
				t.Errorf("rm does not say %q: %v", tt.wantSay, err)
			}
			claims, readErr := os.ReadFile(filepath.Join(dir, "slug-claims.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kept := strings.Contains(string(claims), "brig-claude-code"); kept == tt.wantForget {
				t.Errorf("slug claim kept: %v, want %v: %s", kept, !tt.wantForget, claims)
			}
			blob, readErr := os.ReadFile(sessions)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kept := strings.Contains(string(blob), "brig-claude-code"); kept == tt.wantForget {
				t.Errorf("index entry kept: %v, want %v: %s", kept, !tt.wantForget, blob)
			}
			word, recErr := runtime.BootedNet("brig-claude-code")
			if recErr != nil {
				t.Fatal(recErr)
			}
			if kept := word != ""; kept == tt.wantForget {
				t.Errorf("network record kept: %v, want %v", kept, !tt.wantForget)
			}
			if rt.removed {
				t.Error("rm reached the runtime's Remove for a sandbox not in the listing")
			}
		})
	}
}

// rm of a ref brig has no session for, a typo or a ref ls already pruned, is
// the commonest not-found, and both runtimes now answer Exists for it. There
// is no session to forget, so rm and its preview give the plain not-found and
// say nothing about forgetting one, or about what stays on the host.
func TestRemoveSandboxWithNoSessionForgetsNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		dryRun bool
	}{
		{"rm", false},
		{"dry run", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BRIG_STATE_DIR", t.TempDir())
			t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
			rt := &existsRuntime{listRuntime: absent(), exists: false}
			var err error
			stderr := captureStderr(t, func() {
				err = removeSandbox(&wrap.Config{VMName: "brig-claude-code", Runtime: rt}, "claude@typo", tt.dryRun)
			})
			if exitCode(err) != exitNotFound {
				t.Fatalf("rm of a ref with no sandbox exits %d, want %d: %v", exitCode(err), exitNotFound, err)
			}
			if !strings.Contains(err.Error(), "`brig ls` lists them") {
				t.Errorf("rm does not give the plain not-found: %v", err)
			}
			if strings.Contains(err.Error(), "forgot") || strings.Contains(err.Error(), "forget") {
				t.Errorf("rm talks about forgetting a session it never had: %v", err)
			}
			if strings.Contains(stderr, "stays on the host") {
				t.Errorf("rm says what it leaves for a session it never had:\n%s", stderr)
			}
			if rt.removed {
				t.Error("rm reached the runtime's Remove for a sandbox not in the listing")
			}
		})
	}
}

// lostSession writes what brig keeps for the session claude@refactor on
// brig-claude-code: its index entry, its slug claim and an offline network
// record. It returns the state directory.
func lostSession(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"),
		[]byte(`{"claude@refactor":{"home":"/ws","sandbox":"brig-claude-code"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slug-claims.json"),
		[]byte(`{"brig-claude-code":"refactor"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", "none"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rm forgets the session before the network record. When the index cannot be
// rewritten, it keeps the record too and does not say it forgot anything: an
// entry without its record is the state #432 is about, which makes the next
// flagless run ask the runtime and be refused. Kept whole, that run boots on
// the recorded network.
func TestRemoveSandboxKeepsTheSessionWholeWhenItCannotForgetIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a mode-500 directory, so this cannot be reproduced")
	}
	dir := lostSession(t)
	// Readable, so the entry is found; not writable, so the rewrite fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	rt := &existsRuntime{listRuntime: absent(), exists: false}
	err := removeSandbox(&wrap.Config{VMName: "brig-claude-code", Runtime: rt}, "claude@refactor", false)
	if err == nil {
		t.Fatal("rm reported nothing with an index it could not rewrite")
	}
	if strings.Contains(err.Error(), "forgot its session") {
		t.Errorf("rm said it forgot a session it could not forget: %v", err)
	}
	if !strings.Contains(err.Error(), "could not forget its session") {
		t.Errorf("rm does not say it could not forget the session: %v", err)
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("exit %d, want %d: %v", got, exitFailure, err)
	}
	blob, readErr := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(blob), "brig-claude-code") {
		t.Errorf("the index entry is gone: %s", blob)
	}
	if word, recErr := runtime.BootedNet("brig-claude-code"); recErr != nil || word != "none" {
		t.Errorf("the network record was dropped from a session that stays: %q, %v", word, recErr)
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
