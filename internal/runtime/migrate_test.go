package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tx1 = "0123456789abcdef0123456789abcdef"

// The move's line names the destination, the store and the hull there, the
// gateway brig started there and the txid, and the sandbox last, where
// hull's parser takes it. A dry run changes nothing and needs no txid.
func TestMigrateArgs(t *testing.T) {
	for _, c := range []struct {
		what string
		spec MigrateSpec
		want string
	}{
		{"a move", MigrateSpec{TxID: tx1}, "migrate --to me@box --txid " + tx1 + " s"},
		{"into a named store, through brig's gateway", MigrateSpec{TxID: tx1, RemoteStore: "/st/b",
			RemoteHull: "/opt/hull", GatewaySock: "/g/sandbox-s.sock"},
			"migrate --to me@box --remote-store /st/b --remote-hull /opt/hull --remote-gateway-sock /g/sandbox-s.sock --txid " + tx1 + " s"},
		{"a dry run", MigrateSpec{DryRun: true}, "migrate --to me@box --dry-run s"},
	} {
		got, err := migrateArgs("s", "me@box", c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q, want %q", c.what, strings.Join(got, " "), c.want)
		}
	}
	if got, err := migrateArgs("s", "me@box", MigrateSpec{}); err == nil {
		t.Errorf("a move with no txid was accepted as %q", got)
	}
	if got, err := migrateArgs("s", "me@box", MigrateSpec{TxID: "tp-1"}); err == nil {
		t.Errorf("a txid hull does not take was accepted as %q", got)
	}
	if _, err := (&hull{bin: "/nonexistent"}).MigrateAbort("s", ""); err == nil {
		t.Errorf("an abort naming no move was accepted")
	}
}

// hull's status report is read from its first line: the move, its state and
// the other host. A line for another instance, or one that does not read, is
// an error rather than a guess.
func TestMoveStatusOf(t *testing.T) {
	out := "Move " + tx1 + " of s: committing, to me@box, epoch 1\n  2026-10-14 06:00:00.000  preflight\n"
	got, err := moveStatusOf("s", out)
	if err != nil || got.TxID != tx1 || got.State != "committing" || got.Peer != "me@box" {
		t.Errorf("got %+v, %v", got, err)
	}
	for _, bad := range []string{"", "Move x of s: fenced, to me@box, epoch 1", "Move " + tx1 + " of other: fenced, to me@box, epoch 1"} {
		if got, err := moveStatusOf("s", bad); err == nil {
			t.Errorf("%q read as %+v", bad, got)
		}
	}
	for state, fenced := range map[string]bool{"": false, MovePreflight: false, MoveReserved: false,
		MoveAborted: false, "fenced": true, "transferring": true, "committing": true, MoveCommitted: true} {
		if MoveFenced(state) != fenced {
			t.Errorf("MoveFenced(%q) = %v", state, !fenced)
		}
	}
}

