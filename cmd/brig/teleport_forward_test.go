package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

const forwardedTxID = "fedcba9876543210fedcba9876543210"

// forwardHost is moveHost with the session recorded as running on me@box,
// in the store /st/b there, after move 0123...: forwardRemote records each
// line it is handed and answers code, and outputRemote answers the status
// that brig asks there afterwards with status, or with the probe for
// anything else. The calls are returned in order.
func forwardHost(t *testing.T, rt *moveRuntime, code int, status teleportData) (*[][]string, *[][]string) {
	t.Helper()
	moveHost(t, rt, noNetProbe())
	writeIndex(t, `{"claude-code": `+movedEntry+`}`)
	var forwarded, asked [][]string
	prevForward, prevOut := forwardRemote, outputRemote
	forwardRemote = func(argv, _ []string) (int, error) { forwarded = append(forwarded, argv); return code, nil }
	outputRemote = func(argv []string) ([]byte, error) {
		asked = append(asked, argv)
		var b bytes.Buffer
		if strings.Contains(argv[len(argv)-1], probeVerb) {
			_ = writeJSONDocument(&b, "TeleportProbe", noNetProbe())
		} else {
			_ = writeJSONDocument(&b, "Teleport", status)
		}
		return b.Bytes(), nil
	}
	t.Cleanup(func() { forwardRemote, outputRemote = prevForward, prevOut })
	return &forwarded, &asked
}

// --to, --prepare and --abort on a session that runs on another host are
// the brig there's to do: brig hands the line to it over ssh, with the
// store the session runs in and brig's -q, --verbose and --json, and asks
// this host's hull nothing. A move the brig there finished is then recorded
// here from that brig's own record: the session runs on the host it went
// to, in the store and under the move that brig names. --status stays here,
// and says the moves are driven from there.
func TestAMovedSessionsTeleportRunsOnItsHost(t *testing.T) {
	moved := teleportData{Ref: "claude-code", Sandbox: "brig-claude-code", State: "moved", Host: "me@far",
		RemoteStore: "/st/c", TxID: forwardedTxID}
	for _, c := range []struct {
		args   []string
		remote string
		record bool
	}{
		{[]string{"teleport", "claude", "--to", "me@far"}, "brig teleport claude-code --to me@far", true},
		{[]string{"-q", "teleport", "claude", "--to", "me@far", "--prepare", "--json"},
			"brig -q --json teleport claude-code --to me@far --prepare", false},
		{[]string{"--verbose", "teleport", "claude", "--abort"}, "brig --verbose teleport claude-code --abort", false},
	} {
		rt := &moveRuntime{running: false}
		forwarded, asked := forwardHost(t, rt, 0, moved)
		if _, err := captureStdout(t, func() error { return run(c.args) }); err != nil {
			t.Errorf("%q: %v", c.args, err)
		}
		want := []string{"ssh", "--", "me@box", "env BRIG_HULL_STORE_DIR=/st/b " + c.remote}
		if len(*forwarded) != 1 || strings.Join((*forwarded)[0], "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q was handed on as %q, want %q", c.args, *forwarded, want)
		}
		if len(rt.asked) != 0 || rt.removed != 0 {
			t.Errorf("%q asked this host's hull %v, removed %d", c.args, rt.asked, rt.removed)
		}
		e := entryOf(t)
		switch {
		case c.record:
			status := []string{"ssh", "-o", "BatchMode=yes", "--", "me@box",
				"env BRIG_HULL_STORE_DIR=/st/b brig teleport claude-code --status --json"}
			if len(*asked) != 1 || strings.Join((*asked)[0], "\x00") != strings.Join(status, "\x00") {
				t.Errorf("%q asked there %q, want %q", c.args, *asked, status)
			}
			if e["host"] != "me@far" || e["remoteStore"] != "/st/c" || e["txid"] != forwardedTxID || e["pendingHost"] != nil {
				t.Errorf("%q: the record after the move there: %v", c.args, e)
			}
		default:
			if len(*asked) != 0 || e["host"] != "me@box" || e["txid"] != "0123456789abcdef0123456789abcdef" {
				t.Errorf("%q: asked %q, and the record is %v", c.args, *asked, e)
			}
		}
	}

	forwarded, _ := forwardHost(t, &moveRuntime{}, 0, moved)
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	if err != nil || len(*forwarded) != 0 ||
		out != "claude-code runs on me@box (move 0123456789abcdef0123456789abcdef); its moves are driven from there\n" {
		t.Errorf("--status: %q, %v, handed on %q", out, err, *forwarded)
	}
	_, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) })
	if err == nil || !strings.Contains(err.Error(), "claude-code runs on me@box already") || len(*forwarded) != 0 {
		t.Errorf("a move to the host it runs on: %v, handed on %q", err, *forwarded)
	}
}

