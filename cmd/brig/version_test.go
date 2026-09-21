package main

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// brig version names the build the binary came from, not a stamped string:
// the version Go derived from the nearest tag, then the commit and the rest
// of the build in parentheses. Both spellings print the same line.
func TestVersionPrintsTheBuild(t *testing.T) {
	want := "brig " + buildinfo.Read().String() + "\n"
	for _, verb := range []string{"version", "--version"} {
		out, err := captureStdout(t, func() error { return run([]string{verb}) })
		if err != nil {
			t.Fatalf("brig %s: %v", verb, err)
		}
		if out != want {
			t.Errorf("brig %s printed %q, want %q", verb, out, want)
		}
		if !strings.HasPrefix(out, "brig ") || !strings.Contains(out, " (") {
			t.Errorf("brig %s printed %q, want `brig <version> (<build>)`", verb, out)
		}
	}
}
