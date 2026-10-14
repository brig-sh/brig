package wrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/profile"
)

// writeEntries replaces the session index with these entries, as an older or
// a newer brig would have left it.
func writeEntries(t *testing.T, dir string, entries map[string]map[string]any) {
	t.Helper()
	blob, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionIndexName), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An entry for a session that was moved to another host is the only way to
// reach the session from here. Removing the session's old copy from this
// host, and a listing of this host that no longer has it, must not drop it.
// An entry with no host is pruned and forgotten as it always was.
func TestAMovedSessionsEntryOutlivesItsLocalSandbox(t *testing.T) {
	dir := isolateState(t)
	writeEntries(t, dir, map[string]map[string]any{
		"claude-code":     {"home": "", "sandbox": "brig-claude-code", "teleportable": true, "host": "me@box", "txid": "tx1"},
		"claude-code@mid": {"home": "", "sandbox": "brig-claude-code-mid", "teleportable": true, "pendingHost": "me@box", "txid": "tx2"},
		"claude-code@old": {"home": "", "sandbox": "brig-claude-code-old", "teleportable": true},
	})
	PruneSessions([]string{"some-other-container"})
	if dropped, err := ForgetSandbox("brig-claude-code"); dropped || err != nil {
		t.Errorf("ForgetSandbox dropped a moved session's entry: %v, %v", dropped, err)
	}
	if dropped, _ := ForgetSandbox("brig-claude-code-mid"); dropped {
		t.Errorf("ForgetSandbox dropped the entry of a move under way")
	}
	index := readSessionIndex()
	if index["claude-code"].Host != "me@box" || index["claude-code@mid"].PendingHost != "me@box" {
		t.Errorf("a moved session's entry did not survive: %+v", index)
	}
	if _, ok := index["claude-code@old"]; ok {
		t.Errorf("an ordinary entry whose sandbox is gone was kept")
	}
	got := RemoteSessions()
	if len(got) != 2 || got[0].Sandbox != "brig-claude-code" || got[1].PendingHost != "me@box" {
		t.Errorf("RemoteSessions = %+v", got)
	}
}

// SessionLocation finds a moved session by the same key and sandbox name a
// load would, without loading it, so a verb on it can go to the other host
// before anything here asks this host's runtime or opens its store.
func TestSessionLocationFindsAMovedSessionWithoutLoadingIt(t *testing.T) {
	dir := isolateState(t)
	writeEntries(t, dir, map[string]map[string]any{
		"claude-code@x": {"home": "", "sandbox": "brig-claude-code-x", "teleportable": true,
			"host": "me@box", "remoteStore": "/st/b", "txid": "tx1"},
	})
	p, _ := profile.Lookup("claude")
	loc, ok, err := SessionLocation(p, "x")
	if err != nil || !ok {
		t.Fatalf("got %+v, %v, %v", loc, ok, err)
	}
	want := Location{Ref: "claude-code@x", Sandbox: "brig-claude-code-x", Host: "me@box", RemoteStore: "/st/b", TxID: "tx1"}
	if loc != want {
		t.Errorf("got %+v, want %+v", loc, want)
	}
	// BRIG_NAME renames the sandbox, and an entry for another sandbox is not
	// this one's.
	t.Setenv("BRIG_NAME", "brig-renamed")
	if _, ok, _ := SessionLocation(p, "x"); ok {
		t.Errorf("a renamed sandbox found the entry of another")
	}
}

// A move is recorded in three steps against the entry it began from: begun
// under a txid, then committed or cleared under that same txid. A step that
// names another move, or a session already moved, is refused.
func TestAMoveIsRecordedAgainstTheEntryItBeganFrom(t *testing.T) {
	dir := isolateState(t)
	writeEntries(t, dir, map[string]map[string]any{
		"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true},
	})
	if err := RecordMoveStarted("claude-code", "brig-other", "me@box", "", "tx1"); err == nil {
		t.Errorf("a move was recorded against another sandbox")
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "me@box", "", "tx1"); err != nil {
		t.Fatal(err)
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "me@box", "", "tx2"); err == nil {
		t.Errorf("a second move began while the first was unfinished")
	}
	if err := RecordMoved("claude-code", "brig-claude-code", "me@box", "/st", "tx2"); err == nil {
		t.Errorf("a move was committed under another txid")
	}
	if err := ClearMove("claude-code", "brig-claude-code", "tx1"); err != nil {
		t.Fatal(err)
	}
	if e := readSessionIndex()["claude-code"]; e.away() || e.TxID != "" {
		t.Errorf("a cleared move left %+v", e)
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "me@box", "", "tx3"); err != nil {
		t.Fatal(err)
	}
	if err := RecordMoved("claude-code", "brig-claude-code", "me@box", "/st", "tx3"); err != nil {
		t.Fatal(err)
	}
	e := readSessionIndex()["claude-code"]
	if e.Host != "me@box" || e.PendingHost != "" || e.RemoteStore != "/st" || !e.isTeleportable() {
		t.Errorf("a committed move left %+v", e)
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "you@other", "", "tx4"); err == nil ||
		!strings.Contains(err.Error(), "already runs on me@box") {
		t.Errorf("a moved session began a second move: %v", err)
	}
	if err := ForgetRemoteSession("claude-code", "brig-claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSessionIndex()["claude-code"]; ok {
		t.Errorf("ForgetRemoteSession left the entry")
	}
}

