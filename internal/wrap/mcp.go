package wrap

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// MCP servers: the remote MCP servers the policies bound to a run declare
// under spec.mcp. The broker stands in front of each, at
// https://brig.internal/<sandbox>/mcp/<name>: it holds the credential,
// enforces the tool allowlist and audits each call. The guest holds an MCP
// token that reaches the broker and nothing else.
//
// They are a guest service of kind mcp, so the run takes the posture and the
// divert of every service at brig.internal. Claude Code is pointed at them
// by the guest's MCP configuration, so only a run that mcpWired accepts
// takes them.

// mcpKind is the guest service kind of the MCP servers, under which the
// broker registers and revokes them.
const mcpKind = "mcp"

// mcpTokenVar holds the MCP token in the guest's environment. The MCP
// configuration names it, and Claude Code expands it when it reads the file,
// so the token is never written into the guest.
const mcpTokenVar = "BRIG_MCP_TOKEN"

// brokerProcess serves MCP servers as it serves routes, from the broker
// directory under ~/.brig/host.
func (b *brokerProcess) RegisterMCP(s injector.MCPSandbox) (injector.AddResponse, error) {
	dir, err := injector.Dir()
	if err != nil {
		return injector.AddResponse{}, err
	}
	running, added, err := injector.RegisterMCP(dir, s)
	if err != nil {
		return injector.AddResponse{}, err
	}
	b.client = &running.Client
	return added, nil
}

func (b *brokerProcess) MintMCP(r injector.MCPMintRequest) (string, error) {
	if b.client == nil {
		return "", injector.ErrNotRunning
	}
	return b.client.MintMCP(r)
}

// brokeredMCPSecrets are the secrets the credentials of MCP servers name, by
// secret, with the server that names each. Each stays on the host as a
// route's does: the broker sends it upstream, and a copy in the guest would
// reach the server around the tool allowlist.
func brokeredMCPSecrets(servers []policy.MCPEntry) map[string]string {
	out := map[string]string{}
	for _, e := range servers {
		if c := e.Server.Credential; c != nil && c.SecretRef.Name != "" {
			out[c.SecretRef.Name] = e.Server.Name
		}
	}
	return out
}

// addMCPServers adds each MCP server of this run to the guest's MCP
// configuration, at its path under brig.internal, with the token in
// mcpTokenVar. Two entries under one name are a refusal of the run, kept in
// mcpErr, rather than one server left out.
func (c *Config) addMCPServers() {
	if c.mcpErr != nil {
		return
	}
	base := injector.HostEndpoint{}.Base() + "/" + c.VMName + "/" + mcpKind + "/"
	for _, e := range c.mcp {
		if err := c.addMCPEntry(MCPEntry{Name: e.Server.Name, URL: base + e.Server.Name, TokenVar: mcpTokenVar}); err != nil {
			c.mcpErr = err
			return
		}
	}
}

// mcpWired reports whether brig points this run's agent at its MCP servers,
// by the rule agentArgs in internal/cli adds --mcp-config under: the
// claude-code profile, or a target whose Brigfile runs the claude-code brig
// ships. A repository can call its own profile claude-code, so a target's
// name says nothing.
func (c *Config) mcpWired() bool {
	if c.target != nil {
		return c.target.ProfileSource == "builtin:claude-code"
	}
	return c.Profile.Name == "claude-code"
}

// mcpAgent names the agent of this run in an MCP message: the profile, and
// for a target where the Brigfile took it from.
func (c *Config) mcpAgent() string {
	if c.target != nil {
		return "the profile " + c.Profile.Name + " (" + c.target.ProfileSource + ")"
	}
	return "the profile " + c.Profile.Name
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
// broker, as every guest service does.
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
	if !c.mcpWired() {
		return fmt.Errorf("%s %s MCP servers, which brig serves only to claude-code, so %s "+
			"would get none; run claude-code, or %s", list, declares, c.mcpAgent(), wayOut)
	}
	return c.brigInternalRefusal("each MCP server of " + list)
}

