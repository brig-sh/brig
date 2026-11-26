// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/ptyrelay"
)

// When the relay did not start the command, the recording is closed with one
// request, its files are removed even though the deployment does not answer
// the close, and the person sees only that the run is not recorded after all.
func TestARunTheRelayDidNotStartLeavesNoFilesAndOneLine(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	// The deployment answers every close with a server error. The old path
	// retried it and kept the spool.
	statuses := make([]int, 100000)
	for i := range statuses {
		statuses[i] = 500
	}
	r.f.FailNextCloses(statuses...)
	start := time.Now()
	_, err := r.s.Run([]string{"/nonexistent/brig-recording-test"}, os.Environ())
	if !errors.Is(err, ptyrelay.ErrNotStarted) {
		t.Fatalf("Run = %v, want ErrNotStarted", err)
	}
	if took := time.Since(start); took > 2*OpenTimeout {
		t.Errorf("Run took %v", took)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
	want := "brig: this run is not recorded after all: Brig could not set up the recorder\n"
	if r.notice.String() != want {
		t.Errorf("lines %q, want only %q", r.notice, want)
	}
}

// The close request carries the end reason recorder_error.
func TestARunTheRelayDidNotStartClosesTheRecordingOnce(t *testing.T) {
	r := newRun(t)
	if _, err := r.s.Run([]string{"/nonexistent/brig-recording-test"}, os.Environ()); !errors.Is(err, ptyrelay.ErrNotStarted) {
		t.Fatalf("Run = %v", err)
	}
	keys := r.f.Keys()
	if len(keys) != 1 {
		t.Fatalf("recordings %v", keys)
	}
	if rec := r.f.Recording(keys[0]); !rec.Closed || rec.End.EndReason != "recorder_error" {
		t.Errorf("recording closed %v with %+v", rec.Closed, rec.End)
	}
}
