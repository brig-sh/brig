// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// When the meta file for the continuation cannot be written, the file on disk
// still names the old recording, so the uploader must not move to the new one.
// The run is not stopped and the spool stays for a flush.
func TestAContinuationWhoseMetaCannotBeWrittenIsNotSwitchedTo(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	old := r.s.up.RecordingID
	oldMeta := r.s.meta.RecordingID
	// A directory with a file in it cannot be replaced by the rename.
	mp := r.s.paths[1]
	if err := os.Remove(mp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mp, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := r.tick()
	if !errors.Is(err, errGiveUp) {
		t.Fatalf("tick = %v, want errGiveUp", err)
	}
	if r.s.up.RecordingID != old || r.s.meta.RecordingID != oldMeta {
		t.Errorf("the uploader points at %q and the meta at %q, want %q and %q", r.s.up.RecordingID, r.s.meta.RecordingID, old, oldMeta)
	}
	if r.s.stopped {
		t.Error("the run was stopped")
	}
	if _, serr := os.Stat(r.s.paths[0]); serr != nil {
		t.Errorf("the spool is gone: %v", serr)
	}
}