// A forwarded line that fails leaves this host's record as it was: the
// brig there said why, and its exit code is brig's. An ssh that fails
// (255) says the end of it is not known here and names --status there.
func TestAForwardedFailureLeavesTheRecordAsItWas(t *testing.T) {
	for _, c := range []struct {
		code, exit int
		says       string
	}{
		{exitCapability, exitCapability, ""},
		{exitFailure, exitFailure, ""},
		{sshFailed, exitFailure, "`ssh me@box brig teleport claude-code --status` says where it stands"},
	} {
		_, asked := forwardHost(t, &moveRuntime{}, c.code, teleportData{State: "moved", Host: "me@far", TxID: forwardedTxID})
		_, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@far"}) })
		if code := exitCode(err); code != c.exit {
			t.Errorf("the brig there exited %d, and brig exits %d (%v), want %d", c.code, code, err, c.exit)
		}
		if c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says)) {
			t.Errorf("exit %d: %v, want it to say %q", c.code, err, c.says)
		}
		if e := entryOf(t); e["host"] != "me@box" || e["txid"] != "0123456789abcdef0123456789abcdef" ||
			e["remoteStore"] != "/st/b" || len(*asked) != 0 {
			t.Errorf("exit %d: the record is %v, and brig asked there %q", c.code, e, *asked)
		}
	}
}

// The way back: a move or a prepare whose destination is this host first
// removes the fenced copy the move away left here, which the probe, the
// arrival and hull would each refuse, once hull's record here says that
// move committed away. The record of where the session runs stays for the
// move back to replace. After the move the arrival has filed the session
// here as running, and brig leaves that record as it is. A copy that is not
// proven stale is kept, and nothing is handed on.
func TestAMoveBackHereRemovesTheCopyTheMoveLeft(t *testing.T) {
	t.Setenv("USER", "me")
	name, err := os.Hostname()
	if err != nil || name == "" {
		t.Skip("this host has no name")
	}
	here := "me@" + name
	stale := runtime.Migration{State: runtime.MigrationMigrated, MoveTxID: "0123456789abcdef0123456789abcdef"}
	for _, prepare := range []bool{false, true} {
		rt := &moveRuntime{inspect: stale}
		forwarded, _ := forwardHost(t, rt, 0, teleportData{State: "moved", Host: here, TxID: forwardedTxID})
		prevForward := forwardRemote
		forwardRemote = func(argv, env []string) (int, error) {
			if rt.removed != 1 || entryOf(t)["host"] != "me@box" {
				t.Errorf("the copy here was not removed, or the record went, before the move back: %d, %v",
					rt.removed, entryOf(t))
			}
			if !prepare {
				// The arrival here, by the brig there over ssh.
				writeIndex(t, `{"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true}}`)
			}
			return prevForward(argv, env)
		}
		args := []string{"teleport", "claude", "--to", here}
		if prepare {
			args = append(args, "--prepare")
		}
		var err error
		stderr := captureStderr(t, func() { _, err = captureStdout(t, func() error { return run(args) }) })
		if err != nil || len(*forwarded) != 1 {
			t.Fatalf("%q: %v, handed on %q", args, err, *forwarded)
		}
		if !strings.Contains(stderr, "removed the fenced copy of claude-code that the move to me@box left here") {
			t.Errorf("%q did not say it removed the copy: %q", args, stderr)
		}
		e := entryOf(t)
		switch {
		case prepare:
			if e["host"] != "me@box" {
				t.Errorf("a prepare back here changed the record: %v", e)
			}
		case e["host"] != nil || e["txid"] != nil:
			t.Errorf("the record after the move back: %v", e)
		case !strings.Contains(stderr, "claude-code runs on this host again; `ssh me@box brig rm claude-code` removes the copy left there"):
			t.Errorf("the move back did not say where the session runs: %q", stderr)
		}
	}

	// A copy hull's record does not show as moved away, on a host whose
	// brig does not say the session runs there, may be the only one.
	rt := &moveRuntime{}
	forwarded, _ := forwardHost(t, rt, 0, teleportData{})
	_, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", here}) })
	if err == nil || !strings.Contains(err.Error(), "is kept") || rt.removed != 0 || len(*forwarded) != 0 {
		t.Errorf("an unproven copy: %v, removed %d, handed on %q", err, rt.removed, *forwarded)
	}
}

