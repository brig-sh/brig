package wrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// nofireMCP binds the NOFire AI MCP server through its preset, with one tool
// allowed and the credential in the host store under nofire-token.
const nofireMCP = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: nofire}
spec:
  mcp:
    - name: nofire
      preset: nofire
      credential: {secretRef: {name: nofire-token}}
      tools: {allow: [search]}
`

// nofireSecret is the value of nofire-token in the store a case hands the
// run. The broker reads it on the host, so it must reach no part of the
// guest.
const nofireSecret = "nofire-secret-value-0123456789"

// mcpSession is memorySession with the policy doc written to the policy
// directory and bound to the profile.
func mcpSession(t *testing.T, doc string) (profile.Profile, string) {
	t.Helper()
	p, home := memorySession(t)
	bindPolicy(t, &p, "nofire", doc)
	return p, home
}

// bindPolicy writes doc to the policy directory as name and binds it to p.
func bindPolicy(t *testing.T, p *profile.Profile, name, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIG_POLICY_DIR"), name+".yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Policy = []string{name}
}

// loadMCP loads p as loadMemory does, with the NOFire credential in the
// store the run opens.
func loadMCP(t *testing.T, p profile.Profile, o Options, rt runtime.Runtime) *Config {
	t.Helper()
	c := loadMemory(t, p, o, rt)
	c.OpenStore = func() (creds.SecretReader, error) {
		return fakeSecretStore{"nofire-token": nofireSecret}, nil
	}
	return c
}

// wantNofireEntry is the entry of the NOFire server in the MCP configuration
// of sandbox vm.
func wantNofireEntry(vm string) string {
	return `"nofire":{"type":"http","url":"https://brig.internal/` + vm +
		`/mcp/nofire","headers":{"Authorization":"Bearer ${BRIG_MCP_TOKEN}"}}`
}

// The MCP configuration names the token's variable and never holds the
// token, and the NOFire credential reaches neither the file, the guest's
// environment, a command run in the guest, nor the agent's command line.
func TestMCPConfigHoldsNoToken(t *testing.T) {
	p, home := mcpSession(t, nofireMCP)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	if len(broker.mcpRegistered) != 1 {
		t.Fatalf("registered for mcp %+v, want one registration", broker.mcpRegistered)
	}
	reg := broker.mcpRegistered[0]
	if reg.ID != c.VMName || reg.Profile != "claude-code" || reg.BrigInternal != "198.18.1.1" ||
		len(reg.Servers) != 1 || reg.Servers[0].Policy != "nofire" || reg.Servers[0].Server.Name != "nofire" {
		t.Errorf("registered %+v", reg)
	}
	if len(broker.mcpMinted) != 1 || broker.mcpMinted[0].Sandbox != c.VMName || broker.mcpMinted[0].TTLSeconds <= 0 {
		t.Errorf("minted %+v", broker.mcpMinted)
	}
	token := "mcp-token-" + c.VMName
	if !slices.ContainsFunc(c.mcpVars, func(v runtime.Var) bool { return v.Value == token }) {
		t.Fatalf("mcpVars %v do not hold the minted token", c.mcpVars)
	}
	body, ok := rt.guestFiles[mcpConfigPath]
	if !ok {
		t.Fatalf("no %s in the guest: %v", mcpConfigPath, rt.guestFiles)
	}
	if want := `{"mcpServers":{` + wantNofireEntry(c.VMName) + `}}`; body != want {
		t.Errorf("%s holds\n%s\nwant\n%s", mcpConfigPath, body, want)
	}
	if !strings.Contains(body, "${BRIG_MCP_TOKEN}") || strings.Contains(body, token) {
		t.Errorf("the MCP configuration does not name the variable, or holds the token: %s", body)
	}
	if strings.HasPrefix(mcpConfigPath, c.Profile.GuestHome+"/") {
		t.Errorf("%s is in the guest home %s", mcpConfigPath, c.Profile.GuestHome)
	}
	spec := c.execSpec(set, []string{"claude", "--mcp-config=" + c.MCPConfig()}, false)
	for _, v := range spec.Env {
		if strings.Contains(v.Value, nofireSecret) {
			t.Errorf("the NOFire credential is in the guest variable %s", v.Name)
		}
	}
	if strings.Contains(strings.Join(spec.Cmd, " "), nofireSecret) || strings.Contains(strings.Join(spec.Cmd, " "), token) {
		t.Errorf("the agent's command line carries a secret: %q", spec.Cmd)
	}
	for _, run := range rt.guestRuns {
		if cmd := strings.Join(run.Cmd, " "); strings.Contains(cmd, nofireSecret) || strings.Contains(cmd, token) {
			t.Errorf("a command run in the guest carries a secret: %q", run.Cmd)
		}
	}
	for path, got := range rt.guestFiles {
		if strings.Contains(got, nofireSecret) {
			t.Errorf("the NOFire credential was written to %s", path)
		}
	}
}

