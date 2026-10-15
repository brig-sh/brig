package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A report reads the parts of hull's lines it knows, each on its own. These
// are hull's lines for a whole-bundle move, as brig passed them on when it
// moved the ubuntu guest from rp1 to rp2 with hull 97b7c58: no rounds, no
// downtime line, and a Sent line whose size is rounded, so the report has
// the rate and the steps and no byte count. hull's line on the capture
// says "paused 0 ms", which is not the guest's downtime and is not read as
// it. A line with none of the parts changes nothing, so the JSON leaves out
// what hull did not say.
func TestMoveReportReadsHullsLines(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Move " + tx1 + " of brig-ubuntu to pmoust@rp2, epoch 1",
		"Reserved brig-ubuntu on pmoust@rp2",
		"Fencing brig-ubuntu (checkpoint with then=fence)",
		"Checkpoint written in then fence, paused 0 ms, capture 1318 ms, RAM 90.7 of 1024.0 MiB stored",
		"Fenced brig-ubuntu: checkpoint /home/pmoust/b5/store/instances/brig-ubuntu/checkpoint/" + tx1,
		"Sending brig-ubuntu to pmoust@rp2",
		"Sent 203.1 MiB in 4.8 s (42.6 MiB/s); disk 112.3 MiB of 15.0 GiB carried; destination: complete: " +
			"every part a restore needs is captured",
		"Restoring brig-ubuntu paused on pmoust@rp2",
		"Moved brig-ubuntu to pmoust@rp2: it runs there at epoch 1. Its checkpoint here is stale",
		"Timings (ms): fence 1326, transfer 4765, restore paused 861, commit 489",
	} {
		r.Add(line)
	}
	want := MoveReport{MiBPerSec: 42.6,
		StepsMs: map[string]int64{"fence": 1326, "transfer": 4765, "restore paused": 861, "commit": 489}}
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
	r.Seconds = 9.3
	if s := r.Summary(); s != "hull's move took 9.3 s, at 42.6 MiB/s; "+
		"fence 1.33 s, transfer 4.76 s, restore paused 0.86 s, commit 0.49 s" {
		t.Errorf("summary %q", s)
	}
	// hull prints a size below 1 MiB as a count of bytes, and that count
	// is the total.
	var small MoveReport
	small.Add("Sent 524288 bytes in 0.1 s (5.0 MiB/s); disk 0 bytes of 15.0 GiB carried; destination: complete")
	if small.Bytes != 524288 || small.MiBPerSec != 5 {
		t.Errorf("small move %+v", small)
	}
}

// hull's streamed move names its rounds by what they copy: the disk rounds
// numbered and then the final one taken in the pause, and the memory rounds
// by hvi's component names. Each is a round of its kind, the final ones
// marked: hull names the final disk round, and the last memory round is
// marked when hull says hvi sealed the stream, since hvi takes it in the
// pause. The downtime is read from hull's closing line. A line on how
// far a long round has got is not a round. The lines are hull 4a30a43's.
func TestMoveReportReadsAStreamedMove(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Streaming s to me@box (stream)",
		"move: disk round 0: 812 blocks so far, 30.1 MiB/s (34% of 2381)",
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
		"ram-round-1|ram|1|true|3.1 MiB|0.09|0",
		"disk final round|disk|2|true|128.0 KiB|0.03|0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rounds:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.DowntimeSeconds != 1.45 || r.MiBPerSec != 13.9 || r.Bytes != 0 {
		t.Errorf("report %+v", r)
	}
}

