package wrap

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
)

func TestRunRefusesMCPServersItCannotAttach(t *testing.T) {
	egress := policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "locked-down"}}
	if err := refuseMCPServers([]policy.Policy{egress}); err != nil {
		t.Fatalf("a policy with no mcp servers: %v", err)
	}
	mcp := policy.Policy{Metadata: v1alpha1.ObjectMeta{Name: "nofire"}}
	mcp.Spec.MCP = []v1alpha1.MCPServer{{Name: "nofire", Preset: v1alpha1.MCPPresetNofire}}
	err := refuseMCPServers([]policy.Policy{egress, mcp})
	if err == nil || !strings.Contains(err.Error(), "policy nofire declares mcp servers") {
		t.Fatalf("got %v, want the refusal naming policy nofire", err)
	}
}