// A destination ssh could read as an option, or as more than one word, is
// refused before it reaches any command line.
func TestCheckRemote(t *testing.T) {
	for _, good := range []string{"me@box", "pm.oust@build-box.tail1234.ts.net", "a_b@10.0.0.7", "me@[fe80::1]"} {
		if err := CheckRemote(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "box", "@box", "me@", "me@a@b",
		"-oProxyCommand=sh@box", "me@-oProxyCommand=sh",
		"me@box;reboot", "me@box rm", "me box@box", "me@$(id)", "me@`id`",
		"me@box'", "me@box\nx", "me@[]",
	} {
		if err := CheckRemote(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// migrateHull is a hull that logs every argv, answers a move by recording
// the state it leaves, and reports that record the way `hull migrate
// --status` does.
func migrateHull(t *testing.T, migrate string) (*hull, string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	rec := dir + "/record"
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"  'migrate --status') [ -f " + rec + " ] || { echo 'error: instance s has no move on record' >&2; exit 1; }\n" +
		"    echo \"Move " + tx1 + " of s: $(cat " + rec + "), to me@box, epoch 1\" ;;\n" +
		"  'migrate --abort') echo aborted > " + rec + "; echo 'Aborted move' >&2 ;;\n" +
		"  migrate*) " + strings.ReplaceAll(migrate, "RECORD", rec) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin}, log
}

// The seam drives hull with the lines the builders make and reads hull's
// move record back after a move, a status and an abort alike.
func TestHullMigrateSeam(t *testing.T) {
	h, log := migrateHull(t, "case \"$*\" in *--dry-run*) ;; *) echo committed > RECORD ;; esac")
	if st, err := h.MigrateStatus("s"); err != nil || st != (MoveStatus{Read: true}) {
		t.Fatalf("before a move: %+v, %v; want a read with no record", st, err)
	}
	if _, err := h.Migrate("s", "me@box", MigrateSpec{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	st, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1, RemoteStore: "/st"})
	if err != nil || st.TxID != tx1 || st.State != MoveCommitted || st.Peer != "me@box" {
		t.Errorf("move: %+v, %v", st, err)
	}
	st, err = h.MigrateAbort("s", tx1)
	if err != nil || st.State != MoveAborted {
		t.Errorf("abort: %+v, %v", st, err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "migrate --status s\n" +
		"migrate --to me@box --dry-run s\n" +
		"migrate --to me@box --remote-store /st --txid " + tx1 + " s\nmigrate --status s\n" +
		"migrate --abort " + tx1 + "\nmigrate --status s\n"
	if string(got) != wantLog {
		t.Errorf("hull was asked:\n%s\nwant:\n%s", got, wantLog)
	}
}

// A move that failed still has its record read: a move that failed after
// the fence leaves the source unable to run, and the caller decides on that,
// not on the exit status alone.
func TestAFailedMoveStillReadsTheRecord(t *testing.T) {
	h, _ := migrateHull(t, "echo committing > RECORD; echo 'ssh: connection reset' >&2; exit 1")
	st, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("a failed move came back as %v", err)
	}
	if !st.Read || st.State != "committing" || st.TxID != tx1 {
		t.Errorf("record after a failed move: %+v", st)
	}
}

// A move hull stopped says why. hull narrates the steps before the line that
// names the reason, and the error keeps that line however many steps came
// first, as a move whose checkpoint hvi refused printed on real hardware.
func TestAStoppedMoveSaysWhyAfterItsSteps(t *testing.T) {
	h, _ := migrateHull(t, "echo aborted > RECORD; "+
		"printf 'Move %s of s to me@box, epoch 1\\nReserved s on me@box\\nFencing s (checkpoint with then=fence)\\n' "+tx1+" >&2; "+
		"echo 'error: move stopped at reserved: hvi refused checkpoint: no backend checkpoints it yet (refused)' >&2; "+
		"echo 'The instance still runs here.' >&2; exit 1")
	_, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1})
	if err == nil || !strings.Contains(err.Error(), "hvi refused checkpoint: no backend checkpoints it yet") ||
		!strings.Contains(err.Error(), "Fencing s") {
		t.Fatalf("a stopped move came back as %v, want its steps and its reason", err)
	}
	if strings.Contains(err.Error(), "still runs here") {
		t.Errorf("the error kept lines past the reason: %v", err)
	}
}

// A record that does not read is not a record with no move in it. After a
// move that failed, and after one that hull says succeeded, the status comes
// back unread with the read's error beside the move's, so the caller cannot
// take it for a move that never began.
func TestAnUnreadRecordIsNotNoRecord(t *testing.T) {
	for _, c := range []struct {
		what, migrate string
		want          []string
	}{
		{"a failed move", "echo transferring > RECORD; echo 'ssh: connection reset' >&2; exit 1",
			[]string{"connection reset", "did not read"}},
		{"a move hull says committed", "echo committed > RECORD",
			[]string{"did not read"}},
	} {
		h, _ := migrateHull(t, c.migrate)
		// hull's status line changes shape: brig cannot read it.
		script, err := os.ReadFile(h.bin)
		if err != nil {
			t.Fatal(err)
		}
		script = []byte(strings.Replace(string(script), ", epoch 1", ", generation 1", 1))
		if err := os.WriteFile(h.bin, script, 0o755); err != nil {
			t.Fatal(err)
		}
		st, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1})
		if st.Read || st != (MoveStatus{}) {
			t.Errorf("%s: an unparsable record came back as %+v", c.what, st)
		}
		for _, w := range c.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %v does not say %q", c.what, err, w)
			}
		}
		if err != nil && strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: error spans lines: %q", c.what, err)
		}
	}
}

// A move brig asks for leaves only through ssh to the host named: hull is not
// handed a program to run in ssh's place, even when brig's own environment
// has one. Its status and its abort ask the other host too, and are not
// handed one either.
func TestAMoveIsNotHandedAnotherTransport(t *testing.T) {
	t.Setenv("HULL_MIGRATE_SSH", "/tmp/elsewhere")
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "env.log")
	script := "#!/bin/sh\necho \"$1 $2 transport=${HULL_MIGRATE_SSH:-none}\" >> " + log + "\n" +
		"[ \"$2\" = --status ] && echo 'Move " + tx1 + " of s: committed, to me@box, epoch 1'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &hull{bin: bin}
	if _, err := h.Migrate("s", "me@box", MigrateSpec{TxID: tx1}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MigrateAbort("s", tx1); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	statuses := 0
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if strings.HasPrefix(line, "migrate --status") {
			statuses++
		}
		if strings.HasPrefix(line, "migrate --to") || strings.HasPrefix(line, "migrate --abort") ||
			strings.HasPrefix(line, "migrate --status") {
			if !strings.HasSuffix(line, "transport=none") {
				t.Errorf("hull was handed a transport: %q", line)
			}
		}
	}
	if statuses == 0 {
		t.Errorf("hull's record was not read back: %s", got)
	}
	if !strings.Contains(string(got), "migrate --to transport=none") {
		t.Errorf("the move was not asked: %s", got)
	}
}
