package wrap

import (
	"encoding/json"
	"fmt"
	pathpkg "path"
	"slices"
)

// GuestMCPConfigPath is the guest's MCP configuration: one file that every
// service this run serves over MCP adds its server to, so the agent is
// handed one path whatever the services are.
const GuestMCPConfigPath = "/brig/mcp/mcp.json"

// MCPEntry is one MCP server in the guest's configuration. TokenVar names
// the guest variable that holds its token: the agent expands it from its
// environment when it reads the file, so the file holds no secret and is the
// same on every exec.
type MCPEntry struct {
	Name, URL, TokenVar string
}

// addMCPEntry adds e to the guest's MCP configuration. A second entry under
// one name is refused rather than left to replace the first.
func (c *Config) addMCPEntry(e MCPEntry) error {
	if slices.ContainsFunc(c.mcpEntries, func(o MCPEntry) bool { return o.Name == e.Name }) {
		return fmt.Errorf("two MCP servers are named %s", e.Name)
	}
	c.mcpEntries = append(c.mcpEntries, e)
	return nil
}

// mcpConfig is the JSON of the guest's MCP configuration.
func (c *Config) mcpConfig() (string, error) {
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	servers := map[string]server{}
	for _, e := range c.mcpEntries {
		servers[e.Name] = server{
			Type:    "http",
			URL:     e.URL,
			Headers: map[string]string{"Authorization": "Bearer ${" + e.TokenVar + "}"},
		}
	}
	b, err := json.Marshal(struct {
		MCPServers map[string]server `json:"mcpServers"`
	}{servers})
	return string(b), err
}

// deliverMCPConfig writes the guest's MCP configuration to
// GuestMCPConfigPath, for a run with an entry, the way deliverBrigCA writes
// the CA: root's and 0644 in a directory of root's, outside the guest home,
// on stdin, on every exec. The agent reads it and cannot change it, so it
// cannot point itself at another server with a token.
func (c *Config) deliverMCPConfig() error {
	if len(c.mcpEntries) == 0 {
		return nil
	}
	body, err := c.mcpConfig()
	if err != nil {
		return err
	}
	dir := pathpkg.Dir(GuestMCPConfigPath)
	if err := c.guestRoot("sh", "-c", guestFileCreateScript, "sh", dir, GuestMCPConfigPath); err != nil {
		return fmt.Errorf("could not create %s in the sandbox: %w", GuestMCPConfigPath, err)
	}
	return c.feedGuestFile(GuestMCPConfigPath, body)
}

// MCPConfig is the guest path of this run's MCP configuration, or "" when no
// service added an entry to it.
func (c *Config) MCPConfig() string {
	if len(c.mcpEntries) == 0 {
		return ""
	}
	return GuestMCPConfigPath
}