// The MCP token goes to the guest in its environment, marked Secret, so it is
// never put on a command line. A run with no route and no memory gets the
// CA variables with it, to trust brig.internal.
func TestMCPTokenIsSecretVar(t *testing.T) {
	p, home := mcpSession(t, nofireMCP)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	env := envOf(c.execSpec(set, []string{"claude"}, false).Env)
	if v := env["BRIG_MCP_TOKEN"]; v.Value != "mcp-token-"+c.VMName || !v.Secret {
		t.Errorf("BRIG_MCP_TOKEN = %+v, want the MCP token, marked Secret", v)
	}
	if env["NODE_EXTRA_CA_CERTS"].Value != injector.GuestCAPath {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q for a run reaching brig.internal for MCP alone",
			env["NODE_EXTRA_CA_CERTS"].Value)
	}
	if rt.guestFiles[injector.GuestCAPath] != testBrigCA() {
		t.Error("the CA the broker handed the MCP registration did not reach the guest")
	}
	if c.Network != NetIsolated {
		t.Errorf("Network = %s, want a run with MCP servers isolated", c.Network)
	}
	if d := rt.spec.Egress.Divert; !d.BrigInternal || d.Socket == "" {
		t.Errorf("Divert = %+v, want brig.internal for a run with MCP servers", d)
	}
}

// brig serves MCP servers to claude-code alone. Any other agent would be
// handed a token and no configuration, so the run is refused, naming the
// profile and claude-code, and registers nothing.
func TestMCPRefusedForOtherProfiles(t *testing.T) {
	p, home := mcpSession(t, nofireMCP)
	p.Name = "codex"
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("EnsureRunning = %v, want a refusal naming codex and claude-code", err)
	}
	if rt.boots != 0 || len(broker.calls) != 0 {
		t.Errorf("a refused run booted %d times and asked the broker %v", rt.boots, broker.calls)
	}
}

// Memory and MCP servers share one configuration file, so Claude Code is
// handed one path, with both entries in it.
func TestMCPConfigMergesMemory(t *testing.T) {
	p, home := mcpSession(t, nofireMCP)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home, Memory: "team"}, rt)
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	if c.MCPConfig() != mcpConfigPath || mcpConfigPath != "/brig/mcp/mcp.json" {
		t.Errorf("MCPConfig = %q, want /brig/mcp/mcp.json", c.MCPConfig())
	}
	body := rt.guestFiles[mcpConfigPath]
	var parsed struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", mcpConfigPath, err, body)
	}
	if len(parsed.MCPServers) != 2 || parsed.MCPServers["brig-memory"] == nil || parsed.MCPServers["nofire"] == nil {
		t.Errorf("%s holds %s, want brig-memory and nofire", mcpConfigPath, body)
	}
	if !strings.Contains(body, wantNofireEntry(c.VMName)) ||
		!strings.Contains(body, `"url":"https://brig.internal/`+c.VMName+`/memory","headers":{"Authorization":"Bearer ${BRIG_MEMORY_TOKEN}"}`) {
		t.Errorf("%s holds\n%s", mcpConfigPath, body)
	}
	env := envOf(c.execSpec(set, []string{"claude"}, false).Env)
	if env["BRIG_MCP_TOKEN"].Value == "" || env["BRIG_MEMORY_TOKEN"].Value == "" {
		t.Errorf("the guest did not get both tokens: %v", env)
	}
	n := 0
	for _, v := range c.execSpec(set, []string{"claude"}, false).Env {
		if v.Name == "NODE_EXTRA_CA_CERTS" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("NODE_EXTRA_CA_CERTS is set %d times", n)
	}
}

// A run with neither memory nor an MCP server has no configuration: no path
// for Claude Code, no file in the guest, nothing registered and no token.
func TestMCPNoServersNoFile(t *testing.T) {
	p, home := memorySession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	broker := &fakeBroker{}
	c.broker = broker
	set := creds.Set{}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	if c.MCPConfig() != "" {
		t.Errorf("MCPConfig = %q for a run without memory or MCP servers", c.MCPConfig())
	}
	if _, ok := rt.guestFiles[mcpConfigPath]; ok {
		t.Errorf("a run without memory or MCP servers wrote %s", mcpConfigPath)
	}
	if len(broker.mcpRegistered) != 0 || len(broker.mcpMinted) != 0 {
		t.Errorf("registered %+v and minted %+v", broker.mcpRegistered, broker.mcpMinted)
	}
	if _, ok := envOf(c.execSpec(set, []string{"claude"}, false).Env)["BRIG_MCP_TOKEN"]; ok {
		t.Error("a run without MCP servers got BRIG_MCP_TOKEN")
	}
}