// The lines brig passed on during a streamed move from rp1 to rp2 through
// brig teleport, with hull 1ecb5c8 and hvi ef8833a, as printed: a round is
// counted once, at its end, the last memory round is the one taken in the
// pause though hull does not name it final, and the state's 53259 bytes on
// the Sent line are not the total, which hull gave only rounded.
func TestMoveReportReadsALiveStreamedMove(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Transport: streamed (pre-copy rounds; the disk blocks that differ from base 13fd46b9d871)",
		"move: disk round 0: 3593 blocks so far, 17.1 MiB/s (81% of 4393)",
		"move: disk round 0: 4393 blocks, 274.6 MiB (34.4 MiB on the wire) in 2.26 s, 15.2 MiB/s",
		"move: disk round 1: 2 blocks, 131072 bytes (797 bytes on the wire) in 0.00 s, 383648 bytes/s, guest wrote 1 blocks/s",
		"move: ram-round-0: 48.7 MiB so far, 24.2 MiB/s",
		"move: ram-round-0: 74.0 MiB in 2.88 s, 25.7 MiB/s",
		"move: ram-round-1: 1.0 MiB in 0.03 s, 34.4 MiB/s",
		"move: disk round 2 (final): 4 blocks, 262144 bytes (5696 bytes on the wire) in 0.00 s, 1.5 MiB/s",
		"move: hvi sealed the stream and fenced brig-ubuntu (39 ms after the last RAM round began)",
		"Sent 109.5 MiB in 5.4 s (20.3 MiB/s): disk 274.9 MiB as 34.4 MiB, RAM 75.0 MiB, state 53259 bytes; " +
			"destination: stream: 80 hvi components and 4399 disk blocks in 288 batches checked; hvi restored the guest paused",
		"Timings (ms): fence 0, transfer 5394, restore paused 0, commit 91",
		"move: 109.5 MiB in 5.4 s; disk 34.4 MiB, RAM 75.0 MiB, state 53259 bytes; sealed 39 ms after the last RAM " +
			"round began, last disk round 3 ms; downtime about 358 ms",
	} {
		r.Add(line)
	}
	var names []string
	for _, rd := range r.Rounds {
		names = append(names, fmt.Sprintf("%s/%s/%v", rd.Name, rd.Size, rd.Final))
	}
	want := "disk round 0/274.6 MiB/false|disk round 1/131072 bytes/false|ram-round-0/74.0 MiB/false|" +
		"ram-round-1/1.0 MiB/true|disk round 2 (final)/262144 bytes/true"
	if strings.Join(names, "|") != want {
		t.Errorf("rounds %s\nwant   %s", strings.Join(names, "|"), want)
	}
	if r.Bytes != 0 || r.DowntimeSeconds != 0.358 || r.MiBPerSec != 20.3 || r.StepsMs["transfer"] != 5394 {
		t.Errorf("report %+v", r)
	}
	if s := r.Summary(); !strings.HasPrefix(s, "hull's move took 0.0 s, streamed, ") ||
		!strings.HasSuffix(s, "5 rounds (3 disk, 2 memory), the final disk round 262144 bytes in 0.00 s; "+
			"transfer 5.39 s, commit 0.09 s") || r.Transport != "stream" {
		t.Errorf("summary %q", s)
	}
}

// A fake hull printing its lines while it moves: they reach a MoveReport
// through the move's Progress, as brig teleport wires them. The lines are
// hull 1ecb5c8's, from a streamed move of the ubuntu guest.
func TestAMoveReportIsFedAsTheMoveRuns(t *testing.T) {
	h, _ := migrateHull(t, "printf '"+
		"move: disk round 1: 2 blocks, 131072 bytes (797 bytes on the wire) in 0.00 s, 383648 bytes/s, "+
		"guest wrote 1 blocks/s\\n"+
		"move: ram-round-0: 74.0 MiB in 2.88 s, 25.7 MiB/s\\n"+
		"Sent 109.5 MiB in 5.4 s (20.3 MiB/s): disk 274.9 MiB as 34.4 MiB, RAM 75.0 MiB, state 53259 bytes; "+
		"destination: stream: 80 hvi components and 4399 disk blocks in 288 batches checked\\n"+
		"move: 109.5 MiB in 5.4 s; disk 34.4 MiB, RAM 75.0 MiB, state 53259 bytes; sealed 39 ms after the "+
		"last RAM round began, last disk round 3 ms; downtime about 358 ms\\n' >&2; echo committed > RECORD")
	var r MoveReport
	if _, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1, Progress: r.Add}); err != nil {
		t.Fatal(err)
	}
	if len(r.Rounds) != 2 || r.Rounds[0].Bytes != 131072 || r.Rounds[0].DirtyBlocksPerSec != 1 ||
		r.Bytes != 0 || r.MiBPerSec != 20.3 || r.DowntimeSeconds != 0.358 {
		t.Errorf("report %+v", r)
	}
	if !strings.Contains(r.Summary(), "at 20.3 MiB/s, 2 rounds") {
		t.Errorf("summary %q", r.Summary())
	}
}

// A memory round is final only once the rounds are over: while hull is
// still copying, the last round so far is not the paused one. A hull that
// does not say it sealed ends the rounds with its Sent line.
func TestOnlyTheLastMemoryRoundIsMarkedPaused(t *testing.T) {
	var r MoveReport
	r.Add("move: ram-round-0: 74.0 MiB in 2.88 s, 25.7 MiB/s")
	if r.Rounds[0].Final {
		t.Fatal("a memory round was marked paused before the rounds ended")
	}
	r.Add("move: ram-round-1: 1.0 MiB in 0.03 s, 34.4 MiB/s")
	r.Add("move: disk round 2 (final): 4 blocks, 262144 bytes (5696 bytes on the wire) in 0.00 s, 1.5 MiB/s")
	r.Add("Sent 109.5 MiB in 5.4 s (20.3 MiB/s): disk 274.9 MiB as 34.4 MiB, RAM 75.0 MiB, state 53259 bytes")
	var got []bool
	for _, rd := range r.Rounds {
		got = append(got, rd.Final)
	}
	if fmt.Sprint(got) != "[false true true]" {
		t.Errorf("final %v, want [false true true]", got)
	}
}