// forgetMCPOnly revokes the MCP token of this sandbox and keeps its routes
// registered, so the session tokens of an agent another exec started stay
// valid. A failure is said and does not fail the exec.
func (c *Config) forgetMCPOnly() {
	if err := c.routeBroker().ForgetService(mcpKind, c.VMName); err != nil {
		c.warnf("the credential broker could not revoke the MCP token of %s: %v", c.VMName, err)
	}
}

// serveMCP registers this sandbox's MCP servers with the broker, mints an
// MCP token for this exec, and records what the guest is handed. Called on
// every exec, after serveRoutes, which registers no route for a run with
// none: a broker that started again since the last exec has lost the
// registration, and the same registration again keeps the token of an agent
// already running valid.
//
// Another set of servers than the one registered is refused by the broker,
// since a token already minted would reach them. The registration before is
// removed, which revokes that token, and this one takes its place.
//
// The routes and the MCP servers of a sandbox share its divert socket and
// the broker's CA, so the answer is checked against what the gateway was
// handed and what serveRoutes took.
func (c *Config) serveMCP() error {
	c.mcpVars = nil
	if len(c.mcp) == 0 {
		return nil
	}
	router, ok := c.Runtime.(runtime.BrigInternalRouter)
	if !ok {
		return fmt.Errorf("the runtime %s cannot say where its guest reaches %s", c.Runtime.Kind(),
			injector.BrigInternal)
	}
	addr, err := router.BrigInternalAddr(c.VMName)
	if err != nil {
		return fmt.Errorf("could not find where the guest of %s reaches %s: %w", c.VMName,
			injector.BrigInternal, err)
	}
	b := c.routeBroker()
	reg := injector.MCPSandbox{ID: c.VMName, Profile: c.brokerProfile(), Session: c.RawName,
		BrigInternal: addr.String()}
	for _, e := range c.mcp {
		reg.Servers = append(reg.Servers, injector.MCPServer{Policy: e.Policy, Server: e.Server})
	}
	added, err := b.RegisterMCP(reg)
	var ref *injector.RefusedError
	if errors.As(err, &ref) && ref.Status == http.StatusConflict {
		if err := b.ForgetService(mcpKind, c.VMName); err != nil {
			return fmt.Errorf("could not remove the MCP servers %s was registered with before: %w", c.VMName, err)
		}
		added, err = b.RegisterMCP(reg)
	}
	if err != nil {
		return fmt.Errorf("could not register the MCP servers of %s with the credential broker: %w", c.VMName, err)
	}
	// The gateway is handed the socket before the broker answers, as for a
	// route. A broker that listens elsewhere would leave the guest's
	// connections reset.
	if want := c.diverts(); !want.Requested() || added.DivertSocket != want.Socket {
		return fmt.Errorf("the credential broker listens for the diverted connections of %s on %q, "+
			"and brig expected %q; stop the broker with `brig stop` on every sandbox and run again",
			c.VMName, added.DivertSocket, want.Socket)
	}
	ca, err := checkBrigCA(added.CA)
	if err != nil {
		return fmt.Errorf("the credential broker handed over a brig CA that brig will not put in the "+
			"guest: %w", err)
	}
	// One broker makes one CA. Another CA here means two brokers answered
	// the registrations of one sandbox, and the guest would trust one and
	// be served by the other.
	if c.brigCA != "" && c.brigCA != ca {
		return fmt.Errorf("the credential broker handed the MCP servers of %s a different brig CA from "+
			"its routes; stop the broker with `brig stop` on every sandbox and run again", c.VMName)
	}
	c.brigCA = ca
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
	// A run with an endpoint route has the CA variables from its routes.
	if len(c.endpointRoutes()) == 0 {
		c.mcpVars = append(c.mcpVars, injector.CAVars()...)
	}
	return nil
}

// mcpNames are the names of this run's MCP servers, for the boot's line.
func (c *Config) mcpNames() string {
	names := make([]string, len(c.mcp))
	for i, e := range c.mcp {
		names[i] = e.Server.Name
	}
	return strings.Join(names, ", ")
}
