package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubBin writes a stand-in runtime binary and returns its path.
func stubBin(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	path := filepath.Join(t.TempDir(), "rt")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// brokenBin is a runtime that fails whatever it is asked, which is the shape of
// a broken install, a permission error, or a daemon that is not up. The whole
// point of #49 is that this is not the same event as "no sandbox is running".
func brokenBin(t *testing.T) string {
	t.Helper()
	return stubBin(t, "#!/bin/sh\necho 'cannot connect to the daemon' >&2\nexit 1\n")
}

// A runtime that could not be asked must say so. Reported as "not running", it
// makes EnsureRunning boot a second sandbox onto a workspace the first is still
// holding.
func TestHullRunningReportsARuntimeThatCannotAnswer(t *testing.T) {
	h := &hull{bin: brokenBin(t)}

	running, err := h.Running("brig-claude-code")
	if err == nil {
		t.Fatal("a runtime that failed to answer reported no error, which reads as 'not running'")
	}
	if running {
		t.Error("a runtime that failed to answer must not report the sandbox as up")
	}
	// The runtime's own explanation is the only account of what went wrong, so
	// it has to survive into the error.
	if !strings.Contains(err.Error(), "cannot connect to the daemon") {
		t.Errorf("the runtime's explanation was dropped: %v", err)
	}
}

// A binary that is not there at all is the same class of event: nothing was
// asked, so nothing was answered.
func TestHullRunningReportsAMissingBinary(t *testing.T) {
	h := &hull{bin: filepath.Join(t.TempDir(), "not-installed")}

	if _, err := h.Running("brig-claude-code"); err == nil {
		t.Fatal("a missing runtime binary reported no error")
	}
}

// The other two answers keep working. A stopped instance still holds its name
// and must not read as running, and an absent one is a plain no with no error
// on it -- that is what lets a first boot happen at all.
func TestHullRunningTellsUpFromStoppedAndAbsent(t *testing.T) {
	bin := stubBin(t, "#!/bin/sh\ncat <<'EOF'\nNAME STATE\n"+
		"brig-other running\nbrig-stopped stopped\nEOF\n")
	h := &hull{bin: bin}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"brig-other", true},
		{"brig-stopped", false},
		{"brig-absent", false},
	} {
		running, err := h.Running(tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if running != tc.want {
			t.Errorf("Running(%q) = %v, want %v", tc.name, running, tc.want)
		}
	}
}

func TestNerdctlRunningReportsARuntimeThatCannotAnswer(t *testing.T) {
	n := &nerdctl{bin: brokenBin(t)}

	running, err := n.Running("brig-claude-code")
	if err == nil {
		t.Fatal("a runtime that failed to answer reported no error, which reads as 'not running'")
	}
	if running {
		t.Error("a runtime that failed to answer must not report the sandbox as up")
	}
	if !strings.Contains(err.Error(), "cannot connect to the daemon") {
		t.Errorf("the runtime's explanation was dropped: %v", err)
	}
}

func TestNerdctlRunningReportsAMissingBinary(t *testing.T) {
	n := &nerdctl{bin: filepath.Join(t.TempDir(), "not-installed")}

	if _, err := n.Running("brig-claude-code"); err == nil {
		t.Fatal("a missing runtime binary reported no error")
	}
}

// The filter already restricts the listing to running containers, so an empty
// answer is the genuine no this adapter reports.
func TestNerdctlRunningTellsUpFromAbsent(t *testing.T) {
	up := &nerdctl{bin: stubBin(t, "#!/bin/sh\necho brig-claude-code\n")}
	running, err := up.Running("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Error("a listed sandbox was not reported as running")
	}

	absent := &nerdctl{bin: stubBin(t, "#!/bin/sh\nexit 0\n")}
	running, err = absent.Running("brig-claude-code")
	if err != nil {
		t.Fatalf("an empty listing is a plain no, not a failure: %v", err)
	}
	if running {
		t.Error("an absent sandbox was reported as running")
	}
}
