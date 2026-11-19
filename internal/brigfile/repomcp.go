package brigfile

import (
	"fmt"

	"github.com/brig-sh/brig/internal/policy"
)

// refuseMCPWithoutPreset refuses an MCP server that a repository policy
// declares without a preset.
//
// An MCP server sends one of your secrets to its URL. A repository that set
// the URL, or the inject that carries the secret, would choose where your
// secret goes: a policy that names nofire-token and an upstream of its own
// would hand your token to whoever wrote the repository. A preset fixes the
// destination, and ExpandMCP refuses a server that names one and sets url or
// inject, so a repository policy may name a preset and nothing else. Your own
// policy directory is not held to this.
func refuseMCPWithoutPreset(p policy.Policy) error {
	for _, s := range p.Spec.MCP {
		if s.Preset == "" {
			return fmt.Errorf("policy %s declares mcp server %s without a preset; a repository may attach "+
				"an MCP server only through a preset, such as preset: nofire", p.Metadata.Name, s.Name)
		}
	}
	return nil
}
