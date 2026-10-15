package runtime

import "testing"

// hull7b3f7b1Move is what hull 7b3f7b1 printed on stderr for a streamed move
// of the ubuntu guest from rp1 to rp2, copied from the recording of
// 2026-10-15 (docs/media/teleport-pi-pair.mp4), in order.
var hull7b3f7b1Move = []string{
	"Move 08496b841d5cbbd64659262a4a351500 of brig-ubuntu to pmoust@rp2, epoch 1",
	"Transport: streamed (pre-copy rounds; the disk blocks that differ from base 316f7a8c31d4)",
	"Reserved brig-ubuntu on pmoust@rp2",
	"Streaming brig-ubuntu to pmoust@rp2 (live)",
	"move: disk-round-0: 2.5 MiB in 0.11 s, 22.5 MiB/s",
	"move: ram-round-0: 40.6 MiB in 1.00 s, 40.7 MiB/s",
	"move: disk-round-final: 0 bytes in 0.01 s, 0 bytes/s",
	"move: ram-round-1: 1.3 MiB in 0.01 s, 109.2 MiB/s",
	"move: hvi: 2 RAM rounds (converged), disk rounds of 3876, 0 blocks (242.2 MiB, 2.5 MiB on the wire); " +
		"paused 24 ms, disk sync 1 ms before the pause and 0 ms in it",
	"move: hvi sealed the stream and fenced brig-ubuntu (19 ms after the pause)",
	"Sent 44.4 MiB in 1.2 s (36.7 MiB/s): disk 242.2 MiB as 2.5 MiB, RAM 41.9 MiB, state 54052 bytes; " +
		"destination: stream: 51 hvi components checked; hvi checked them against its manifest, wrote the disk " +
		"rounds onto base 316f7a8c31d4 and restored the guest paused",
	"Moved brig-ubuntu to pmoust@rp2: it runs there at epoch 1. Its checkpoint here is stale",
	"Timings (ms): fence 0, transfer 1208, restore paused 0, commit 13",
	"move: 44.4 MiB in 1.2 s; disk 2.5 MiB, RAM 41.9 MiB, state 54052 bytes; sealed 19 ms after the pause, " +
		"last disk round 0 bytes in 0 ms; downtime about 60 ms",
	"move: from the seal, ms (there: the destination's clock): trailer sent 0, instance's fence recorded 5, " +
		"there manifest read 5, there trailer read 5, there hvi restored the guest paused 18, there base checked " +
		"18, there verified recorded 23, destination answered 27, there commit received 28, commit point " +
		"recorded 30, commit sent 30, there ledger written 31, there running recorded 34, there resumed 35, " +
		"commit answered 44",
}

// By default a move says one line a round, the pause, the seal, the
// restore and the resume, in a few words each, and nothing of the rest:
// hull's dump of the steps from the seal and its Timings line are in the
// report and in --json, and --verbose prints them whole.
func TestAQuietMoveSaysTheRoundsThePauseAndTheEnd(t *testing.T) {
	var got []string
	for _, line := range hull7b3f7b1Move {
		if s, ok := QuietLine(line); ok {
			got = append(got, s)
		}
	}
	want := []string{
		"disk-round-0 2.5 MiB in 0.11 s",
		"ram-round-0 40.6 MiB in 1.00 s",
		"disk-round-final 0 bytes in 0.01 s",
		"ram-round-1 1.3 MiB in 0.01 s",
		"paused after 2 RAM rounds (converged)",
		"sealed 19 ms after the pause",
		"sent 44.4 MiB in 1.2 s; restored paused there",
		"resumed on pmoust@rp2 (epoch 1)",
	}
	if len(got) != len(want) {
		t.Fatalf("said %d lines, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q, want %q", i, got[i], want[i])
		}
	}
}

// The quiet lines read hull's older spellings too, and the whole-bundle
// move's, and keep what explains a long wait: the base sent first, and a
// destination that copied it in the pause. A round's progress is not a
// round, and hull's warnings and errors are never held back.
func TestQuietLinesOfOtherMoves(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"move: disk round 3 (final): 4 blocks, 262144 bytes (5696 bytes on the wire) in 0.00 s, 1.5 MiB/s",
			"disk round 3 (final) 262144 bytes in 0.00 s"},
		{"move: hvi sealed the stream and fenced brig-claude-code (39 ms after the last RAM round began)",
			"sealed 39 ms after the last RAM round began"},
		{"move: hvi sealed the stream and fenced s", "sealed"},
		{"Fencing brig-ubuntu (checkpoint with then=fence)", "paused to fence brig-ubuntu"},
		{"Fenced brig-ubuntu: checkpoint /st/x", "fenced brig-ubuntu"},
		{"Sent 288.2 MiB in 5.1 s (56.5 MiB/s); disk 1.0 GiB of 1.0 GiB carried; destination: verified",
			"sent 288.2 MiB in 5.1 s"},
		{"Restoring brig-ubuntu paused on pmoust@rp2", "restoring brig-ubuntu paused on pmoust@rp2"},
		{"Sending base 316f7a8c31d4 to pmoust@rp1 (once per base; `hull migrate-prepare` does it ahead of a move)",
			"sending base 316f7a8c31d4 to pmoust@rp1 first"},
		{"move: the destination took the stream in 22.9 s (it copied the base)",
			"the destination took the stream in 22.9 s (it copied the base)"},
		{"warning: stopping the fenced VMM here: exit status 1", "warning: stopping the fenced VMM here: exit status 1"},
		{"error: ssh: connection reset by peer", "error: ssh: connection reset by peer"},
		{"move: hvi did not fence brig-ubuntu: its stream ended before the manifest, and the guest runs on here",
			"hvi did not fence brig-ubuntu: its stream ended before the manifest, and the guest runs on here"},
	} {
		if got, ok := QuietLine(c.line); !ok || got != c.want {
			t.Errorf("%q: said %q (%v), want %q", c.line, got, ok, c.want)
		}
	}
	for _, line := range []string{
		"move: disk round 0: 1574 blocks so far, 48.1 MiB/s",
		"move: disk-round-0: 812.0 MiB so far, 30.1 MiB/s",
		"Preparing pmoust@rp1 for brig-ubuntu",
		"  base 316f7a8c31d4: held (a spare copy made for the move (copied))",
		"pmoust@rp1 is prepared: a move of brig-ubuntu sends only what the guest changed",
		"Timings (ms): fence 0, transfer 1208, restore paused 0, commit 13",
	} {
		if got, ok := QuietLine(line); ok {
			t.Errorf("%q is for --verbose, and said %q", line, got)
		}
	}
}
