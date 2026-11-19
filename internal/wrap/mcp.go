package wrap

import (
	"fmt"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/runtime"
)

// MCP servers: the remote MCP servers the policies bound to a run declare
// under spec.mcp. The broker stands in front of each, at
// https://brig.internal/<sandbox>/mcp/<name>: it holds the credential,
// enforces the tool allowlist and audits each call. The guest holds an MCP
// token that reaches the broker and nothing else.
//
// Claude Code is pointed at them by the MCP configuration it shares with
// agent memory, so only a run that memoryWired accepts takes them.

// mcpConfigPath is where the guest finds the MCP configuration that points
// Claude Code at its memory store and its MCP servers: beside the brig CA,
// outside the guest home, root's and 0644. It holds no token. See
// deliverMCPConfig.
const mcpConfigPath = "/brig/mcp/mcp.json"

// mcpTokenVar holds the MCP token in the guest's environment. The MCP
// configuration names it, and Claude Code expands it when it reads the file,
// so the token is never written into the guest.
const mcpTokenVar = "BRIG_MCP_TOKEN"

// MCPConfig is the guest path of the MCP configuration of this run, or ""
// when it has neither memory nor an MCP server.
func (c *Config) MCPConfig() string {
	if c.memoryKey == "" && len(c.mcp) == 0 {
		return ""
	}
	return mcpConfigPath
}

// mcpList names each policy with its MCP servers, the way a refusal does:
// "policy a (mcp x)" or "policies a (mcp x), b (mcp y)". plural says which,
// so the refusal's verb agrees.
func (c *Config) mcpList() (list string, plural bool) {
	byPolicy := map[string][]string{}
	var order []string
	for _, e := range c.mcp {
		if _, ok := byPolicy[e.Policy]; !ok {
			order = append(order, e.Policy)
		}
		byPolicy[e.Policy] = append(byPolicy[e.Policy], e.Server.Name)
	}
	var out []string
	for _, p := range order {
		out = append(out, fmt.Sprintf("%s (mcp %s)", p, strings.Join(byPolicy[p], ", ")))
	}
	if len(out) > 1 {
		return "policies " + strings.Join(out, ", "), true
	}
	return "policy " + strings.Join(out, ", "), false
}

// mcpRefusal is why this run cannot have the MCP servers its policies
// declare, or nil. Read-only, because Plan calls it through sessionRefusal.
//
// Only claude-code is pointed at the servers. Any other agent would get a
// token, an isolated network and no server, so the run is refused rather
// than booted without what its policy names. The guest reaches the servers
// at brig.internal, so the run needs a run path that routes that name to the
// broker, as memory does.
func (c *Config) mcpRefusal() error {
	if c.mcpErr != nil {
		return fmt.Errorf("the policies bound to %s cannot apply together: %w", c.subject(), c.mcpErr)
	}
	if len(c.mcp) == 0 {
		return nil
	}
	list, plural := c.mcpList()
	declares, wayOut := "declares", "detach the policy"
	if plural {
		declares, wayOut = "declare", "detach the policies"
	}
	if !c.memoryWired() {
		return fmt.Errorf("%s %s MCP servers, which brig serves only to claude-code, so %s "+
			"would get none; run claude-code, or %s", list, declares, c.memoryAgent(), wayOut)
	}
	if c.Runtime == nil {
		return nil
	}
	return c.brigInternalRefusal("each MCP server of "+list, "MCP servers run", wayOut)
}

// forgetMCPOnly revokes the MCP tokens of this sandbox and keeps its routes
// and its memory registered. A failure is said and does not fail the exec.
func (c *Config) forgetMCPOnly() {
	if err := c.routeBroker().ForgetMCP(c.VMName); err != nil {
		c.warnf("the credential broker could not revoke the MCP tokens of %s: %v", c.VMName, err)
	}
}

// serveMCP registers this sandbox's MCP servers with the broker, mints an
// MCP token for this exec, and records what the guest is handed. Called on
// every exec, after serveMemory: a broker that started again since the last
// exec has lost the registration, the same registration again keeps the
// token of an agent already running valid, and another set of servers
// replaces the one before.
//
// The routes, the memory and the MCP servers of a sandbox share its divert
// socket and the broker's CA, so each is checked against what the gateway
// was handed and what the others took. See acceptBrigInternal.
func (c *Config) serveMCP() error {
	c.mcpVars = nil
	if len(c.mcp) == 0 {
		return nil
	}
	addr, err := c.guestBrigInternal()
	if err != nil {
		return err
	}
	b := c.routeBroker()
	reg := injector.MCPSandbox{ID: c.VMName, Profile: c.brokerProfile(), Session: c.RawName, BrigInternal: addr}
	for _, e := range c.mcp {
		reg.Servers = append(reg.Servers, injector.MCPServer{Policy: e.Policy, Server: e.Server})
	}
	added, err := b.RegisterMCP(reg)
	if err != nil {
		return fmt.Errorf("could not register the MCP servers of %s with the credential broker: %w", c.VMName, err)
	}
	if err := c.acceptBrigInternal("the MCP servers", "its routes or memory", added); err != nil {
		return err
	}
	token, err := b.MintMCP(injector.MCPMintRequest{
		Sandbox:    c.VMName,
		Session:    c.RawName,
		TTLSeconds: int64(injector.TokenTTL / time.Second),
	})
	if err != nil {
		return fmt.Errorf("could not mint an MCP token for %s: %w", c.VMName, err)
	}
	// Secret, so the token never goes on a command line.
	c.mcpVars = []runtime.Var{{Name: mcpTokenVar, Value: token, Secret: true}}
	// A run with an endpoint route or memory has the CA variables from
	// those.
	if len(c.endpointRoutes()) == 0 && c.memoryKey == "" {
		c.mcpVars = append(c.mcpVars, injector.CAVars()...)
	}
	return nil
}