// A policy with an MCP server that is detached from a running sandbox takes
// the server with it: the next exec leaves no MCP registration for the
// sandbox, so the token the running agent holds is refused. With memory on,
// the memory stays registered, as a detached route leaves it.
func TestMCPDetachedPolicyLeavesNoRegistration(t *testing.T) {
	for _, mem := range []string{"", "team"} {
		t.Run("memory="+mem, func(t *testing.T) {
			p, home, rt := runningMemorySandbox(t, mem)
			broker := &fakeBroker{}
			bound := p
			bindPolicy(t, &bound, "nofire", nofireMCP)
			c := loadMCP(t, bound, Options{Workspace: home, NoProject: true}, rt)
			c.broker = broker
			if err := c.PrepareWorkspace(); err != nil {
				t.Fatal(err)
			}
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if _, ok := broker.mcp["brig-claude-code"]; !ok {
				t.Fatalf("the first exec did not register the MCP servers: %v", broker.calls)
			}
			// The policy is detached, and the sandbox is exec'd again.
			c = loadMCP(t, p, Options{Workspace: home, NoProject: true}, rt)
			c.broker = broker
			if err := c.PrepareWorkspace(); err != nil {
				t.Fatal(err)
			}
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if rt.boots != 0 {
				t.Fatal("an exec rebooted the running sandbox")
			}
			if reg, ok := broker.mcp["brig-claude-code"]; ok {
				t.Errorf("the detached policy's MCP servers are still registered: %+v", reg)
			}
			if _, ok := envOf(c.execSpec(creds.Set{}, nil, false).Env)["BRIG_MCP_TOKEN"]; ok {
				t.Error("an exec without MCP servers handed the guest BRIG_MCP_TOKEN")
			}
			if mem != "" {
				if _, ok := broker.memory["brig-claude-code"]; !ok {
					t.Error("detaching the MCP policy removed the sandbox's memory")
				}
			}
		})
	}
}

// An exec whose policies declare another set of servers registers that set,
// which replaces the one before, so the guest is served only what its
// policies now name.
func TestMCPChangedServersReRegister(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "")
	broker := &fakeBroker{}
	first := p
	bindPolicy(t, &first, "nofire", nofireMCP)
	c := loadMCP(t, first, Options{Workspace: home, NoProject: true}, rt)
	c.broker = broker
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	second := p
	bindPolicy(t, &second, "docs", strings.ReplaceAll(strings.ReplaceAll(nofireMCP,
		"{name: nofire}", "{name: docs}"), "- name: nofire", "- name: docs"))
	c = loadMCP(t, second, Options{Workspace: home, NoProject: true}, rt)
	c.broker = broker
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	reg := broker.mcp["brig-claude-code"]
	if len(reg.Servers) != 1 || reg.Servers[0].Server.Name != "docs" || reg.Servers[0].Policy != "docs" {
		t.Errorf("registered %+v, want the docs server alone", reg.Servers)
	}
	if body := rt.guestFiles[mcpConfigPath]; strings.Contains(body, `"nofire"`) || !strings.Contains(body, `"docs"`) {
		t.Errorf("%s holds %s, want the docs server alone", mcpConfigPath, body)
	}
}

// readRecorder is a store that records the names it is asked to read.
type readRecorder struct {
	fakeSecretStore
	read *[]string
}

func (r readRecorder) Read(name string) ([]byte, error) {
	*r.read = append(*r.read, name)
	return r.fakeSecretStore.Read(name)
}

// withheldSession is mcpSession with a profile that binds the NOFire
// credential's secret, and an unrelated one, into the guest.
func withheldSession(t *testing.T) (profile.Profile, string) {
	t.Helper()
	p, home := mcpSession(t, nofireMCP)
	p.Secrets = append(slices.Clone(p.Secrets), profile.SecretDecl{Name: "nofire-token"},
		profile.SecretDecl{Name: "other-token"})
	p.Env = append(slices.Clone(p.Env),
		profile.EnvBinding{Name: "NOFIRE_TOKEN", Ref: "secrets.nofire-token"},
		profile.EnvBinding{Name: "OTHER_TOKEN", Ref: "secrets.other-token"})
	return p, home
}

