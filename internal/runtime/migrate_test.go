package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The move's line names the destination, the store there and the txid, and
// the sandbox last, where hull's parser takes it. A dry run pauses nothing
// and needs no txid.
func TestMigrateArgs(t *testing.T) {
	for _, c := range []struct {
		what string
		spec MigrateSpec
		want string
	}{
		{"a move", MigrateSpec{TxID: "tx1"}, "migrate --to me@box --txid tx1 s"},
		{"into a named store", MigrateSpec{TxID: "tx1", RemoteStore: "/st/b"},
			"migrate --to me@box --remote-store /st/b --txid tx1 s"},
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
	if got, err := migrateAbortArgs("s", ""); err == nil {
		t.Errorf("an abort naming no move was accepted as %q", got)
	}
	if got, _ := migrateAbortArgs("s", "tx1"); strings.Join(got, " ") != "migrate-abort --txid tx1 s" {
		t.Errorf("abort line %q", strings.Join(got, " "))
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

// migrateHull is a hull that logs every argv, moves by recording the
// migration record a move leaves, and aborts by recording the abort.
func migrateHull(t *testing.T, migrate string) (*hull, string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1\" in\n" +
		"  migrate) " + migrate + " ;;\n" +
		"  migrate-status) echo 'move tx1 of s to me@box: migrated' ;;\n" +
		"  migrate-abort) echo '{\"id\":\"s\",\"migration\":{\"state\":\"aborted\",\"txid\":\"tx1\",\"peer\":\"me@box\"}}' > " + dir + "/record\n" +
		"    echo 'move tx1 aborted' ;;\n" +
		"  inspect) cat " + dir + "/record 2>/dev/null || echo '{\"id\":\"s\"}' ;;\n" +
		"esac\n"
	script = strings.ReplaceAll(script, "RECORD", dir+"/record")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin}, log
}

// The seam drives hull with the lines the builders make and reads hull's
// record back for where the move stands, after a move, a status and an
// abort alike.
func TestHullMigrateSeam(t *testing.T) {
	h, log := migrateHull(t, "echo '{\"id\":\"s\",\"migration\":{\"state\":\"migrated\",\"txid\":\"tx1\",\"peer\":\"me@box\"}}' > RECORD; echo 'moved s to me@box'")
	if _, err := h.Migrate("s", "me@box", MigrateSpec{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	res, err := h.Migrate("s", "me@box", MigrateSpec{TxID: "tx1", RemoteStore: "/st"})
	if err != nil {
		t.Fatal(err)
	}
	want := Migration{State: MigrationMigrated, TxID: "tx1", Peer: "me@box"}
	if res.Migration != want || res.RecordErr != nil || res.Summary != "moved s to me@box" {
		t.Errorf("move result %+v, want %+v", res, want)
	}
	st, err := h.MigrateStatus("s")
	if err != nil || st.Migration != want || !strings.Contains(st.Said, "migrated") {
		t.Errorf("status %+v, %v", st, err)
	}
	st, err = h.MigrateAbort("s", "tx1")
	if err != nil || st.Migration.State != MigrationAborted {
		t.Errorf("abort %+v, %v", st, err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "migrate --to me@box --dry-run s\n" +
		"migrate --to me@box --remote-store /st --txid tx1 s\ninspect s\n" +
		"migrate-status s\ninspect s\n" +
		"migrate-abort --txid tx1 s\ninspect s\n"
	if string(got) != wantLog {
		t.Errorf("hull was asked:\n%s\nwant:\n%s", got, wantLog)
	}
}

// A move hull reports as finished whose record names another move is not
// taken for this one: the record is what says where the guest runs, and a
// caller told "done" for the wrong move would record a host the session is
// not on.
func TestAMoveWhoseRecordNamesAnotherMoveSaysSo(t *testing.T) {
	h, _ := migrateHull(t, "echo '{\"id\":\"s\",\"migration\":{\"state\":\"migrated\",\"txid\":\"other\"}}' > RECORD")
	res, err := h.Migrate("s", "me@box", MigrateSpec{TxID: "tx1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecordErr == nil || !strings.Contains(res.RecordErr.Error(), "other") {
		t.Errorf("RecordErr = %v, want one naming the other move", res.RecordErr)
	}
}

// A move that failed still has its record read: a move that failed after the
// fence leaves the source unable to run, and the caller decides on that, not
// on the exit status alone.
func TestAFailedMoveStillReadsTheRecord(t *testing.T) {
	h, _ := migrateHull(t, "echo '{\"id\":\"s\",\"migration\":{\"state\":\"migrating\",\"txid\":\"tx1\"}}' > RECORD; echo 'ssh: connection reset' >&2; exit 1")
	res, err := h.Migrate("s", "me@box", MigrateSpec{TxID: "tx1"})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("a failed move came back as %v", err)
	}
	if res.Migration.State != MigrationMigrating || res.Migration.TxID != "tx1" {
		t.Errorf("record after a failed move: %+v", res.Migration)
	}
}
