// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runOnPipes records `script` with the streams on files, and returns what the
// run printed at its end.
func runOnPipes(t *testing.T, p Params, script string) string {
	t.Helper()
	notice := &bytes.Buffer{}
	p.Notice = notice
	in, _ := os.Open(os.DevNull)
	p.In = in
	p.Out, _ = os.Create(filepath.Join(t.TempDir(), "out"))
	p.Err, _ = os.Create(filepath.Join(t.TempDir(), "err"))
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	if strings.Contains(script, "BREAK-SPOOL") {
		_ = s.rec.f.Close() // a spool that can no longer be written
		script = "echo hello"
	}
	if _, err := s.Run([]string{"sh", "-c", script}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	return notice.String()
}

// A run that writes more than the spool cap is recorded only up to it. The
// person is told when the run ends, and the close call carries the end reason
// spool_cap so the deployment can show the recording as cut short.
func TestARunThatReachesTheSpoolCapSaysSoAndClosesWithSpoolCap(t *testing.T) {
	f := newFake(t)
	p := beginParams(f, t.TempDir())
	p.SpoolCap = 1 << 20
	got := runOnPipes(t, p, `head -c 3000000 /dev/zero | tr '\0' x`)
	want := "brig: recording stopped part way through this run because it reached the limit of 1 MiB; the rest of its output is not recorded\n"
	if got != want {
		t.Errorf("closing lines %q, want %q", got, want)
	}
	rec := f.Recording(f.Keys()[0])
	if !rec.Closed || rec.End.EndReason != "spool_cap" || rec.End.ExitCode != nil {
		t.Errorf("close %+v, want end reason spool_cap and no exit status", rec.End)
	}
	if len(rec.Raw) > 1<<20+4096 {
		t.Errorf("the recording holds %d bytes, past the cap", len(rec.Raw))
	}
}

func TestARunUnderTheSpoolCapPrintsNoCapLine(t *testing.T) {
	f := newFake(t)
	p := beginParams(f, t.TempDir())
	p.SpoolCap = 1 << 20
	if got := runOnPipes(t, p, `echo small`); got != "" {
		t.Errorf("closing lines %q", got)
	}
	if rec := f.Recording(f.Keys()[0]); rec.End.EndReason != "exit" {
		t.Errorf("close %+v", rec.End)
	}
}

// A write error on the spool ends the recording, and the person is told. The
// close call carries recorder_error.
func TestARunWhoseSpoolCouldNotBeWrittenSaysSoAndClosesWithRecorderError(t *testing.T) {
	f := newFake(t)
	got := runOnPipes(t, beginParams(f, t.TempDir()), "BREAK-SPOOL")
	want := "brig: recording stopped part way through this run because Brig could not write the recording to disk; the rest of its output is not recorded\n"
	if got != want {
		t.Errorf("closing lines %q, want %q", got, want)
	}
	if rec := f.Recording(f.Keys()[0]); rec.End.EndReason != "recorder_error" || rec.End.ExitCode != nil {
		t.Errorf("close %+v", rec.End)
	}
}