// A secret an MCP server's credential names stays on the host, as a route's
// does: an env binding of it is dropped, brig never reads it, and its value
// reaches no part of the guest. A binding of another secret is delivered.
func TestMCPSecretIsWithheldFromTheGuestEnv(t *testing.T) {
	p, home := withheldSession(t)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	var read []string
	store := readRecorder{fakeSecretStore{"nofire-token": nofireSecret, "other-token": "other-value"}, &read}
	c.OpenStore = func() (creds.SecretReader, error) { return store, nil }
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureRunning(set); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(read, "nofire-token") {
		t.Errorf("brig read the MCP server's secret: %v", read)
	}
	if !slices.Contains(c.routeSkipped, "nofire-token") {
		t.Errorf("kept on the host %v, want nofire-token", c.routeSkipped)
	}
	spec := c.execSpec(set, []string{"claude", "--mcp-config=" + c.MCPConfig()}, false)
	env := envOf(spec.Env)
	if _, ok := env["NOFIRE_TOKEN"]; ok {
		t.Error("the guest got NOFIRE_TOKEN, bound to the MCP server's secret")
	}
	if env["OTHER_TOKEN"].Value != "other-value" {
		t.Errorf("OTHER_TOKEN = %+v, want the unrelated secret delivered", env["OTHER_TOKEN"])
	}
	for _, v := range spec.Env {
		if strings.Contains(v.Value, nofireSecret) {
			t.Errorf("the MCP server's secret is in the guest variable %s", v.Name)
		}
	}
	if strings.Contains(strings.Join(spec.Cmd, " "), nofireSecret) {
		t.Errorf("the agent's command line carries the MCP server's secret: %q", spec.Cmd)
	}
	for _, run := range rt.guestRuns {
		if strings.Contains(strings.Join(run.Cmd, " "), nofireSecret) {
			t.Errorf("a command run in the guest carries the MCP server's secret: %q", run.Cmd)
		}
	}
	for path, got := range rt.guestFiles {
		if strings.Contains(got, nofireSecret) {
			t.Errorf("the MCP server's secret was written to %s", path)
		}
	}
}

// A files: binding of the MCP server's secret is dropped too, and the
// profile no longer declares it, so brig never reads it to write it. A files:
// binding of another secret stays.
func TestMCPSecretIsWithheldFromTheGuestFiles(t *testing.T) {
	p, home := withheldSession(t)
	p.Files = append(slices.Clone(p.Files),
		profile.FileBinding{Ref: "secrets.nofire-token", Path: ".nofire/token", Mode: "0600"},
		profile.FileBinding{Ref: "secrets.other-token", Path: ".other/token", Mode: "0600"})
	c := loadMCP(t, p, Options{Workspace: home}, nil)
	for _, f := range c.Profile.Files {
		if f.Ref == "secrets.nofire-token" {
			t.Errorf("the files: binding of the MCP server's secret is still delivered: %+v", f)
		}
	}
	if !slices.ContainsFunc(c.Profile.Files, func(f profile.FileBinding) bool { return f.Ref == "secrets.other-token" }) {
		t.Errorf("the files: binding of an unrelated secret was dropped: %+v", c.Profile.Files)
	}
	for _, d := range c.Profile.Secrets {
		if d.Name == "nofire-token" {
			t.Error("brig still reads the MCP server's secret")
		}
	}
	if !slices.ContainsFunc(c.Profile.Secrets, func(d profile.SecretDecl) bool { return d.Name == "other-token" }) {
		t.Error("the unrelated secret is no longer declared")
	}
}

// A store that does not hold the MCP server's secret refuses the run before
// boot, naming the policy and the server, as for a route's.
func TestMCPMissingSecretNamesTheServer(t *testing.T) {
	p, home := mcpSession(t, nofireMCP)
	rt := &memoryRuntime{&livenessRuntime{workspace: home}}
	c := loadMCP(t, p, Options{Workspace: home}, rt)
	c.OpenStore = func() (creds.SecretReader, error) { return fakeSecretStore{}, nil }
	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "nofire (mcp nofire)") {
		t.Fatalf("EnsureRunning = %v, want a refusal naming nofire (mcp nofire)", err)
	}
	if rt.boots != 0 {
		t.Error("a run whose store lacks the MCP server's secret booted")
	}
}

