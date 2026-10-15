package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

const preparedBase = "e85e4338aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// --prepare asks hull to make the host ready, with the store and the hull
// the destination's brig drives, and moves nothing: no dry run, no move, no
// network taken there, no move recorded. It says what the host now holds,
// and the index notes the host. A second prepare asks hull again: hull's
// prepare is idempotent, and brig does not guess what is still there.
func TestTeleportPrepareReadiesTheHostAndMovesNothing(t *testing.T) {
	rt := &moveRuntime{running: true, prepared: preparedReport(true),
		progress: []string{"Preparing me@box for brig-claude-code", "base e85e4338aaaa: sent (1.6 GiB of data)"}}
	probe := noNetProbe()
	probe.Store.Named = "/st/b"
	calls := moveHost(t, rt, probe)
	var out string
	stderr := captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, func() error {
			return run([]string{"teleport", "claude", "--to", "me@box", "--prepare"})
		})
		if err != nil {
			t.Errorf("prepare: %v", err)
		}
	})
	if strings.Join(rt.asked, ",") != "prepare me@box" {
		t.Errorf("hull was asked %v", rt.asked)
	}
	if s := rt.prepares[0]; s.RemoteStore != "/st/b" || s.RemoteHull != "/opt/hull" {
		t.Errorf("the prepare was asked as %+v", s)
	}
	for _, argv := range *calls {
		if !strings.Contains(argv[len(argv)-1], probeVerb) {
			t.Errorf("the destination was asked more than the probe: %q", argv)
		}
	}
	for _, w := range []string{"prepared me@box for claude-code\n", "  image 1a2b3c4d5e6f: held\n",
		"  portable-arm64-v1 kernel: sent (e85e4338)\n", "  base e85e4338aaaa: sent (1.6 GiB of data as 0.9 GiB)\n",
		"  base digest: " + preparedBase + "\n", "  gateway: brig starts it there at the move"} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout %q does not say %q", out, w)
		}
	}
	// hull's lines say what brig says after them, so they are for
	// --verbose.
	if strings.Contains(stderr, "hull: ") || strings.Contains(stderr, "Preparing") {
		t.Errorf("a prepare passed hull's lines on by default: %q", stderr)
	}
	e := entryOf(t)
	if e["preparedHost"] != "me@box" || e["preparedBase"] != preparedBase || e["preparedStream"] != true || e["preparedAt"] == nil ||
		e["pendingHost"] != nil || e["host"] != nil || e["txid"] != nil {
		t.Errorf("the record after a prepare: %v", e)
	}
	stderr = captureStderr(t, func() {
		var err error
		out, err = captureStdout(t, func() error {
			return run([]string{"--verbose", "teleport", "claude", "--to", "me@box", "--prepare", "--json"})
		})
		if err != nil {
			t.Errorf("prepare --json: %v", err)
		}
	})
	if !strings.Contains(stderr, "brig: hull: Preparing me@box for brig-claude-code\n") ||
		!strings.Contains(stderr, "brig: hull: base e85e4338aaaa: sent") {
		t.Errorf("--verbose did not pass hull's lines on: %q", stderr)
	}
	var doc struct {
		Kind string       `json:"kind"`
		Data teleportData `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Kind != "Teleport" {
		t.Fatalf("--json: %q, %v", out, err)
	}
	if p := doc.Data.Prepared; p == nil || p.Host != "me@box" || p.Base != preparedBase || !p.Stream ||
		len(p.Steps) != 3 || p.Kernels["portable-arm64-v1"] == "" || doc.Data.State != "here" || doc.Data.TxID != "" {
		t.Errorf("--json says %+v", doc.Data)
	}
	if strings.Join(rt.asked, ",") != "prepare me@box,prepare me@box" {
		t.Errorf("a second prepare did not ask hull: %v", rt.asked)
	}
	// --status names the prepared host, and a move to it says so.
	out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status"}) })
	if err != nil || !strings.Contains(out, "me@box was prepared for it at ") {
		t.Errorf("--status after a prepare: %q, %v", out, err)
	}
	rt.state = runtime.MoveCommitted
	stderr = captureStderr(t, func() {
		if _, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) }); err != nil {
			t.Errorf("move: %v", err)
		}
	})
	if !strings.Contains(stderr, "me@box was prepared for claude-code at ") ||
		!strings.Contains(stderr, ", when hull said a move there would stream\n") {
		t.Errorf("the move did not say the host was prepared: %q", stderr)
	}
	if e := entryOf(t); e["host"] != "me@box" || e["preparedHost"] != nil {
		t.Errorf("the record after the move: %v", e)
	}
}

// A prepare runs the checks a move runs and is refused the same way, before
// hull copies anything: a host that could not take the session is not sent
// its base. A prepare hull fails records nothing. A session that runs on
// another host, or whose move has not finished, is prepared from nowhere
// here.
func TestATeleportPrepareIsRefusedLikeAMove(t *testing.T) {
	for _, c := range []struct {
		what  string
		rt    *moveRuntime
		probe func(*probeReport)
		index string
		want  string
	}{
		{"not teleportable", &moveRuntime{running: true, notTeleportable: true}, nil, "", "block root"},
		{"no such agent there", &moveRuntime{running: true}, func(r *probeReport) { r.Agent.Known = false }, "", "no agent"},
		{"the name taken there", &moveRuntime{running: true}, func(r *probeReport) { r.Sandbox.Exists = true }, "", "already has a sandbox"},
		{"hull fails", &moveRuntime{running: true, prepareErr: errors.New("ssh: connection reset")}, nil, "", "connection reset"},
		{"moved", &moveRuntime{running: true}, nil,
			`{"claude-code": {"home": "", "sandbox": "brig-claude-code", "teleportable": true, "host": "you@pi", "txid": "t"}}`,
			"runs on you@pi"},
	} {
		probe := noNetProbe()
		if c.probe != nil {
			c.probe(&probe)
		}
		moveHost(t, c.rt, probe)
		if c.index != "" {
			writeIndex(t, c.index)
		}
		_, err := captureStdout(t, func() error {
			return run([]string{"teleport", "claude", "--to", "me@box", "--prepare"})
		})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.what, err, c.want)
		}
		wantAsked := ""
		if c.what == "hull fails" {
			wantAsked = "prepare me@box"
		}
		if strings.Join(c.rt.asked, ",") != wantAsked {
			t.Errorf("%s: hull was asked %v", c.what, c.rt.asked)
		}
		if e := entryOf(t); e["preparedHost"] != nil {
			t.Errorf("%s: a refused prepare was recorded: %v", c.what, e)
		}
	}
}

// A host hull could not make ready for a streamed move, an older hull or hvi
// there, is recorded as holding the base, with stream false, so --status and
// the move do not claim a move there streams. brig says why, and the prepare
// itself did not fail. A host that holds no base at all is not recorded.
func TestAPrepareThatCannotStreamSaysSo(t *testing.T) {
	for _, c := range []struct {
		what     string
		noBase   bool
		wantOut  string
		recorded bool
	}{
		{"the base held", false, "prepared me@box for claude-code; a move there still sends the whole checkpoint and disk\n", true},
		{"no base", true, "me@box is not prepared for claude-code\n", false},
	} {
		p := preparedReport(false)
		if c.noBase {
			p.Base = ""
		}
		rt := &moveRuntime{running: true, prepared: p}
		moveHost(t, rt, noNetProbe())
		var out string
		stderr := captureStderr(t, func() {
			var err error
			out, err = captureStdout(t, func() error {
				return run([]string{"teleport", "claude", "--to", "me@box", "--prepare"})
			})
			if err != nil {
				t.Errorf("%s: prepare: %v", c.what, err)
			}
		})
		if !strings.HasPrefix(out, c.wantOut) ||
			!strings.Contains(stderr, "still sends the whole checkpoint and disk: its hvi (0.1) cannot restore from a stream") {
			t.Errorf("%s: stdout %q, stderr %q", c.what, out, stderr)
		}
		e := entryOf(t)
		if (e["preparedHost"] != nil) != c.recorded || e["preparedStream"] != nil {
			t.Errorf("%s: the record %v", c.what, e)
		}
		if !c.recorded {
			continue
		}
		out, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--status", "--json"}) })
		if err != nil || !strings.Contains(out, `"stream": false`) || !strings.Contains(out, `"base": "`+preparedBase+`"`) {
			t.Errorf("%s: --status %q, %v", c.what, out, err)
		}
		rt.state = runtime.MoveCommitted
		stderr = captureStderr(t, func() {
			if _, err := captureStdout(t, func() error { return run([]string{"teleport", "claude", "--to", "me@box"}) }); err != nil {
				t.Errorf("move: %v", err)
			}
		})
		if !strings.Contains(stderr, ", when hull said a move there would still send the whole checkpoint and disk\n") {
			t.Errorf("%s: the move's note: %q", c.what, stderr)
		}
	}
}

// preparedReport is hull's report of a prepare of claude's sandbox, one that
// a move would stream to or not.
func preparedReport(stream bool) runtime.Prepared {
	p := runtime.Prepared{Stream: stream, Steps: []runtime.PrepareStep{
		{What: "image 1a2b3c4d5e6f", Result: "held"},
		{What: "portable-arm64-v1 kernel", Result: "sent", Detail: "e85e4338"},
		{What: "base e85e4338aaaa", Result: "sent", Detail: "1.6 GiB of data as 0.9 GiB", Bytes: 966367641}},
		Base: preparedBase, Kernels: map[string]string{"portable-arm64-v1": "e85e4338"}}
	if !stream {
		p.Why = "its hvi (0.1) cannot restore from a stream"
	}
	return p
}

// --prepare names the host it makes ready, and asks nothing else.
func TestTeleportPrepareNeedsAHostAndNothingElse(t *testing.T) {
	for _, o := range []teleportOptions{
		{prepare: true},
		{prepare: true, status: true},
		{prepare: true, abort: true},
		{prepare: true, to: "me@box", status: true},
	} {
		err := teleportCmd(nil, profile.Profile{}, "claude-code", o, false)
		if code := exitCode(err); code != exitUsage {
			t.Errorf("%+v exits %d, want %d: %v", o, code, exitUsage, err)
		}
	}
	o, _, _, err := parse("teleport", []string{"claude", "--prepare", "--to", "me@box"})
	if err != nil || !o.teleport.prepare || o.teleport.to != "me@box" {
		t.Errorf("teleport line: %+v %v", o.teleport, err)
	}
	o, _, tail, err := parse("run", []string{"codex", "--prepare"})
	if err != nil || o.teleport.prepare || strings.Join(tail, " ") != "--prepare" {
		t.Errorf("the run line took the agent's --prepare: %+v, tail %q, %v", o.teleport, tail, err)
	}
}

// writeIndex replaces the session index moveHost wrote.
func writeIndex(t *testing.T, index string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_STATE_DIR"), "sessions.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A move passes hull's lines on to stderr as they arrive: by default one
// line a round, the pause, the seal, the restore and the resume, and under
// --verbose every line whole. Its outcome carries how it went either way:
// the text ends with the numbers, and --json has them in its move object,
// rounds included. stdout stays the one document.
func TestAMovePassesHullsLinesOnAndReportsItsNumbers(t *testing.T) {
	// hull 7b3f7b1's lines for a streamed move of the ubuntu guest, as the
	// recording of 2026-10-15 shows them.
	lines := []string{"Move 08496b841d5cbbd64659262a4a351500 of brig-claude-code to me@box, epoch 1",
		"Transport: streamed (pre-copy rounds; the disk blocks that differ from base 316f7a8c31d4)",
		"Reserved brig-claude-code on me@box",
		"Streaming brig-claude-code to me@box (live)",
		"move: disk-round-0: 2.5 MiB in 0.11 s, 22.5 MiB/s",
		"move: ram-round-0: 40.6 MiB in 1.00 s, 40.7 MiB/s",
		"move: disk-round-final: 0 bytes in 0.01 s, 0 bytes/s",
		"move: ram-round-1: 1.3 MiB in 0.01 s, 109.2 MiB/s",
		"move: hvi: 2 RAM rounds (converged), disk rounds of 3876, 0 blocks (242.2 MiB, 2.5 MiB on the wire); " +
			"paused 24 ms, disk sync 1 ms before the pause and 0 ms in it",
		"move: hvi sealed the stream and fenced brig-claude-code (19 ms after the pause)",
		"Sent 44.4 MiB in 1.2 s (36.7 MiB/s): disk 242.2 MiB as 2.5 MiB, RAM 41.9 MiB, state 54052 bytes; " +
			"destination: stream: 51 hvi components checked; hvi checked them against its manifest, wrote the " +
			"disk rounds onto base 316f7a8c31d4 and restored the guest paused",
		"Moved brig-claude-code to me@box: it runs there at epoch 1. Its checkpoint here is stale",
		"Timings (ms): fence 0, transfer 1208, restore paused 0, commit 13",
		"move: 44.4 MiB in 1.2 s; disk 2.5 MiB, RAM 41.9 MiB, state 54052 bytes; sealed 19 ms after the pause, " +
			"last disk round 0 bytes in 0 ms; downtime about 60 ms",
		"move: from the seal, ms (there: the destination's clock): trailer sent 0, instance's fence recorded 5, " +
			"there resumed 35, commit answered 44"}
	quiet := []string{"disk-round-0 2.5 MiB in 0.11 s", "ram-round-0 40.6 MiB in 1.00 s",
		"disk-round-final 0 bytes in 0.01 s", "ram-round-1 1.3 MiB in 0.01 s", "paused after 2 RAM rounds (converged)",
		"sealed 19 ms after the pause", "sent 44.4 MiB in 1.2 s; restored paused there", "resumed on me@box (epoch 1)"}
	for _, c := range []struct {
		wantJSON, verbose bool
	}{{false, false}, {true, false}, {false, true}} {
		rt := &moveRuntime{running: true, state: runtime.MoveCommitted, progress: lines}
		moveHost(t, rt, noNetProbe())
		args := []string{"teleport", "claude", "--to", "me@box"}
		if c.wantJSON {
			args = append(args, "--json")
		}
		if c.verbose {
			args = append([]string{"--verbose"}, args...)
		}
		var out string
		stderr := captureStderr(t, func() {
			var err error
			if out, err = captureStdout(t, func() error { return run(args) }); err != nil {
				t.Errorf("move: %v", err)
			}
		})
		if c.verbose {
			for _, l := range lines {
				if !strings.Contains(stderr, "brig: hull: "+l+"\n") {
					t.Errorf("--verbose does not pass on %q: %q", l, stderr)
				}
			}
			if !strings.Contains(stderr, "brig rm claude-code removes the fenced copy left here") {
				t.Errorf("--verbose does not say what runs there now: %q", stderr)
			}
		} else {
			var said []string
			for _, l := range strings.Split(strings.TrimSpace(stderr), "\n") {
				if s, ok := strings.CutPrefix(l, "brig: "); ok && !strings.HasPrefix(s, "moving ") {
					said = append(said, s)
				}
			}
			if strings.Join(said, "|") != strings.Join(quiet, "|") {
				t.Errorf("stderr by default:\n%s\nwant the lines\n%s", stderr, strings.Join(quiet, "\n"))
			}
		}
		if !c.wantJSON {
			if !strings.Contains(out, "\n  hull's move took ") ||
				!strings.Contains(out, "hull estimates the guest was paused about 0.06 s, at 36.7 MiB/s, 4 rounds (2 disk, 2 memory)") ||
				strings.Count(out, "\n") != 2 {
				t.Errorf("stdout %q", out)
			}
			continue
		}
		var doc struct {
			Data teleportData `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--json is not one document: %q, %v", out, err)
		}
		m := doc.Data.Move
		if m == nil || len(m.Rounds) != 4 || !m.Rounds[3].Final || m.DowntimeSeconds != 0.06 ||
			m.StepsMs["transfer"] != 1208 || doc.Data.State != "moved" {
			t.Errorf("--json says %+v, move %+v", doc.Data, m)
		}
	}
}

