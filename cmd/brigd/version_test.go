package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// The version op names the build the daemon came from, the same fields brig
// prints for itself, so a client can tell a daemon left running across an
// upgrade from the binary that is talking to it.
func TestVersionOpReportsTheBuild(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	info := buildinfo.Read()
	resp := ask(t, socket, `{"op":"version"}`)
	if !resp.OK {
		t.Fatalf("version refused: %+v", resp)
	}
	if resp.Version != info.Version || resp.Commit != info.Commit || resp.Modified == nil || *resp.Modified != info.Modified {
		t.Errorf("version op = %+v, want the build %+v", resp, info)
	}
	if info.CommitTime.IsZero() {
		if resp.CommitTime != "" {
			t.Errorf("commitTime = %q, want it absent for a build without VCS", resp.CommitTime)
		}
		return
	}
	if want := info.CommitTime.UTC().Format(time.RFC3339); resp.CommitTime != want {
		t.Errorf("commitTime = %q, want %q", resp.CommitTime, want)
	}
}

// modified is on the version op's answer whether it is true or false, as it is
// in `brig version --json`, and on no other op's.
func TestModifiedIsOnTheVersionOpOnly(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	if _, ok := askJSON(t, socket, `{"op":"version"}`)["modified"]; !ok {
		t.Errorf("version op carries no modified")
	}
	if got, ok := askJSON(t, socket, `{"op":"status"}`)["modified"]; ok {
		t.Errorf("status op carries modified = %v", got)
	}
}
