package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The checkpoint line names the bundle's place and what the guest does after
// it only when asked, so hull's own defaults hold otherwise, and the name is
// last, after every flag, where hull's parser takes it.
func TestCheckpointArgs(t *testing.T) {
	for _, c := range []struct {
		what string
		spec CheckpointSpec
		want string
	}{
		{"hull's defaults", CheckpointSpec{}, "checkpoint s"},
		{"a fence", CheckpointSpec{Then: CheckpointFence}, "checkpoint --then fence s"},
		{"a continue into a directory", CheckpointSpec{Then: CheckpointContinue, Dir: "/b/1"},
			"checkpoint --then continue --dir /b/1 s"},
		{"a timeout, rounded up", CheckpointSpec{Timeout: 1500 * time.Millisecond}, "checkpoint --timeout 2 s"},
	} {
		got, err := checkpointArgs("s", c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q, want %q", c.what, strings.Join(got, " "), c.want)
		}
	}
}

// A then hull does not know, or a directory hull would resolve against its
// own working directory, is refused before hull is asked: the guest is about
// to be paused, and a typo must not be what pauses it.
func TestCheckpointArgsRefuseWhatHullWouldMisread(t *testing.T) {
	for _, spec := range []CheckpointSpec{
		{Then: "stop"},
		{Then: "Fence"},
		{Dir: "bundles/1"},
	} {
		if got, err := checkpointArgs("s", spec); err == nil {
			t.Errorf("%+v was accepted as %q", spec, got)
		}
	}
}

// A restore is always detached: brig hands the restored sandbox back to its
// own commands and never holds the terminal for the VMM.
func TestRestoreArgs(t *testing.T) {
	for _, c := range []struct {
		spec RestoreSpec
		want string
	}{
		{RestoreSpec{}, "restore --detach s"},
		{RestoreSpec{Paused: true}, "restore --detach --paused s"},
		{RestoreSpec{Paused: true, Timeout: 2 * time.Minute}, "restore --detach --paused --timeout 120 s"},
	} {
		if got := strings.Join(restoreArgs("s", c.spec), " "); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.spec, got, c.want)
		}
	}
}

// The migration record is read from what `hull inspect` prints. No record is
// the zero value, not an error: an ordinary sandbox has none, and brig info
// must show nothing for it rather than fail.
func TestMigrationOf(t *testing.T) {
	for _, c := range []struct {
		what   string
		record string
		want   Migration
	}{
		{"no record", `{"id":"s","cmdLine":["hvi","boot"]}`, Migration{}},
		{"a null record", `{"id":"s","migration":null}`, Migration{}},
		{"a fenced checkpoint", `{"id":"s","migration":{"state":"fenced","bundleDir":"/b","txid":"t1"}}`,
			Migration{State: "fenced", BundleDir: "/b", TxID: "t1"}},
		{"a field this brig does not know", `{"id":"s","migration":{"state":"restored","peer":"h"}}`,
			Migration{State: "restored"}},
	} {
		got, err := migrationOf("s", []byte(c.record))
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.what, got, c.want)
		}
	}
	for _, record := range []string{`not json`, `{"id":"other","migration":{"state":"fenced"}}`} {
		if got, err := migrationOf("s", []byte(record)); err == nil {
			t.Errorf("%s was read as %+v", record, got)
		}
	}
}

// checkpointHull is a hull that logs every argv it is given, checkpoints by
// recording a migration its inspect then reports, and refuses a resume the
// way hull refuses one for a fenced guest.
func checkpointHull(t *testing.T) (*hull, string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1\" in\n" +
		"  checkpoint) echo '{\"id\":\"s\",\"migration\":{\"state\":\"fenced\",\"bundleDir\":\"/b/t1\",\"txid\":\"t1\"}}' > " + dir + "/record\n" +
		"    echo 'Checkpoint of s written to /b/t1' ;;\n" +
		"  inspect) cat " + dir + "/record 2>/dev/null || echo '{\"id\":\"s\"}' ;;\n" +
		"  resume) echo 'instance s was fenced by `hull checkpoint --then fence`' >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin}, log
}

// The seam drives hull with the lines the builders make, reads the record
// hull left for where the bundle went, and hands back hull's own refusal.
func TestHullCheckpointSeam(t *testing.T) {
	h, log := checkpointHull(t)
	if m, err := h.Migration("s"); err != nil || m != (Migration{}) {
		t.Fatalf("before a checkpoint: %+v, %v; want no record", m, err)
	}
	if err := h.Pause("s"); err != nil {
		t.Fatal(err)
	}
	res, err := h.Checkpoint("s", CheckpointSpec{Then: CheckpointFence})
	if err != nil {
		t.Fatal(err)
	}
	want := Migration{State: "fenced", BundleDir: "/b/t1", TxID: "t1"}
	if res.Migration != want || res.Summary != "Checkpoint of s written to /b/t1" {
		t.Errorf("checkpoint result %+v, want %+v and hull's line", res, want)
	}
	if err := h.Resume("s"); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Errorf("a resume hull refused came back as %v", err)
	}
	if err := h.Restore("s", RestoreSpec{Paused: true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "inspect s\npause s\ncheckpoint --then fence s\ninspect s\nresume s\nrestore --detach --paused s\n"
	if string(got) != wantLog {
		t.Errorf("hull was asked:\n%s\nwant:\n%s", got, wantLog)
	}
}
