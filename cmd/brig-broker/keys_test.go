package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key file of the wrong length is refused by the name of the key it is, so
// an operator with several key files knows which one to remove.
func TestLoadOrCreateKeyNamesWhatItRefuses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "echo.key")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrCreateKey(p, "echo key")
	if err == nil {
		t.Fatal("a 3-byte key file was accepted")
	}
	for _, want := range []string{"refusing the echo key", "is 3 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