// hull says how it moves the guest on its Transport line, and says it again
// when the base it sends first fails and it falls back to the whole bundle:
// the last line is the one that counts. A hull that prints no Transport
// line, as 97b7c58 did not, leaves it out of the report rather than have
// brig guess.
func TestMoveReportReadsHullsTransport(t *testing.T) {
	var r MoveReport
	r.Add("Transport: streamed (pre-copy rounds; the disk blocks that differ from base 13fd46b9d871)")
	if r.Transport != "stream" || r.TransportWhy != "" {
		t.Errorf("streamed: %+v", r)
	}
	r.Add("Transport: the whole checkpoint and disk, because sending the base failed: no space left on device")
	if r.Transport != "bundle" || r.TransportWhy != "sending the base failed: no space left on device" {
		t.Errorf("fallback: %+v", r)
	}
	r.Seconds = 9.3
	if s := r.Summary(); s != "hull's move took 9.3 s, the whole checkpoint and disk "+
		"(sending the base failed: no space left on device)" {
		t.Errorf("summary %q", s)
	}
	var old MoveReport
	old.Add("Move " + tx1 + " of brig-ubuntu to pmoust@rp2, epoch 1")
	old.Add("Sent 203.1 MiB in 4.8 s (42.6 MiB/s); disk 112.3 MiB of 15.0 GiB carried; destination: complete")
	if b, _ := json.Marshal(old); strings.Contains(string(b), "transport") {
		t.Errorf("a hull that said nothing of its transport gave %s", b)
	}
}

// rounds is each round of a report as name|kind|round|final|size|seconds.
func rounds(r MoveReport) string {
	var got []string
	for _, rd := range r.Rounds {
		got = append(got, fmt.Sprintf("%s|%s|%d|%v|%s|%.2f", rd.Name, rd.Kind, rd.Round, rd.Final, rd.Size, rd.Seconds))
	}
	return strings.Join(got, "\n")
}

// The lines of a streamed move from rp2 to rp1 with the hull of U5's fix
// (d4ae775; 511877d on top of it changes only docs, so it prints the same),
// as hull printed them: three disk rounds while the guest ran, five memory
// rounds, the seal, then the final disk round with the guest stopped. The
// summary says how many rounds of each kind, and the final disk round, which
// the guest waited for.
func TestMoveReportReadsTheLinesOfHull511877d(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"Streaming fx1 to pmoust@rp1 (live)",
		"move: the destination took the stream in 2.2 s (it copied the base)",
		"move: disk round 0: 1574 blocks so far, 48.1 MiB/s",
		"move: disk round 0: 1731 blocks, 108.2 MiB (97.6 MiB on the wire) in 3.13 s, 31.2 MiB/s",
		"move: disk round 1: 163 blocks, 10.2 MiB (10.0 MiB on the wire) in 0.19 s, 53.5 MiB/s, guest wrote 52 blocks/s",
		"move: disk round 2: 0 blocks, 0 bytes (0 bytes on the wire) in 0.01 s, 0 bytes/s",
		"move: ram-round-0: 123.2 MiB so far, 61.6 MiB/s",
		"move: ram-round-0: 138.2 MiB in 2.56 s, 53.9 MiB/s",
		"move: ram-round-1: 15.7 MiB in 0.26 s, 59.5 MiB/s",
		"move: ram-round-2: 3.9 MiB in 0.09 s, 45.5 MiB/s",
		"move: ram-round-3: 2.9 MiB in 0.05 s, 53.8 MiB/s",
		"move: ram-round-4: 28854 bytes in 0.00 s, 27.5 MiB/s",
		"move: hvi sealed the stream and fenced fx1 (7 ms after the last RAM round began)",
		"move: disk round 3 (final): 163 blocks, 10.2 MiB (10.0 MiB on the wire) in 0.16 s, 64.1 MiB/s",
		"Sent 278.6 MiB in 8.8 s (31.5 MiB/s): disk 128.6 MiB as 117.7 MiB, RAM 160.8 MiB, state 53575 bytes; " +
			"destination: stream: 154 hvi components and 2057 disk blocks in 143 batches checked; hvi restored the guest paused",
		"Moved fx1 to pmoust@rp1: it runs there at epoch 8. Its checkpoint here is stale",
		"Timings (ms): fence 0, transfer 8843, restore paused 0, commit 54",
		"move: 278.6 MiB in 8.8 s; disk 117.7 MiB, RAM 160.8 MiB, state 53575 bytes; sealed 7 ms after the last RAM " +
			"round began, last disk round 10.2 MiB in 156 ms; downtime about 13442 ms",
		"move: the guest wrote up to 3.3 MiB/s to its disk during the disk rounds",
	} {
		r.Add(line)
	}
	want := strings.Join([]string{
		"disk round 0|disk|0|false|108.2 MiB|3.13",
		"disk round 1|disk|1|false|10.2 MiB|0.19",
		"disk round 2|disk|2|false|0 bytes|0.01",
		"ram-round-0|ram|0|false|138.2 MiB|2.56",
		"ram-round-1|ram|1|false|15.7 MiB|0.26",
		"ram-round-2|ram|2|false|3.9 MiB|0.09",
		"ram-round-3|ram|3|false|2.9 MiB|0.05",
		"ram-round-4|ram|4|true|28854 bytes|0.00",
		"disk round 3 (final)|disk|3|true|10.2 MiB|0.16",
	}, "\n")
	if got := rounds(r); got != want {
		t.Errorf("rounds:\n%s\nwant:\n%s", got, want)
	}
	if r.DowntimeSeconds != 13.442 || r.MiBPerSec != 31.5 || r.Transport != "" || r.Rounds[1].DirtyBlocksPerSec != 52 {
		t.Errorf("report %+v", r)
	}
	if s := r.Summary(); !strings.Contains(s, ", 9 rounds (4 disk, 5 memory), the final disk round 10.2 MiB in 0.16 s; ") {
		t.Errorf("summary %q", s)
	}
}