// writeArrival files a prepared arrival of move forwardedTxID from me@box
// here, as the brig on me@box files it over ssh at the start of a move back,
// with the session filed here as running and the record it replaced kept.
func writeArrival(t *testing.T) {
	t.Helper()
	writeIndex(t, `{"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true}}`)
	arrivals := `{"brig-claude-code": {"ref": "claude-code", "txid": "` + forwardedTxID + `", "from": "me@box", ` +
		`"network": "none", "slot": 0, "egress": {}, "state": "prepared", "replaced": ` + movedEntry + `}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "arrivals.json"), []byte(arrivals), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A move back here whose ssh drops after the brig there began it leaves an
// arrival filed here, which the source has to settle: the arrival rewrote
// this host's record as a session of its own. brig says that, not that the
// record still names the other host, and says the fenced copy is gone.
// Afterwards --status here says the move is arriving and where it is driven
// from, and --abort and --to are refused with the line that settles it,
// rather than taken for a session that runs here.
func TestAMoveBackLeftArrivingHereIsNotTakenForOneOfThisHosts(t *testing.T) {
	t.Setenv("USER", "me")
	name, err := os.Hostname()
	if err != nil || name == "" {
		t.Skip("this host has no name")
	}
	here := "me@" + name
	rt := &moveRuntime{inspect: runtime.Migration{State: runtime.MigrationMigrated,
		MoveTxID: "0123456789abcdef0123456789abcdef"}}
	forwarded, _ := forwardHost(t, rt, sshFailed, teleportData{})
	prevForward := forwardRemote
	forwardRemote = func(argv, env []string) (int, error) {
		writeArrival(t)
		return prevForward(argv, env)
	}
	_, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", here}) })
	if code := exitCode(err); code != exitFailure || len(*forwarded) != 1 {
		t.Fatalf("a dropped move back: exit %d (%v), handed on %q", code, err, *forwarded)
	}
	for _, want := range []string{
		"`ssh me@box brig teleport claude-code --status` says where it stands",
		"This host records claude-code as arriving from me@box (move " + forwardedTxID + "), and the move has not finished",
		"The fenced copy the move away left here was removed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a dropped move back said %q, want %q in it", err, want)
		}
	}
	if strings.Contains(err.Error(), "still says") {
		t.Errorf("a dropped move back said the record still names the other host: %v", err)
	}
	forwardRemote = prevForward

	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	if err != nil || strings.Contains(out, "runs on this host") ||
		!strings.Contains(out, "claude-code is arriving on this host from me@box (move "+forwardedTxID+")") ||
		!strings.Contains(out, "`ssh me@box brig teleport claude-code --status` settles it") {
		t.Errorf("--status of an arrival not finished: %q, %v", out, err)
	}
	out, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status", "--json"}) })
	var doc struct {
		Data teleportData `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil || doc.Data.State != "arriving" ||
		doc.Data.From != "me@box" || doc.Data.TxID != forwardedTxID || doc.Data.Host != "" {
		t.Errorf("--status --json of an arrival not finished: %q, %v", out, err)
	}
	for _, args := range [][]string{
		{"teleport", "claude", "--abort"},
		{"teleport", "claude", "--to", "me@far"},
		{"teleport", "claude", "--to", "me@far", "--prepare"},
	} {
		_, err := captureStdout(t, func() error { return run(args) })
		if code := exitCode(err); code != exitFailure || !strings.Contains(err.Error(), "is not moved or called off from here") ||
			!strings.Contains(err.Error(), "`ssh me@box brig teleport claude-code --abort` calls it off") {
			t.Errorf("%q of an arrival not finished: exit %d, %v", args, code, err)
		}
	}
	if len(*forwarded) != 1 || len(rt.asked) != 0 {
		t.Errorf("an arrival not finished was handed on %q, or this host's hull asked %v", *forwarded, rt.asked)
	}
}

// A move the brig there says came here, with no arrival of it filed here,
// as when that brig reached this host under another BRIG_STATE_DIR, is not
// recorded as running on this host: a record that names this host would
// send every verb here over ssh to itself. brig says so and leaves the
// record as it was.
func TestAMoveSaidToComeHereWithNoArrivalIsNotRecordedAsElsewhere(t *testing.T) {
	t.Setenv("USER", "me")
	name, err := os.Hostname()
	if err != nil || name == "" {
		t.Skip("this host has no name")
	}
	here := "me@" + name
	rt := &moveRuntime{inspect: runtime.Migration{State: runtime.MigrationMigrated,
		MoveTxID: "0123456789abcdef0123456789abcdef"}}
	forwarded, _ := forwardHost(t, rt, 0, teleportData{State: "moved", Host: here, TxID: forwardedTxID})
	stderr := captureStderr(t, func() {
		_, err = captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", here}) })
	})
	if err != nil || len(*forwarded) != 1 {
		t.Fatalf("%v, handed on %q", err, *forwarded)
	}
	if !strings.Contains(stderr, "says claude-code moved to this host, and no arrival of it is filed here") {
		t.Errorf("a move said to come here with no arrival: %q", stderr)
	}
	if e := entryOf(t); e["host"] != "me@box" || e["txid"] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("the record after a move said to come here with no arrival: %v", e)
	}
}