// A sandbox that keeps a route when its MCP policy is detached has its MCP
// servers removed alone on the next exec: the MCP token its agent holds is
// refused, and its routes stay registered.
func TestMCPDetachedBesideARouteForgetsMCPAlone(t *testing.T) {
	p, home, rt := runningMemorySandbox(t, "")
	routeAndMCP := claudeEndpoint + nofireMCP[strings.Index(nofireMCP, "  mcp:"):]
	broker := &fakeBroker{}
	exec := func(doc string) *Config {
		t.Helper()
		bound := p
		bindPolicy(t, &bound, "brokered", doc)
		c := loadMCP(t, bound, Options{Workspace: home, NoProject: true}, rt)
		c.OpenStore = func() (creds.SecretReader, error) {
			return fakeSecretStore{"claude-credentials": "{}", "nofire-token": nofireSecret}, nil
		}
		c.broker = broker
		if err := c.PrepareWorkspace(); err != nil {
			t.Fatal(err)
		}
		set, err := c.BuildEnv()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureRunning(set); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := exec(routeAndMCP)
	if len(c.routes) != 1 || len(c.mcp) != 1 {
		t.Fatalf("routes %v and mcp %v, want one of each", c.routes, c.mcp)
	}
	if _, ok := broker.mcp["brig-claude-code"]; !ok {
		t.Fatalf("the first exec did not register the MCP servers: %v", broker.calls)
	}
	// The MCP server leaves the policy; the route stays.
	exec(claudeEndpoint)
	if rt.boots != 0 {
		t.Fatal("an exec rebooted the running sandbox")
	}
	if !slices.Equal(broker.mcpForgotten, []string{"brig-claude-code"}) {
		t.Errorf("mcp forgotten %v, want the sandbox's MCP servers removed once", broker.mcpForgotten)
	}
	if _, ok := broker.mcp["brig-claude-code"]; ok {
		t.Error("the detached MCP servers are still registered")
	}
	if len(broker.forgotten) != 0 || len(broker.routesForgotten) != 0 {
		t.Errorf("forgotten %v and routes forgotten %v, want the routes kept", broker.forgotten, broker.routesForgotten)
	}
	if len(broker.registered) != 2 || len(broker.registered[1].Routes) != 1 {
		t.Errorf("registered %+v, want the route registered on each exec", broker.registered)
	}
}

// The boot says which MCP servers the sandbox is attached to, as it says
// which memory store, and a boot with none says nothing of MCP.
func TestMCPBootLineNamesTheServers(t *testing.T) {
	boot := func(p profile.Profile, home string) string {
		t.Helper()
		rt := &memoryRuntime{&livenessRuntime{workspace: home}}
		c := loadMCP(t, p, Options{Workspace: home}, rt)
		var warn strings.Builder
		c.Err = &warn
		if err := c.EnsureRunning(creds.Set{}); err != nil {
			t.Fatal(err)
		}
		return warn.String()
	}
	p, home := mcpSession(t, nofireMCP)
	if got := boot(p, home); !strings.Contains(got, "mcp: nofire\n") {
		t.Errorf("the boot does not name the MCP server:\n%s", got)
	}
	p, home = memorySession(t)
	if got := boot(p, home); strings.Contains(got, "mcp:") {
		t.Errorf("a boot with no MCP server names one:\n%s", got)
	}
}

// A refusal names the policies that declare MCP servers, and its grammar
// follows how many there are: "policy a (mcp x) declares", "policies a
// (mcp x), b (mcp y) declare".
func TestMCPRefusalWordsOneOrMorePolicies(t *testing.T) {
	one := []policy.MCPEntry{{Policy: "a", Server: v1alpha1.MCPServer{Name: "x"}}}
	two := append(slices.Clone(one), policy.MCPEntry{Policy: "b", Server: v1alpha1.MCPServer{Name: "y"}})
	for _, tc := range []struct {
		entries []policy.MCPEntry
		profile string
		network Network
		want    string
	}{
		{one, "codex", "", "policy a (mcp x) declares MCP servers, "},
		{two, "codex", "", "policies a (mcp x), b (mcp y) declare MCP servers, "},
		{one, "claude-code", NetOffline, "each MCP server of policy a (mcp x) is served to the guest"},
		{two, "claude-code", NetOffline, "each MCP server of policies a (mcp x), b (mcp y) is served to the guest"},
	} {
		c := &Config{Profile: profile.Profile{Name: tc.profile}, Network: tc.network, Runtime: &livenessRuntime{},
			mcp: tc.entries}
		err := c.mcpRefusal()
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("mcpRefusal with %d policies on %s = %v, want it to start %q", len(tc.entries),
				tc.profile, err, tc.want)
		}
	}
}