// A run that records the session never clears the host a move recorded.
func TestRememberingASessionKeepsTheHostAMoveRecorded(t *testing.T) {
	dir := isolateState(t)
	writeEntries(t, dir, map[string]map[string]any{
		"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true, "host": "me@box", "txid": "tx1"},
	})
	c := mustLoadAs(t, "claude", Options{})
	c.rememberSession()
	if e := readSessionIndex()["claude-code"]; e.Host != "me@box" || e.TxID != "tx1" {
		t.Errorf("rememberSession dropped the move: %+v", e)
	}
}

// A prepare is recorded against the session's own entry, and names the host
// it made ready and no other: preparing one host and then asking about the
// session finds that host, a second prepare replaces the first, another
// session's entry is untouched, and a run that records the session again
// keeps it. A committed move drops it, since the next move starts from the
// host the session went to; an aborted one keeps it, since the base is still
// there. A session that is not here is refused.
func TestAPrepareRecordsTheHostItMadeReady(t *testing.T) {
	dir := isolateState(t)
	writeEntries(t, dir, map[string]map[string]any{
		"claude-code":   {"home": "", "sandbox": "brig-claude-code", "teleportable": true},
		"claude-code@b": {"home": "", "sandbox": "brig-claude-code-b", "teleportable": true},
		"claude-code@m": {"home": "", "sandbox": "brig-claude-code-m", "teleportable": true, "host": "me@box", "txid": "tx0"},
	})
	at := time.Date(2026, 10, 15, 1, 30, 0, 0, time.FixedZone("EEST", 3*3600))
	if err := RecordPrepared("claude-code", "brig-other", "me@box", "ab12", at); err == nil {
		t.Errorf("a prepare was recorded against another sandbox")
	}
	if err := RecordPrepared("claude-code@m", "brig-claude-code-m", "you@other", "ab12", at); err == nil {
		t.Errorf("a prepare was recorded for a session that runs on another host")
	}
	if err := RecordPrepared("claude-code", "brig-claude-code", "me@box", "ab12", at); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrepared("claude-code", "brig-claude-code", "me@pi", "cd34", at); err != nil {
		t.Fatal(err)
	}
	p, _ := profile.Lookup("claude")
	loc, ok, err := SessionLocation(p, "")
	if err != nil || !ok || loc.PreparedHost != "me@pi" || loc.PreparedBase != "cd34" ||
		loc.PreparedAt != "2026-10-14T22:30:00Z" || loc.Away() {
		t.Errorf("after two prepares: %+v, %v, %v", loc, ok, err)
	}
	if b, _, _ := SessionLocation(p, "b"); b.PreparedHost != "" {
		t.Errorf("preparing claude-code claimed claude-code@b: %+v", b)
	}
	c := mustLoadAs(t, "claude", Options{})
	c.rememberSession()
	if e := readSessionIndex()["claude-code"]; e.PreparedHost != "me@pi" || e.PreparedBase != "cd34" {
		t.Errorf("rememberSession dropped the prepared host: %+v", e)
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "me@pi", "", "tx1"); err != nil {
		t.Fatal(err)
	}
	if err := ClearMove("claude-code", "brig-claude-code", "tx1"); err != nil {
		t.Fatal(err)
	}
	if e := readSessionIndex()["claude-code"]; e.PreparedHost != "me@pi" {
		t.Errorf("a move that never left dropped the prepared host: %+v", e)
	}
	if err := RecordMoveStarted("claude-code", "brig-claude-code", "me@pi", "/st", "tx2"); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrepared("claude-code", "brig-claude-code", "me@pi", "ef56", at); err == nil {
		t.Errorf("a prepare was recorded while a move was under way")
	}
	if err := RecordMoved("claude-code", "brig-claude-code", "me@pi", "/st", "tx2"); err != nil {
		t.Fatal(err)
	}
	if e := readSessionIndex()["claude-code"]; e.PreparedHost != "" || e.PreparedBase != "" || e.PreparedAt != "" {
		t.Errorf("a committed move kept the prepared host: %+v", e)
	}
}
