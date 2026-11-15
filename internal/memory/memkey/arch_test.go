package memkey

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Profile and Brigfile loading import this package to read a memory setting.
// They must not link a network server or a process launcher, so the
// dependency is what is asserted: a behavioural test cannot show that code is
// absent.
func TestMemkeyLinksNoNetworkOrExec(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", "list", "-deps", "github.com/brig-sh/brig/internal/memory/memkey")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps: %v", goos, err)
		}
		deps := strings.Split(string(out), "\n")
		for _, banned := range []string{"net/http", "net", "os/exec"} {
			if slices.Contains(deps, banned) {
				t.Errorf("GOOS=%s: internal/memory/memkey links %s", goos, banned)
			}
		}
	}
}
