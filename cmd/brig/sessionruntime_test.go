package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// otherKindRuntime is a runtime of kind that has no sandbox at all, which is
// what every sandbox of another runtime looks like to it.
type otherKindRuntime struct {
	*existsRuntime
	kind string
}

func (r *otherKindRuntime) Kind() string { return r.kind }

// sessionOn records the session probe@x1 as booted on kind, with a guest
// home brig created and some work in it, and returns the home.
func sessionOn(t *testing.T, kind string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	home := filepath.Join(dir, "homes", "brig-probe-x1")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "work"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"probe@x1":{"home":"` + home + `","sandbox":"brig-probe-x1","ephemeral":true,"runtime":"` + kind + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// pathWith puts an executable named kind on a PATH of its own, or nothing when
// kind is "", so a runtime reads as on PATH or off it.
func pathWith(t *testing.T, kind string) {
	t.Helper()
	dir := t.TempDir()
	if kind != "" {
		if err := os.WriteFile(filepath.Join(dir, kind), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// Negative: rm under a runtime that does not have the sandbox, because it is
// on another, is refused before anything is touched. That runtime reports the
// sandbox absent, and rm used to revoke its tokens and forget its session,
// after which the next run there deleted the home under the VM still on it.
// It is refused with that runtime off PATH too: BRIG_RUNTIME_BIN or a
// profile's runtimeBin may still reach it, and its sandbox may still run.
func TestRemoveOnAnotherRuntimeTouchesNothing(t *testing.T) {
	for _, onPath := range []string{"nerdctl", ""} {
		home := sessionOn(t, "nerdctl")
		pathWith(t, onPath)
		var revoked []string
		swap(t, &revokeRoutes, func(name string) error {
			revoked = append(revoked, name)
			return nil
		})
		rt := &otherKindRuntime{existsRuntime: &existsRuntime{listRuntime: absent()}, kind: "hull"}
		err := removeSandbox(&wrap.Config{VMName: "brig-probe-x1", Runtime: rt}, "probe@x1", false)
		var other *wrap.OtherRuntimeError
		if !errors.As(err, &other) || other.Recorded != "nerdctl" {
			t.Fatalf("PATH with %q: rm on hull: %v, want it refused as nerdctl's", onPath, err)
		}
		if len(revoked) != 0 || rt.removed || rt.stopped {
			t.Errorf("PATH with %q: rm acted on a sandbox of another runtime: revoked %v, removed %v, stopped %v",
				onPath, revoked, rt.removed, rt.stopped)
		}
		if got := wrap.WorkspaceOfSandbox("brig-probe-x1"); got != home {
			t.Errorf("PATH with %q: rm forgot the session of a sandbox on another runtime: %q", onPath, got)
		}
		if _, err := os.Stat(filepath.Join(home, "work")); err != nil {
			t.Errorf("PATH with %q: the guest home lost its work: %v", onPath, err)
		}
	}
}

// A session recorded on a runtime that is gone is refused by every verb and
// kept by ls, so its ref was stuck with only an edit of sessions.json past it.
// rm --forget forgets it: its tokens, its session and its records go, and the
// guest home stays. It touches no runtime, and --dry-run forgets nothing.
func TestForgetForgetsASessionOnAnotherRuntime(t *testing.T) {
	home := sessionOn(t, "nerdctl")
	pathWith(t, "")
	var revoked []string
	swap(t, &revokeRoutes, func(name string) error {
		revoked = append(revoked, name)
		return nil
	})
	rt := &otherKindRuntime{existsRuntime: &existsRuntime{listRuntime: absent()}, kind: "hull"}
	cfg := &wrap.Config{VMName: "brig-probe-x1", Runtime: rt}
	if err := forgetSession(cfg, "probe@x1", "brig rm probe@x1", true, nil); err != nil {
		t.Fatalf("rm --forget --dry-run: %v", err)
	}
	if got := wrap.WorkspaceOfSandbox("brig-probe-x1"); got != home || len(revoked) != 0 {
		t.Fatalf("--dry-run forgot %q or revoked %v", got, revoked)
	}
	if err := forgetSession(cfg, "probe@x1", "brig rm probe@x1", false, nil); err != nil {
		t.Fatalf("rm --forget: %v", err)
	}
	if got := wrap.WorkspaceOfSandbox("brig-probe-x1"); got != "" {
		t.Errorf("rm --forget kept the session: %q", got)
	}
	if len(revoked) != 1 || revoked[0] != "brig-probe-x1" {
		t.Errorf("rm --forget revoked %v, want the session's tokens", revoked)
	}
	if rt.removed || rt.stopped {
		t.Error("rm --forget asked a runtime to stop or remove a sandbox")
	}
	if _, err := os.Stat(filepath.Join(home, "work")); err != nil {
		t.Errorf("rm --forget deleted the guest home: %v", err)
	}
}

// Negative: --forget is refused on a session of the runtime brig drives. rm
// is the way to remove it, and forgetting it would leave its sandbox running
// with no session. And it goes with a ref: --all forgets no session.
func TestForgetIsOnlyForASessionOnAnotherRuntime(t *testing.T) {
	home := sessionOn(t, "hull")
	var revoked []string
	swap(t, &revokeRoutes, func(name string) error {
		revoked = append(revoked, name)
		return nil
	})
	rt := &otherKindRuntime{existsRuntime: &existsRuntime{listRuntime: absent()}, kind: "hull"}
	err := forgetSession(&wrap.Config{VMName: "brig-probe-x1", Runtime: rt}, "probe@x1", "brig rm probe@x1", false, nil)
	if exitCode(err) != exitUsage || !strings.Contains(err.Error(), "`brig rm probe@x1` removes it") {
		t.Errorf("rm --forget of hull's own session: %v (exit %d)", err, exitCode(err))
	}
	if got := wrap.WorkspaceOfSandbox("brig-probe-x1"); got != home || len(revoked) != 0 {
		t.Errorf("the refused --forget forgot %q or revoked %v", got, revoked)
	}
	if _, o, err := takeRemoveFlags([]string{"--forget", "probe@x1"}); err != nil || !o.forget {
		t.Errorf("--forget was not read: %v", err)
	}
	if err := run([]string{"rm", "--all", "--forget"}); exitCode(err) != exitUsage {
		t.Errorf("rm --all --forget: %v (exit %d), want a usage refusal", err, exitCode(err))
	}
}

// Negative: ls under one runtime keeps the session of a sandbox recorded on
// another, which it cannot list. It still prunes its own runtime's session
// whose sandbox is gone, and one with no runtime recorded, as before.
func TestListingUnderOneRuntimeKeepsAnothersSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_STATE_DIR", dir)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	body := `{` +
		`"probe@x1": {"home": "/ws/x1", "sandbox": "brig-probe-x1", "runtime": "nerdctl"},` +
		`"probe@x2": {"home": "/ws/x2", "sandbox": "brig-probe-x2", "runtime": "hull"},` +
		`"probe@x3": {"home": "/ws/x3", "sandbox": "brig-probe-x3"}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// hull lists nerdctl's container, if at all, by its ID and not its name.
	pruneSessionIndex("hull", []runtime.Instance{{Name: "4f0c2a9e", State: "running"}})
	if got := wrap.WorkspaceOfSandbox("brig-probe-x1"); got != "/ws/x1" {
		t.Errorf("ls on hull pruned the session nerdctl is running: %q", got)
	}
	for _, gone := range []string{"brig-probe-x2", "brig-probe-x3"} {
		if got := wrap.WorkspaceOfSandbox(gone); got != "" {
			t.Errorf("ls on hull kept %s, which it does not have: %q", gone, got)
		}
	}
}

// The stuck ref, end to end on the command line: a session nerdctl booted,
// under hull. brig rm <ref> is refused and names --forget, and removes
// nothing; brig rm <ref> --forget then forgets the session, and the ref is
// free again.
func TestTheCommandLineForgetsASessionOnAnotherRuntime(t *testing.T) {
	rt := &removeRuntime{}
	removeHost(t, rt)
	pathWith(t, "")
	swap(t, &revokeRoutes, func(string) error { return nil })
	index := filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json")
	body := `{"faker@x1":{"home":"/ws/x1","sandbox":"brig-faker-x1","runtime":"nerdctl"}}`
	if err := os.WriteFile(index, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() { _, err = run2(t, []string{"rm", "faker@x1"}) })
	var other *wrap.OtherRuntimeError
	if !errors.As(err, &other) || !strings.Contains(err.Error(), "brig rm --forget") {
		t.Fatalf("brig rm faker@x1 under hull: %v, want it refused as nerdctl's, naming --forget", err)
	}
	if got := wrap.WorkspaceOfSandbox("brig-faker-x1"); got != "/ws/x1" {
		t.Fatalf("the refused rm forgot the session: %q", got)
	}
	captureStderr(t, func() { _, err = run2(t, []string{"rm", "faker@x1", "--forget"}) })
	if err != nil {
		t.Fatalf("brig rm faker@x1 --forget: %v", err)
	}
	if got := wrap.WorkspaceOfSandbox("brig-faker-x1"); got != "" {
		t.Errorf("--forget kept the session: %q", got)
	}
	if len(rt.removed) != 0 {
		t.Errorf("--forget removed %v from hull", rt.removed)
	}
}

// recordOn files a session of a target in sessions.json as booted on kind,
// beside its project index entry, and takes its sandbox from rt.
func recordOn(t *testing.T, rt *projectRuntime, ps wrap.ProjectSession, root, target, kind string) {
	t.Helper()
	id := wrap.ProjectIdentity(root, target, "", "")
	body := `{"` + id.Key + `":{"home":"` + ps.Home + `","sandbox":"` + ps.Sandbox + `","ephemeral":true,"runtime":"` + kind + `"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(rt.have, ps.Sandbox)
}

// A session of a target on another runtime is refused by rm --target, and
// rm --target --forget forgets it: the project's entry and the session go,
// its tokens and approval are revoked, and its home stays. hull is asked to
// remove nothing.
func TestForgetATargetsSessionOnAnotherRuntime(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	recordOn(t, rt, dev, root, "dev", "nerdctl")
	var revoked []string
	swap(t, &revokeRoutes, func(name string) error {
		revoked = append(revoked, name)
		return nil
	})

	_, _, err := runQuiet(t, "rm", "--target", "dev")
	var other *wrap.OtherRuntimeError
	if !errors.As(err, &other) || other.Recorded != "nerdctl" {
		t.Fatalf("rm --target dev under hull: %v, want it refused as nerdctl's", err)
	}
	if _, ok := wrap.LookupProject(dev.Sandbox); !ok || len(revoked) != 0 {
		t.Fatalf("the refused rm forgot the session or revoked %v", revoked)
	}

	if _, _, err := runQuiet(t, "rm", "--target", "dev", "--forget"); err != nil {
		t.Fatalf("rm --target dev --forget: %v", err)
	}
	if _, ok := wrap.LookupProject(dev.Sandbox); ok {
		t.Error("--forget kept the project's entry")
	}
	if got := wrap.WorkspaceOfSandbox(dev.Sandbox); got != "" {
		t.Errorf("--forget kept the session: %q", got)
	}
	if len(revoked) != 1 || revoked[0] != dev.Sandbox {
		t.Errorf("--forget revoked %v, want the session's tokens", revoked)
	}
	if len(rt.removed) != 0 {
		t.Errorf("--forget removed %v from hull", rt.removed)
	}
	if !there(filepath.Join(dev.Home, "agent-state")) {
		t.Error("--forget deleted the guest home")
	}
}

// ls shows a session recorded on another runtime, which its own runtime
// cannot list, so a session whose runtime is gone can be found to forget:
// a profile's with its ref, a target's in its table, each "on nerdctl" in
// place of a state. -q leaves the profile's ref out, since no verb there but
// rm --forget takes it, and --json carries the recorded runtime.
func TestListingShowsTheSessionsOfAnotherRuntime(t *testing.T) {
	rt := &projectRuntime{have: map[string]string{}}
	root := projectsHost(t, rt)
	dev := seedProject(t, rt, root, "dev", "")
	recordOn(t, rt, dev, root, "dev", "nerdctl")
	index := filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json")
	blob, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	profile := `"faker@x1":{"home":"/ws/x1","sandbox":"brig-faker-x1","runtime":"nerdctl"},`
	if err := os.WriteFile(index, []byte("{"+profile+string(blob[1:])), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := sandboxRows(mustList(t, rt), rt)
	var table, refs, errw bytes.Buffer
	printListing(&table, &errw, rows)
	for _, want := range []string{"faker@x1", "brig-faker-x1", "on nerdctl", "/ws/x1", dev.Sandbox} {
		if !strings.Contains(table.String(), want) {
			t.Errorf("ls does not show %q:\n%s", want, table.String())
		}
	}
	if strings.Contains(table.String(), stateUnlisted) {
		t.Errorf("ls shows the target's session on nerdctl as unlisted:\n%s", table.String())
	}
	printRefs(&refs, rows)
	if strings.Contains(refs.String(), "faker@x1") {
		t.Errorf("ls -q printed a ref of another runtime: %q", refs.String())
	}
	data := sandboxListData(rows, rt)
	found := 0
	for _, s := range data.Sandboxes {
		if s.Sandbox == "brig-faker-x1" || s.Sandbox == dev.Sandbox {
			found++
			if s.RecordedRuntime != "nerdctl" || s.State != stateOtherRuntime {
				t.Errorf("--json: %+v", s)
			}
		}
	}
	if found != 2 {
		t.Errorf("--json lists %d of the two sessions on nerdctl", found)
	}
}