// A prepare is a record of what hull said then. When hull's Transport line
// says this move sends the whole checkpoint and disk to a host the prepare
// said would stream, the move says so, and --json carries hull's word and
// its reason, so a -q --json caller sees it too. hull's last Transport line
// is the one that counts: it switches after a base it sends first fails.
func TestAMoveSaysWhenHullDoesNotStreamToAPreparedHost(t *testing.T) {
	rt := &moveRuntime{running: true, prepared: preparedReport(true)}
	moveHost(t, rt, noNetProbe())
	if _, err := captureStdout(t, func() error {
		return run([]string{"teleport", "claude", "--to", "me@box", "--prepare"})
	}); err != nil {
		t.Fatal(err)
	}
	rt.state = runtime.MoveCommitted
	rt.progress = []string{"Transport: streamed (pre-copy rounds; the disk blocks that differ from base e85e4338aaaa)",
		"Sending base e85e4338aaaa to me@box (once per base; `hull migrate-prepare` does it ahead of a move)",
		"Transport: the whole checkpoint and disk, because sending the base failed: no space left on device"}
	var out string
	stderr := captureStderr(t, func() {
		var err error
		if out, err = captureStdout(t, func() error {
			return run([]string{"teleport", "claude", "--to", "me@box", "--json"})
		}); err != nil {
			t.Errorf("move: %v", err)
		}
	})
	if !strings.Contains(stderr, "brig: hull sends the whole checkpoint and disk of claude-code, with the guest "+
		"paused, though the prepare at ") ||
		!strings.Contains(stderr, "said a move to me@box would stream: sending the base failed: no space left on device") {
		t.Errorf("the move did not say hull stopped streaming: %q", stderr)
	}
	if strings.Count(stderr, "the prepare at ") != 1 {
		t.Errorf("the move said it more than once: %q", stderr)
	}
	var doc struct {
		Data teleportData `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json: %q, %v", out, err)
	}
	if m := doc.Data.Move; m == nil || m.Transport != "bundle" ||
		m.TransportWhy != "sending the base failed: no space left on device" {
		t.Errorf("--json move %+v", doc.Data.Move)
	}
}
