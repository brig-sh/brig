//go:build darwin || linux

package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// At the deadline the stack tool is asked to stop, so it can detach, and the
// VMM it may have left stopped is sent SIGCONT.
func TestStackToolDetachesAndResumesTheVMM(t *testing.T) {
	resumed := 0
	old := resumeVMM
	resumeVMM = func(pid int) { resumed = pid }
	t.Cleanup(func() { resumeVMM = old })
	tool := filepath.Join(t.TempDir(), "tracer")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ntrap 'kill $!; echo detached; exit 0' TERM\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Long enough for the shell to set its trap on a loaded machine.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	out, _ := stackTool(ctx, 4242, tool)
	if time.Since(start) > stackGrace {
		t.Errorf("the tool took %s to stop; SIGTERM was not what stopped it", time.Since(start))
	}
	if !strings.Contains(string(out), "detached") {
		t.Errorf("tool output %q: it was not sent SIGTERM", out)
	}
	if resumed != 4242 {
		t.Errorf("resumed pid %d, want the VMM 4242", resumed)
	}
}

// A tool that finishes on its own has detached already; the VMM is left as
// it is.
func TestStackToolLeavesAFinishedVMMAlone(t *testing.T) {
	resumed := false
	old := resumeVMM
	resumeVMM = func(int) { resumed = true }
	t.Cleanup(func() { resumeVMM = old })
	if _, err := stackTool(context.Background(), 4242, "true"); err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Error("the VMM was sent SIGCONT after a tool that finished")
	}
}
