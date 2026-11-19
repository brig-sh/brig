package mcpproxy_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The broker calls this package, and the memory server is a separate MCP
// surface with its own token. A dependency on either would let the rules of one
// leak into the other, while every behavioural test still passed.
func TestMCPProxyImportsNoBrokerOrMemory(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", "list", "-deps", "github.com/brig-sh/brig/internal/mcpproxy")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps: %v", goos, err)
		}
		for _, dep := range strings.Split(string(out), "\n") {
			for _, banned := range []string{"internal/broker", "internal/memory"} {
				if strings.HasSuffix(dep, "brig/"+banned) {
					t.Errorf("GOOS=%s: internal/mcpproxy depends on %s", goos, dep)
				}
			}
		}
	}
}
