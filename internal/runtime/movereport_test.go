package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// A report reads the parts of hull's lines it knows, each on its own: a
// round's number, its bytes, time, rate and dirty rate, the downtime, and
// the whole copy's rate from hull's Sent line. hull's rounded sizes are not
// turned into byte counts, and a line with none of the parts changes
// nothing, so the JSON leaves out what hull did not say.
func TestMoveReportReadsHullsLines(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Move " + tx1 + " of s to me@box, epoch 1",
		"Reserved s on me@box",
		"round 0: 1048576 bytes in 0.5 s (2.0 MiB/s), dirty 120 pages/s",
		"Round 1: 65536 bytes in 0.1 s (512.0 KiB/s), dirty 40.5 pages/s",
		"round 2: 8.0 MiB in 0.2 s",
		"Sent 1.7 GiB in 46.3 s (37.1 MiB/s); disk 1.6 GiB of 16.0 GiB carried; destination: verified",
		"Resumed s on me@box; downtime 1450 ms",
		"Timings (ms): fence 1326, transfer 4765, restore paused 861, commit 489, verify 12",
	} {
		r.Add(line)
	}
	want := MoveReport{Rounds: []MoveRound{
		{Round: 0, Bytes: 1048576, Seconds: 0.5, MiBPerSec: 2, DirtyPagesPerSec: 120},
		{Round: 1, Bytes: 65536, Seconds: 0.1, MiBPerSec: 0.5, DirtyPagesPerSec: 40.5},
		{Round: 2, Seconds: 0.2},
	}, MiBPerSec: 37.1, DowntimeSeconds: 1.45,
		StepsMs: map[string]int64{"fence": 1326, "transfer": 4765, "restore paused": 861, "commit": 489, "verify": 12}}
	got, _ := json.Marshal(r)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Errorf("got  %s\nwant %s", got, wantJSON)
	}
	var empty MoveReport
	empty.Add("Sending s to me@box")
	if b, _ := json.Marshal(empty); string(b) != "{}" {
		t.Errorf("a line with nothing to read gave %s", b)
	}
	r.Seconds = 7.25
	if s := r.Summary(); s != "hull's move took 7.2 s, the guest was paused 1.45 s, at 37.1 MiB/s, 3 rounds; "+
		"fence 1.33 s, transfer 4.76 s, restore paused 0.86 s, commit 0.49 s, verify 0.01 s" {
		t.Errorf("summary %q", s)
	}
}

// A fake hull printing its lines while it moves: they reach a MoveReport
// through the move's Progress, as brig teleport wires them.
func TestAMoveReportIsFedAsTheMoveRuns(t *testing.T) {
	h, _ := migrateHull(t, "printf 'round 0: 4096 bytes in 0.1 s (0.1 MiB/s), dirty 9 pages/s\\n"+
		"Sent 1048576 bytes in 0.2 s (5.0 MiB/s)\\ndowntime: 0.8 s\\n' >&2; echo committed > RECORD")
	var r MoveReport
	if _, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1, Progress: r.Add}); err != nil {
		t.Fatal(err)
	}
	if len(r.Rounds) != 1 || r.Rounds[0].Bytes != 4096 || r.Bytes != 1048576 || r.MiBPerSec != 5 ||
		r.DowntimeSeconds != 0.8 {
		t.Errorf("report %+v", r)
	}
	if !strings.Contains(r.Summary(), "1048576 bytes sent") {
		t.Errorf("summary %q", r.Summary())
	}
}
