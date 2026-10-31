package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A plain copy of newuidmap carries no capabilities, and neither does a path
// with nothing there.
func TestHasFileCapsOnAPlainFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newuidmap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hasFileCaps(path) {
		t.Error("a plain file reported file capabilities")
	}
	if hasFileCaps(filepath.Join(t.TempDir(), "absent")) {
		t.Error("a missing file reported file capabilities")
	}
}