// Once hvi carries the disk rounds inside its migrate stream, hull names
// them by hvi's components: "disk-round-k" before "ram-round-k" while the
// guest runs, and "disk-round-final" in the pause. These lines are
// anticipated, not observed: the names are the ones SPEC-H8 C1 fixes, worded
// the way hull 511877d words a round. The final round is final and numbered
// after the others of its kind, not read as round 0, and the last memory
// round is the paused one once hull says hvi sealed the stream.
func TestMoveReportReadsTheRoundsHviStreams(t *testing.T) {
	var r MoveReport
	for _, line := range []string{
		"move: disk-round-0: 812 blocks so far, 30.1 MiB/s",
		"move: disk-round-0: 1731 blocks, 108.2 MiB (97.6 MiB on the wire) in 3.13 s, 31.2 MiB/s",
		"move: ram-round-0: 138.2 MiB in 2.56 s, 53.9 MiB/s",
		"move: disk-round-1: 163 blocks, 10.2 MiB (10.0 MiB on the wire) in 0.19 s, 53.5 MiB/s, guest wrote 52 blocks/s",
		"move: ram-round-1: 15.7 MiB in 0.26 s, 59.5 MiB/s",
		"move: disk-round-final: 4 blocks, 262144 bytes (5696 bytes on the wire) in 0.01 s, 25.0 MiB/s",
		"move: ram-round-2: 28854 bytes in 0.00 s, 27.5 MiB/s",
		"move: hvi sealed the stream and fenced s (7 ms after the last RAM round began)",
		"move: a line hull has not printed before: 3 of something",
	} {
		r.Add(line)
	}
	want := strings.Join([]string{
		"disk-round-0|disk|0|false|108.2 MiB|3.13",
		"ram-round-0|ram|0|false|138.2 MiB|2.56",
		"disk-round-1|disk|1|false|10.2 MiB|0.19",
		"ram-round-1|ram|1|false|15.7 MiB|0.26",
		"disk-round-final|disk|2|true|262144 bytes|0.01",
		"ram-round-2|ram|2|true|28854 bytes|0.00",
	}, "\n")
	if got := rounds(r); got != want {
		t.Errorf("rounds:\n%s\nwant:\n%s", got, want)
	}
	if s := r.Summary(); !strings.Contains(s, "6 rounds (3 disk, 3 memory), the final disk round 262144 bytes in 0.01 s") {
		t.Errorf("summary %q", s)
	}
}

// A line the report does not know changes nothing in it, and it reaches
// the move's Progress whole, as hull printed it: brig prints every line
// hull prints, so a hull that words its lines anew is still seen.
func TestALineTheReportDoesNotKnowIsPassedOnWhole(t *testing.T) {
	lines := []string{
		"move: hvi: 3 disk components, 12 frames",
		"Moving s in a way brig has not seen: 42 of 43",
		"move: round trip 3 ms to the destination",
	}
	h, _ := migrateHull(t, "printf '"+strings.Join(lines, "\\n")+"\\n' >&2; echo committed > RECORD")
	var r MoveReport
	var seen []string
	each := func(line string) { seen = append(seen, line); r.Add(line) }
	if _, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1, Progress: each}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "\n") != strings.Join(lines, "\n") {
		t.Errorf("passed on:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(lines, "\n"))
	}
	if b, _ := json.Marshal(r); string(b) != "{}" {
		t.Errorf("lines the report does not know gave %s", b)
	}
}
