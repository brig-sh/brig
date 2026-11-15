package memory

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The memory package serves a directory the guest writes into. It must not be
// able to reach a credential or a session token, so the dependency is what is
// asserted: a behavioural test cannot show that code is absent.
func TestMemoryLinksNoSecretsOrTokens(t *testing.T) {
	out, err := goList("-deps", "github.com/brig-sh/brig/internal/memory")
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(string(out), "\n")
	for _, banned := range []string{"broker", "secret", "sessiontoken", "wrap"} {
		if slices.Contains(deps, "github.com/brig-sh/brig/internal/"+banned) {
			t.Errorf("internal/memory depends on internal/%s", banned)
		}
	}
}

// goList runs go list for both host platforms, since brig ships for both and
// a dependency may sit behind a build constraint.
func goList(args ...string) ([]byte, error) {
	var all []byte
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", append([]string{"list"}, args...)...)
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("GOOS=%s: %w", goos, err)
		}
		all = append(all, out...)
	}
	return all, nil
}
