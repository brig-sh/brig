package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Brig Community has no login and records nothing. A recorder is Brig
// Enterprise code, so the Community binary must not link it, or the code that
// would carry a person's terminal output off the machine ships to people who
// never agreed to it. This asserts the packages, not the behaviour: a
// recorder that is not linked cannot run.
//
// The core never imports enterprise/ (Go would not resolve it from this
// module), and the compression library the recorder frames its upload with
// is a dependency of enterprise/ alone. A package whose path says "recording"
// or "ptyrelay" is the recorder or its terminal relay under any name that
// directory could be moved to.
func TestCommunityLinksNoRecorder(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", "list", "-deps", "github.com/brig-sh/brig/cmd/brig")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps (GOOS=%s): %v", goos, err)
		}
		pkgs := strings.Fields(string(out))
		if len(pkgs) < 50 {
			t.Fatalf("go list -deps (GOOS=%s) named only %d packages: %q", goos, len(pkgs), out)
		}
		for _, p := range pkgs {
			switch {
			case strings.HasPrefix(p, "github.com/brig-sh/brig/enterprise"):
				t.Errorf("GOOS=%s: cmd/brig links %s, a Brig Enterprise package", goos, p)
			case strings.HasPrefix(p, "github.com/klauspost/compress"):
				t.Errorf("GOOS=%s: cmd/brig links %s, the recorder's compression", goos, p)
			case strings.Contains(p, "recording"), strings.Contains(p, "ptyrelay"):
				t.Errorf("GOOS=%s: cmd/brig links %s, which is recorder code", goos, p)
			}
		}
	}
}
