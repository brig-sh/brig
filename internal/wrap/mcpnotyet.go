package wrap

import (
	"fmt"

	"github.com/brig-sh/brig/internal/policy"
)

// refuseMCPServers refuses a run whose bound policies declare MCP servers.
// This build reads spec.mcp but does not attach the servers to the agent, and
// a run that went ahead would let you believe the agent has them.
func refuseMCPServers(bound []policy.Policy) error {
	for _, p := range bound {
		if len(p.Spec.MCP) > 0 {
			return fmt.Errorf("policy %s declares mcp servers, which this build of brig does not attach "+
				"yet; detach the policy or remove spec.mcp", p.Metadata.Name)
		}
	}
	return nil
}
