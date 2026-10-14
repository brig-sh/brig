package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BRIG_HULL_STORE_DIR goes in front of every hull verb that opens the store,
// and nowhere when unset.
func TestHullArgsNameTheStoreBeforeTheVerb(t *testing.T) {
	t.Setenv(HullStoreDir, "")
	if got := strings.Join(hullArgs("ps", "-a"), " "); got != "ps -a" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv(HullStoreDir, "/st/b")
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &hull{bin: bin}
	_, _ = h.Running("s")
	_, _ = h.Migration("s")
	_ = h.Stop("s")
	argv, _, err := h.replaceCmd(ExecSpec{Name: "s", Cmd: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv[1:4], " ") != "--store-dir /st/b exec" {
		t.Errorf("handover argv %q", argv)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if !strings.HasPrefix(line, "--store-dir /st/b ") {
			t.Errorf("hull was asked %q without the store", line)
		}
	}
}
