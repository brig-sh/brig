package runtime

import (
	"encoding/json"
	"fmt"
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
		"round 2: 8.0 MiB in 0.2 s, 512 bytes/s",
		"Sent 1.7 GiB in 46.3 s (37.1 MiB/s); disk 1.6 GiB of 16.0 GiB carried; destination: verified",
		"Resumed s on me@box; downtime 1450 ms",
		"Timings (ms): fence 1326, transfer 4765, restore paused 861, commit 489, verify 12",
	} {
		r.Add(line)
	}
	want := MoveReport{Rounds: []MoveRound{
		{Name: "round 0", Round: 0, Size: "1048576 bytes", Bytes: 1048576, Seconds: 0.5, MiBPerSec: 2, DirtyPagesPerSec: 120},
		{Name: "Round 1", Round: 1, Size: "65536 bytes", Bytes: 65536, Seconds: 0.1, MiBPerSec: 0.5, DirtyPagesPerSec: 40.5},
		{Name: "round 2", Round: 2, Size: "8.0 MiB", Seconds: 0.2, MiBPerSec: 512.0 / (1 << 20)},
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

// hull's streamed move names its rounds by what they copy: the disk rounds
// numbered and then the final one taken in the pause, and the memory rounds
// by hvi's component names. Each is a round of its kind, the final ones
// marked, and the downtime is read from hull's closing line.
func TestMoveReportReadsAStreamedMove(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Streaming s to me@box (stream)",
		"move: disk round 0: 2381 blocks, 148.8 MiB (61.0 MiB on the wire) in 1.92 s, 31.8 MiB/s",
		"move: disk round 1: 12 blocks, 768.0 KiB (301.2 KiB on the wire) in 0.20 s, 1.5 MiB/s, guest wrote 60 blocks/s",
		"move: ram-round-0: 90.7 MiB in 2.10 s, 43.2 MiB/s",
		"move: ram-round-1: 3.1 MiB in 0.09 s, 34.4 MiB/s",
		"move: hvi sealed the stream and fenced s (310 ms after the last RAM round began)",
		"move: disk final round: 2 blocks, 128.0 KiB (40.0 KiB on the wire) in 0.03 s, 1.3 MiB/s",
		"Sent 64.1 MiB in 4.6 s (13.9 MiB/s): disk 149.6 MiB as 61.3 MiB, RAM 93.8 MiB, state 1.2 MiB; destination: complete",
		"move: 64.1 MiB in 4.6 s; disk 61.3 MiB, RAM 93.8 MiB, state 1.2 MiB; sealed 310 ms after the last RAM round " +
			"began, last disk round 30 ms; downtime about 1450 ms",
	} {
		r.Add(line)
	}
	var got []string
	for _, rd := range r.Rounds {
		got = append(got, fmt.Sprintf("%s|%s|%d|%v|%s|%.2f|%.0f", rd.Name, rd.Kind, rd.Round, rd.Final, rd.Size,
			rd.Seconds, rd.DirtyBlocksPerSec))
	}
	want := []string{
		"disk round 0|disk|0|false|148.8 MiB|1.92|0",
		"disk round 1|disk|1|false|768.0 KiB|0.20|60",
		"ram-round-0|ram|0|false|90.7 MiB|2.10|0",
		"ram-round-1|ram|1|false|3.1 MiB|0.09|0",
		"disk final round|disk|2|true|128.0 KiB|0.03|0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rounds:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.DowntimeSeconds != 1.45 || r.MiBPerSec != 13.9 || r.Bytes != 0 {
		t.Errorf("report %+v", r)
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
