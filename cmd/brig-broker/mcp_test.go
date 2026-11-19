package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/sessiontoken"
)

const (
	// mcpSecretName and mcpSecretValue are the credential of the MCP
	// server in these tests, as the store holds it.
	mcpSecretName  = "nofire-token"
	mcpSecretValue = "nofire-secret-value"
	rpcCallSearch  = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"q":"SECRETQUERY"}}}`
	rpcCallDelete  = `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_everything","arguments":{}}}`
)

// mcpRig is a broker in this process, with a memory root and an upstream of
// its own that every MCP server and route reaches.
type mcpRig struct {
	*running
	root string
	mu   sync.Mutex
	seen []upstreamRequest
}

// mcpResult answers with a JSON-RPC result.
func mcpResult(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"found it"}]}}`)
}

// newMCPRig starts the rig. answer is how the upstream answers, after it
// records the request; nil is mcpResult.
func newMCPRig(t *testing.T, answer http.HandlerFunc) *mcpRig {
	t.Helper()
	if answer == nil {
		answer = mcpResult
	}
	rig := &mcpRig{root: filepath.Join(shortDir(t), "memory")}
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.seen = append(rig.seen, upstreamRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
			Body: string(body)})
		rig.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		answer(w, r)
	}))
	t.Cleanup(up.Close)
	cfg := serverConfig{dir: shortDir(t), logTo: io.Discard, memoryDir: rig.root, transport: transportTo(up)}
	rig.running = startBrokerWith(t, cfg, nil)
	if err := rig.store.Create(mcpSecretName, []byte(mcpSecretValue)); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *mcpRig) upstream() []upstreamRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// nofireMCP is a server as a policy writes it without a preset.
func nofireMCP() injector.MCPServer {
	return injector.MCPServer{Policy: "nofire", Server: v1alpha1.MCPServer{
		Name:       "nofire",
		Transport:  v1alpha1.MCPTransportHTTP,
		URL:        "https://mcp.example.com/mcp",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: mcpSecretName}},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}},
		}},
		Tools: &v1alpha1.MCPTools{Allow: []string{"search"}},
	}}
}

// addMCP registers the MCP servers of id at the brig.internal address
// internal, nofireMCP when none is given.
func (r *mcpRig) addMCP(t *testing.T, id, internal string, servers ...injector.MCPServer) injector.AddResponse {
	t.Helper()
	if len(servers) == 0 {
		servers = []injector.MCPServer{nofireMCP()}
	}
	added, err := r.client.AddMCP(injector.MCPSandbox{ID: id, Profile: "claude-code", Servers: servers,
		BrigInternal: internal})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket == "" || added.CA == "" {
		t.Fatalf("add mcp = %+v, want a divert socket and the CA", added)
	}
	return added
}

func (r *mcpRig) mcpToken(t *testing.T, id string) string {
	t.Helper()
	tok, err := r.client.MintMCP(injector.MCPMintRequest{Sandbox: id})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// mcpReg is the MCP registration of sandbox id, as the broker holds it.
func (r *running) mcpReg(t *testing.T, id string) mcpReg {
	t.Helper()
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	reg, ok := r.srv.mcps[id]
	if !ok {
		t.Fatalf("sandbox %s has no mcp registration", id)
	}
	return reg
}

// A registered sandbox is minted an MCP token that its own key verifies as
// an MCP token, from a key of its own: not the host key, not the memory key.
func TestMCPKeyIsItsOwn(t *testing.T) {
	rig := newMCPRig(t, nil)
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	reg := rig.mcpReg(t, "brig-a")
	c, err := sessiontoken.Verify(reg.key, tok, "brig-a", "", mcpTokenType, time.Now())
	if err != nil {
		t.Fatalf("the mcp token does not verify: %v", err)
	}
	if c.Typ != "brig-mcp" || c.Route != "" || c.Exp > time.Now().Add(injector.TokenTTL+time.Minute).Unix() {
		t.Errorf("claims = %+v", c)
	}
	path := filepath.Join(rig.dir, injector.MCPKeyName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != hostKeySize {
		t.Errorf("mcp key mode %v size %d", info.Mode().Perm(), info.Size())
	}
	mcpKey, _ := os.ReadFile(path)
	for _, other := range []string{injector.HostKeyName, injector.MemoryKeyName} {
		k, err := os.ReadFile(filepath.Join(rig.dir, other))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(k, mcpKey) {
			t.Errorf("the mcp key is the %s", other)
		}
	}
	want, err := sessiontoken.SandboxKey(mcpKey, "brig-mcp", "brig-a", reg.nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reg.key, want) {
		t.Error("the mcp key of brig-a is not derived from mcpkey with label brig-mcp")
	}
	if _, err := rig.client.MintMCP(injector.MCPMintRequest{Sandbox: "brig-b"}); refusedStatus(err) != http.StatusNotFound {
		t.Errorf("an mcp token for an unregistered sandbox = %v, want 404", err)
	}
}

// postTo sends body to url with token, through c, and returns the status.
func postTo(t *testing.T, c *http.Client, url, token, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// MCP is served on brig.internal alone. The same request with a valid token
// on the loopback route server, which names no sandbox, and on the
// link-local address, which is clear text, is not found, and nothing reaches
// the upstream.
func TestMCPServedOnlyOnBrigInternal(t *testing.T) {
	rig := newMCPRig(t, nil)
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	loopback := fmt.Sprintf("http://127.0.0.1:%d/brig-a/mcp/nofire", rig.srv.port)
	if got := postTo(t, http.DefaultClient, loopback, tok, rpcCallSearch); got != http.StatusNotFound {
		t.Errorf("mcp on the loopback port: %d, want 404", got)
	}
	host, _, _ := net.SplitHostPort(injector.LinkLocalAddr)
	if got := postTo(t, linkLocalClient(added.DivertSocket), "http://"+host+"/brig-a/mcp/nofire", tok,
		rpcCallSearch); got != http.StatusNotFound {
		t.Errorf("mcp on the link-local address: %d, want 404", got)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-a", "check=arrival", "status=404"); len(lines) != 1 {
		t.Errorf("the audit lines of the link-local request: %q", lines)
	}
	// The same token on brig.internal is served, so the refusals above
	// were for where the request arrived.
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if got, body, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK ||
		!strings.Contains(body, "found it") {
		t.Errorf("mcp on brig.internal: %d %q %v, want 200", got, body, err)
	}
}

// A token is served only on the divert socket of its own sandbox: brig-a's
// token for brig-a's server arriving on brig-b's socket is not found, and
// brig-a's token for brig-b's server is not brig-b's token.
func TestMCPRefusesOtherSandboxArrival(t *testing.T) {
	rig := newMCPRig(t, nil)
	rig.addMCP(t, "brig-a", internalA)
	b := rig.addMCP(t, "brig-b", internalB)
	tokA := rig.mcpToken(t, "brig-a")
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-a/mcp/nofire", tokA, rpcCallSearch); err != nil || got != http.StatusNotFound {
		t.Errorf("brig-a's server on brig-b's socket: %d %v, want 404", got, err)
	}
	if got, _, err := rpc(cb, "/brig-b/mcp/nofire", tokA, rpcCallSearch); err != nil || got != http.StatusUnauthorized {
		t.Errorf("brig-a's token on brig-b's server: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-b", "path=brig-a", "check=arrival"); len(lines) != 1 {
		t.Errorf("the audit lines of brig-b asking for brig-a: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "sandbox=brig-a"); len(lines) != 0 {
		t.Errorf("a request of brig-b wrote a line naming brig-a as its sandbox: %q", lines)
	}
}

// addAll registers brig-a for routes, memory and MCP at internalA, and
// returns the answer to the routes' registration.
func (r *mcpRig) addAll(t *testing.T) injector.AddResponse {
	t.Helper()
	added, err := r.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes-a", BrigInternal: internalA}); err != nil {
		t.Fatal(err)
	}
	r.addMCP(t, "brig-a", internalA)
	return added
}

// A route token and a memory token of the same sandbox are refused by MCP as
// a bad token, as memory refuses a route token, and reach no upstream.
func TestMCPRefusesRouteAndMemoryTokens(t *testing.T) {
	rig := newMCPRig(t, nil)
	added := rig.addAll(t)
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	memTok, err := rig.client.MintMemory(injector.MemoryMintRequest{Sandbox: "brig-a"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"route": rig.mint(t, "brig-a", "anthropic"), "memory": memTok, "no": ""} {
		if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusUnauthorized {
			t.Errorf("a %s token on the mcp path: %d %v, want 401", name, got, err)
		}
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", rig.mcpToken(t, "brig-a"), rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("the mcp token on the mcp path: %d %v, want 200", got, err)
	}
}

// An MCP token is refused by a route and by memory.
func TestRouteAndMemoryRefuseMCPToken(t *testing.T) {
	rig := newMCPRig(t, nil)
	added := rig.addAll(t)
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	tok := rig.mcpToken(t, "brig-a")
	if got, err := post(c, "/brig-a/r/anthropic/v1/messages", tok); err != nil || got != http.StatusUnauthorized {
		t.Errorf("an mcp token on a route: %d %v, want 401", got, err)
	}
	if got, _, err := rpc(c, "/brig-a/memory", tok, rpcInitialize); err != nil || got != http.StatusUnauthorized {
		t.Errorf("an mcp token on memory: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
}

// A server the sandbox did not register is not found, after the token check,
// and so is anything below a server's path.
func TestMCPUnknownServerName(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	for _, path := range []string{"/brig-a/mcp/other", "/brig-a/mcp/", "/brig-a/mcp/nofire/x", "/brig-a/mcp/Nofire"} {
		if got, _, err := rpc(c, path, tok, rpcCallSearch); err != nil || got != http.StatusNotFound {
			t.Errorf("%s: %d %v, want 404", path, got, err)
		}
	}
	// Without a token the name is not looked up: a guest cannot learn which
	// servers another policy attached.
	if got, _, err := rpc(c, "/brig-a/mcp/other", "", rpcCallSearch); err != nil || got != http.StatusUnauthorized {
		t.Errorf("an unknown server without a token: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
	long := strings.Repeat("y", 300)
	if got, _, err := rpc(c, "/brig-a/mcp/"+long, tok, rpcCallSearch); err != nil || got != http.StatusNotFound {
		t.Errorf("a long server name: %d %v, want 404", got, err)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "server="+long[:64]+"...", "check=server"); len(lines) != 1 {
		t.Errorf("the audit lines of a long server name: %q", lines)
	}
	if lines := auditWith(t, rig.dir, strings.Repeat("y", 65)); len(lines) != 0 {
		t.Errorf("the server name was not clipped: %q", lines)
	}
}

// remove-mcp revokes the tokens: a token minted before is refused once the
// sandbox is removed, and still once it registers again, under a new nonce.
func TestMCPRemoveRevokesTokens(t *testing.T) {
	rig := newMCPRig(t, nil)
	// The routes keep the divert socket open across the removal, so the
	// refusal comes from the MCP check and not from a closed socket.
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Fatalf("before the removal: %d %v, want 200", got, err)
	}
	rm, err := rig.client.RemoveMCP("brig-a")
	if err != nil || !rm.Removed || rm.Remaining != 1 {
		t.Fatalf("remove mcp = %+v, %v, want removed, the routes remaining", rm, err)
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusNotFound {
		t.Errorf("after the removal: %d %v, want 404", got, err)
	}
	if _, err := rig.client.MintMCP(injector.MCPMintRequest{Sandbox: "brig-a"}); refusedStatus(err) != http.StatusNotFound {
		t.Errorf("an mcp token after the removal = %v, want 404", err)
	}
	rig.addMCP(t, "brig-a", internalA)
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusUnauthorized {
		t.Errorf("the old token after a new registration: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 1 {
		t.Errorf("the upstream saw %d requests, want the 1 before the removal", n)
	}
	if rm, err := rig.client.RemoveMCP("brig-c"); err != nil || rm.Removed {
		t.Errorf("remove an unregistered sandbox = %+v, %v", rm, err)
	}
}

// The same registration again keeps the nonce, so a running agent's token
// stays valid. Another set of servers replaces the registration under a new
// nonce, as another set of routes does.
func TestMCPSameRegistrationKeepsTheToken(t *testing.T) {
	rig := newMCPRig(t, nil)
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	rig.addMCP(t, "brig-a", internalA)
	if _, err := sessiontoken.Verify(rig.mcpReg(t, "brig-a").key, tok, "brig-a", "", mcpTokenType, time.Now()); err != nil {
		t.Errorf("the same registration again revoked the token: %v", err)
	}
	wider := nofireMCP()
	wider.Server.Tools = &v1alpha1.MCPTools{Allow: []string{"*"}}
	rig.addMCP(t, "brig-a", internalA, wider)
	if _, err := sessiontoken.Verify(rig.mcpReg(t, "brig-a").key, tok, "brig-a", "", mcpTokenType, time.Now()); err == nil {
		t.Error("a token minted for one set of servers stayed valid for another")
	}
	if _, err := rig.client.AddMCP(injector.MCPSandbox{ID: "brig-a", Profile: "claude-code",
		Servers: []injector.MCPServer{nofireMCP()}, BrigInternal: internalB}); refusedStatus(err) != http.StatusConflict {
		t.Errorf("brig-a at another brig.internal address = %v, want 409", err)
	}
}

// The broker expands what it is sent and does not rely on brig's policy
// check: a server it cannot expand is refused, and so is a sandbox that does
// not reach brig.internal, where MCP is served alone.
func TestMCPRefusesUnexpandableRegistration(t *testing.T) {
	rig := newMCPRig(t, nil)
	stdio := nofireMCP()
	stdio.Server.Transport = "stdio"
	plain := nofireMCP()
	plain.Server.URL = "http://mcp.example.com/mcp"
	noTools := nofireMCP()
	noTools.Server.Tools = nil
	redirected := injector.MCPServer{Policy: "nofire", Server: v1alpha1.MCPServer{Name: "nofire",
		Preset: v1alpha1.MCPPresetNofire, URL: "https://evil.example.com/mcp",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: mcpSecretName}},
		Tools:      &v1alpha1.MCPTools{Allow: []string{"*"}}}}
	reinjected := injector.MCPServer{Policy: "nofire", Server: v1alpha1.MCPServer{Name: "nofire",
		Preset: v1alpha1.MCPPresetNofire, URL: "https://mcp.nofire.ai/mcp",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: mcpSecretName}},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "X-Token", Value: "{value}"}},
		}},
		Tools: &v1alpha1.MCPTools{Allow: []string{"*"}}}}
	reserved := nofireMCP()
	reserved.Server.Name = "brig-memory"
	for name, tc := range map[string]injector.MCPSandbox{
		"stdio":             {ID: "brig-a", Servers: []injector.MCPServer{stdio}, BrigInternal: internalA},
		"http url":          {ID: "brig-a", Servers: []injector.MCPServer{plain}, BrigInternal: internalA},
		"no tools":          {ID: "brig-a", Servers: []injector.MCPServer{noTools}, BrigInternal: internalA},
		"preset elsewhere":  {ID: "brig-a", Servers: []injector.MCPServer{redirected}, BrigInternal: internalA},
		"preset reinjected": {ID: "brig-a", Servers: []injector.MCPServer{reinjected}, BrigInternal: internalA},
		"reserved name":     {ID: "brig-a", Servers: []injector.MCPServer{reserved}, BrigInternal: internalA},
		"twice":             {ID: "brig-a", Servers: []injector.MCPServer{nofireMCP(), nofireMCP()}, BrigInternal: internalA},
		"no servers":        {ID: "brig-a", BrigInternal: internalA},
		"no brig.internal":  {ID: "brig-a", Servers: []injector.MCPServer{nofireMCP()}},
		"loopback":          {ID: "brig-a", Servers: []injector.MCPServer{nofireMCP()}, BrigInternal: "127.0.0.1"},
		"bad id":            {ID: "brig a", Servers: []injector.MCPServer{nofireMCP()}, BrigInternal: internalA},
	} {
		tc.Profile = "claude-code"
		if _, err := rig.client.AddMCP(tc); refusedStatus(err) != http.StatusBadRequest {
			t.Errorf("%s: add mcp = %v, want 400", name, err)
		}
	}
	if st, err := rig.client.Status(); err != nil || len(st.MCP) != 0 {
		t.Errorf("after refusals status = %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(rig.dir, "divert")); err == nil {
		if entries, _ := os.ReadDir(filepath.Join(rig.dir, "divert")); len(entries) != 0 {
			t.Errorf("a refused registration left a divert socket: %v", entries)
		}
	}
	// A preset server as brig sends it, expanded, registers, and is served
	// at the preset's url alone.
	expanded := injector.MCPServer{Policy: "nofire", Server: v1alpha1.MCPServer{Name: "nofire",
		Preset: v1alpha1.MCPPresetNofire, Transport: v1alpha1.MCPTransportHTTP, URL: "https://mcp.nofire.ai/mcp",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: mcpSecretName}},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}},
		}},
		Tools: &v1alpha1.MCPTools{Allow: []string{"*"}}}}
	rig.addMCP(t, "brig-a", internalA, expanded)
	asWritten := injector.MCPServer{Policy: "nofire", Server: v1alpha1.MCPServer{Name: "nofire",
		Preset:     v1alpha1.MCPPresetNofire,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: mcpSecretName}},
		Tools:      &v1alpha1.MCPTools{Allow: []string{"*"}}}}
	rig.addMCP(t, "brig-b", internalB, asWritten)
	for _, id := range []string{"brig-a", "brig-b"} {
		if got := rig.mcpReg(t, id).servers["nofire"].URL; got != "https://mcp.nofire.ai/mcp" {
			t.Errorf("%s: the preset server is served at %q", id, got)
		}
	}
}

// A call the policy allows reaches the upstream with the credential from
// the store in place of the guest's token, and the guest's answer holds
// neither. A call the policy does not name never reaches the upstream. Each
// has one audit line, which holds no argument, credential or token.
func TestMCPServesAllowedCall(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	got, body, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch)
	if err != nil || got != http.StatusOK || !strings.Contains(body, "found it") {
		t.Fatalf("an allowed call: %d %q %v", got, body, err)
	}
	if strings.Contains(body, mcpSecretValue) || strings.Contains(body, tok) {
		t.Errorf("the guest's answer holds the credential or its token: %q", body)
	}
	seen := rig.upstream()
	if len(seen) != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", len(seen))
	}
	if auth := seen[0].Header.Get("Authorization"); auth != "Bearer "+mcpSecretValue {
		t.Errorf("the upstream's Authorization = %q, want the credential", auth)
	}
	if seen[0].Path != "/mcp" {
		t.Errorf("the upstream path = %q, want /mcp", seen[0].Path)
	}
	for _, v := range seen[0].Header {
		for _, s := range v {
			if strings.Contains(s, tok) {
				t.Errorf("the guest's token went upstream: %q", s)
			}
		}
	}
	got, body, err = rpc(c, "/brig-a/mcp/nofire", tok, rpcCallDelete)
	if err != nil || got != http.StatusOK || !strings.Contains(body, `"error"`) {
		t.Errorf("a call the policy does not name: %d %q %v, want a JSON-RPC error", got, body, err)
	}
	if n := len(rig.upstream()); n != 1 {
		t.Errorf("a refused call reached the upstream: %d requests", n)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-a", "server=nofire", "method=tools/call",
		"tool=search", "verdict=forwarded", "status=200"); len(lines) != 1 {
		t.Errorf("the audit lines of the allowed call: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "tool=delete_everything", "verdict=refused:tool"); len(lines) != 1 {
		t.Errorf("the audit lines of the refused call: %q", lines)
	}
	blob, err := os.ReadFile(filepath.Join(rig.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{mcpSecretValue, tok, "SECRETQUERY"} {
		if strings.Contains(string(blob), s) {
			t.Errorf("the audit log holds %q", s)
		}
	}
}

// A guest's method name cannot write a line of its own or a field of its
// own into the audit log: it is quoted, and clipped.
func TestMCPAuditQuotesGuestStrings(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	evil := `{"jsonrpc":"2.0","id":5,"method":"x\nlevel=ERROR forged=yes \"q\""}`
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, evil); err != nil || got != http.StatusOK {
		t.Fatalf("a refused method: %d %v", got, err)
	}
	lines := auditWith(t, rig.dir, "kind=mcp", "verdict=refused:method")
	if len(lines) != 1 || !strings.Contains(lines[0], `method="x\nlevel=ERROR forged=yes \"q\""`) {
		t.Errorf("the audit line of a forged method: %q", lines)
	}
	if forged := auditWith(t, rig.dir, "forged=yes"); len(forged) != 1 || !strings.Contains(forged[0], "kind=mcp") {
		t.Errorf("the method wrote a line of its own: %q", forged)
	}
}

// When the filter fails part way through a stream, the guest sees a broken
// stream, not a clean end: the abort reaches net/http through internalMux
// with nothing in between to recover it.
func TestMCPAbortMidStreamBreaksTheGuestStream(t *testing.T) {
	rig := newMCPRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {not json\n\n")
	})
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/mcp/nofire",
		strings.NewReader(rpcCallSearch))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 before the abort", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("the stream ended cleanly with %q, want a broken stream", body)
	}
	if !strings.Contains(string(body), "notifications/progress") || strings.Contains(string(body), "not json") {
		t.Errorf("the guest got %q, want the first event alone", body)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-a", "verdict=error"); len(lines) != 1 {
		t.Errorf("the audit lines of the aborted stream: %q", lines)
	}
}

// A sandbox with MCP alone keeps the broker: stop-if-idle leaves it
// running, and removing the MCP registration leaves nothing.
func TestRemainingCountsMCP(t *testing.T) {
	rig := newMCPRig(t, nil)
	rig.addMCP(t, "brig-a", internalA)
	st, err := rig.client.Status()
	if err != nil || !slices.Equal(st.MCP, []string{"brig-a"}) || len(st.Sandboxes) != 0 || len(st.Memory) != 0 {
		t.Errorf("status = %+v, %v, want brig-a under mcp alone", st, err)
	}
	if _, err := rig.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes-a", BrigInternal: internalA}); err != nil {
		t.Fatal(err)
	}
	if rm, err := rig.client.RemoveMemory("brig-a"); err != nil || !rm.Removed || rm.Remaining != 1 {
		t.Errorf("remove the memory = %+v, %v, want removed, mcp remaining", rm, err)
	}
	if stopping, err := rig.client.Stop(true); err != nil || stopping {
		t.Errorf("stop ifIdle with mcp registered = %v, %v", stopping, err)
	}
	if rm, err := rig.client.RemoveMCP("brig-a"); err != nil || !rm.Removed || rm.Remaining != 0 {
		t.Errorf("remove the mcp = %+v, %v, want removed, none remaining", rm, err)
	}
	if stopping, err := rig.client.Stop(true); err != nil || !stopping {
		t.Errorf("stop ifIdle with nothing registered = %v, %v", stopping, err)
	}
}

// The routes, the memory and the MCP servers of a sandbox share one divert
// socket at one brig.internal address. Each keeps it open while it holds
// it, and a registration at another address is refused.
func TestMCPSharesTheDivertSocket(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	if _, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalB}); refusedStatus(err) != http.StatusConflict {
		t.Errorf("routes at another address than the mcp servers = %v, want 409", err)
	}
	if _, err := rig.client.AddMemory(injector.MemorySandbox{ID: "brig-a", Key: "notes-a",
		BrigInternal: internalB}); refusedStatus(err) != http.StatusConflict {
		t.Errorf("memory at another address than the mcp servers = %v, want 409", err)
	}
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	if added.DivertSocket != a.DivertSocket {
		t.Errorf("the routes got socket %s, the mcp servers %s", added.DivertSocket, a.DivertSocket)
	}
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	// Removing the routes leaves the socket to the mcp servers.
	if _, err := rig.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("mcp after the routes were removed: %d %v, want 200", got, err)
	}
	// Removing memory, which the sandbox never had, leaves it too.
	if _, err := rig.client.RemoveMemory("brig-a"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("mcp after a memory removal: %d %v, want 200", got, err)
	}
	// Removing the mcp servers, the last holder, closes the socket.
	if _, err := rig.client.RemoveMCP("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.DivertSocket); err == nil {
		t.Error("the divert socket stayed after its last holder was removed")
	}
	// Routes registered after the mcp servers keep the socket when the mcp
	// servers go.
	rig.addMCP(t, "brig-b", internalB)
	addedB, err := rig.client.Add(injector.Sandbox{ID: "brig-b", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalB})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.RemoveMCP("brig-b"); err != nil {
		t.Fatal(err)
	}
	cb := guestClient(addedB.DivertSocket, internalB+":443", caPool(t, addedB))
	if got, err := post(cb, "/brig-b/r/anthropic/v1/messages", rig.mint(t, "brig-b", "anthropic")); err != nil || got != http.StatusOK {
		t.Errorf("a route after the mcp servers were removed: %d %v, want 200", got, err)
	}
}

// brig rm revokes the MCP token: after Forget, a token minted before is
// refused, by the same sandbox registered again too, and the broker holds no
// MCP registration of the sandbox. Another sandbox keeps the broker running.
func TestForgetRevokesTheMCPToken(t *testing.T) {
	rig := newMCPRig(t, nil)
	rig.addMCP(t, "brig-b", internalB)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Fatalf("before Forget: %d %v, want 200", got, err)
	}
	if err := injector.Forget(rig.dir, "brig-a"); err != nil {
		t.Fatal(err)
	}
	st, err := rig.client.Status()
	if err != nil || !slices.Equal(st.MCP, []string{"brig-b"}) {
		t.Fatalf("status after Forget = %+v, %v, want brig-b alone under mcp", st, err)
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err == nil && got == http.StatusOK {
		t.Error("the token was served after Forget")
	}
	a = rig.addMCP(t, "brig-a", internalA)
	c = guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusUnauthorized {
		t.Errorf("the token minted before Forget, after a new registration: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 1 {
		t.Errorf("the upstream saw %d requests, want the 1 before Forget", n)
	}
}

// brig rm --all revokes every MCP token and leaves the broker with no MCP
// registration, so it stops.
func TestForgetAllRevokesTheMCPTokens(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	rig.addMCP(t, "brig-b", internalB)
	tok := rig.mcpToken(t, "brig-a")
	removed, err := injector.ForgetAll(rig.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"brig-a", "brig-b"}) {
		t.Errorf("ForgetAll removed %v, want brig-a brig-b", removed)
	}
	rig.srv.mu.Lock()
	n := len(rig.srv.mcps)
	rig.srv.mu.Unlock()
	if n != 0 {
		t.Errorf("the broker holds %d mcp registrations after ForgetAll", n)
	}
	select {
	case <-rig.done:
		rig.done <- nil
	case <-time.After(5 * time.Second):
		t.Error("the broker did not stop with nothing registered")
	}
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err == nil {
		t.Errorf("the token reached the broker after ForgetAll: %d", got)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
}

// MCP requests past the bound of one sandbox are answered 503 at once, as
// memory's are, reach no upstream, and leave another sandbox served.
func TestMCPBusy503(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	b := rig.addMCP(t, "brig-b", internalB)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	// The slots are taken as requests in flight would take them.
	for range maxMCPPerSandbox {
		if !rig.srv.mcpGate.acquire("brig-a") {
			t.Fatal("a slot under the bound was refused")
		}
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/mcp/nofire",
		strings.NewReader(rpcCallSearch))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" ||
		strings.Count(strings.TrimSpace(string(body)), "\n") != 0 {
		t.Errorf("mcp past the bound: %d Retry-After %q %q, want 503, 1 and one line",
			resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("a busy refusal reached the upstream: %d requests", n)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-a", "status=503", "check=busy"); len(lines) != 1 {
		t.Errorf("the audit lines of the busy refusal: %q", lines)
	}
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-b/mcp/nofire", rig.mcpToken(t, "brig-b"), rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("brig-b while brig-a is busy: %d %v, want 200", got, err)
	}
	for range maxMCPPerSandbox {
		rig.srv.mcpGate.release("brig-a")
	}
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("mcp once the slots are free: %d %v, want 200", got, err)
	}
}

// stalledMCPRequest opens a guest connection to sock, sends the headers of
// an MCP request and half its body, and stalls.
func stalledMCPRequest(t *testing.T, sock, internal string, pool *x509.CertPool, id, token string) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(raw, "BRIG-DIVERT/1 %s 443 10.100.0.2 40000\n", internal); err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: injector.BrigInternal})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	body := rpcCallSearch
	if _, err := fmt.Fprintf(c, "POST /%s/mcp/nofire HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Authorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", id, injector.BrigInternal, token,
		len(body), body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	return c
}

// A guest that trickles its body holds no MCP slot while it does, and is cut
// off at the deadline without reaching the upstream.
func TestStalledMCPBodyHoldsNoSlot(t *testing.T) {
	// Put back after the broker's own cleanup, which runs first, so no
	// handler still reads them.
	saved, savedHook := memoryReadTimeout, beforeMCPRead
	t.Cleanup(func() { memoryReadTimeout, beforeMCPRead = saved, savedHook })
	memoryReadTimeout = 500 * time.Millisecond
	var reading atomic.Int32
	beforeMCPRead = func() { reading.Add(1) }
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	pool := caPool(t, a)
	var stalled []*tls.Conn
	for range maxMCPPerSandbox + 1 {
		stalled = append(stalled, stalledMCPRequest(t, a.DivertSocket, internalA, pool, "brig-a", tok))
	}
	// Every stalled request is past the token check and reading its body
	// before this one is sent, so it is served beside them.
	waitFor(t, "the stalled requests to read their bodies", func() bool {
		return reading.Load() == int32(len(stalled))
	})
	c := guestClient(a.DivertSocket, internalA+":443", pool)
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("a request beside %d stalled ones: %d %v, want 200", len(stalled), got, err)
	}
	_ = stalled[0].SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(stalled[0]), nil)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("a stalled body was answered %d, want it cut off", resp.StatusCode)
		}
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the broker did not cut off a stalled body within 5 s")
	}
	if n := len(rig.upstream()); n != 1 {
		t.Errorf("the upstream saw %d requests, want the 1 complete one", n)
	}
}

// waitFor polls cond until it holds, and fails the test after 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitWithin(t, 5*time.Second, what, cond)
}

// waitWithin polls cond until it holds, and fails the test after d.
func waitWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// inFlight is how many MCP calls of sandbox id hold a slot.
func (r *running) inFlight(id string) int {
	r.srv.mcpGate.mu.Lock()
	defer r.srv.mcpGate.mu.Unlock()
	return r.srv.mcpGate.per[id]
}

// rpcCallHang is a call the hanging upstream holds until released.
const rpcCallHang = `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search","arguments":{"q":"hang"}}}`

// hangingUpstream answers a call for "hang" only once release is closed or
// the broker gives up on it, and every other call at once.
func hangingUpstream(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"hang"`) {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		mcpResult(w, r)
	}
}

// MCP calls are bounded per sandbox alone: a sandbox with every slot held
// by calls that do not finish leaves another sandbox served, and its own
// next call is answered 503 at once.
func TestMCPSandboxAtItsLimitDoesNotStopAnother(t *testing.T) {
	release := make(chan struct{})
	rig := newMCPRig(t, hangingUpstream(release))
	// Cleanups run last first: the held calls are released, then waited
	// for, then the broker stops.
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(func() { close(release) })
	a := rig.addMCP(t, "brig-a", internalA)
	b := rig.addMCP(t, "brig-b", internalB)
	tokA := rig.mcpToken(t, "brig-a")
	ca := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	for range maxMCPPerSandbox {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = rpc(ca, "/brig-a/mcp/nofire", tokA, rpcCallHang)
		}()
	}
	waitFor(t, "brig-a's calls to hold every slot", func() bool { return rig.inFlight("brig-a") == maxMCPPerSandbox })
	cb := guestClient(b.DivertSocket, internalB+":443", caPool(t, b))
	if got, _, err := rpc(cb, "/brig-b/mcp/nofire", rig.mcpToken(t, "brig-b"), rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("brig-b while brig-a holds %d calls: %d %v, want 200", maxMCPPerSandbox, got, err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+"/brig-a/mcp/nofire",
		strings.NewReader(rpcCallSearch))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokA)
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("brig-a's call past its bound: %d Retry-After %q, want 503 and 1", resp.StatusCode,
			resp.Header.Get("Retry-After"))
	}
}

// A call whose upstream never finishes is cut at the call timeout, and its
// slot is given back.
func TestMCPCallIsCutAtTheTimeout(t *testing.T) {
	saved := mcpCallTimeout
	t.Cleanup(func() { mcpCallTimeout = saved })
	mcpCallTimeout = 300 * time.Millisecond
	release := make(chan struct{})
	rig := newMCPRig(t, hangingUpstream(release))
	t.Cleanup(func() { close(release) })
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	start := time.Now()
	got, body, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallHang)
	if err == nil && got == http.StatusOK && strings.Contains(body, "found it") {
		t.Errorf("a call that never finished was answered: %d %q", got, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the call was cut after %v, want about %v", d, mcpCallTimeout)
	}
	waitFor(t, "the cut call's slot to be given back", func() bool { return rig.inFlight("brig-a") == 0 })
	if got, _, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("a call after the cut one: %d %v, want 200", got, err)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-a", "verdict=error"); len(lines) != 1 {
		t.Errorf("the audit lines of the cut call: %q", lines)
	}
}

// A tools/call answered as application/json sends no header until the tool
// is done, so the broker's own upstream transport waits for headers as long
// as the call may run. A shorter wait would cut a slow tool with 502 well
// before the call timeout.
func TestMCPTransportWaitsForHeadersAsLongAsTheCall(t *testing.T) {
	p := (&server{}).newMCPProxy(nil)
	tr, ok := p.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the proxy's transport is %T, want *http.Transport", p.Transport)
	}
	if d := tr.ResponseHeaderTimeout; d != 0 && d < mcpCallTimeout {
		t.Errorf("the transport gives up on response headers after %v, before the call timeout %v", d, mcpCallTimeout)
	}
}

// An upstream that sends its headers late, with the answer, is still
// answered while the call is inside its timeout.
func TestMCPCallWithLateHeadersIsAnswered(t *testing.T) {
	saved := mcpCallTimeout
	t.Cleanup(func() { mcpCallTimeout = saved })
	mcpCallTimeout = 5 * time.Second
	rig := newMCPRig(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		mcpResult(w, r)
	})
	a := rig.addMCP(t, "brig-a", internalA)
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	got, body, err := rpc(c, "/brig-a/mcp/nofire", rig.mcpToken(t, "brig-a"), rpcCallSearch)
	if err != nil || got != http.StatusOK || !strings.Contains(body, "found it") {
		t.Errorf("a call whose headers came after 1 s: %d %q %v, want 200 and the answer", got, body, err)
	}
}

// A guest that sends a call and never reads the answer is cut at the call
// timeout too: the write deadline unblocks a write the full socket holds,
// which cancelling the context alone does not, and the slot is given back.
func TestMCPCallIsCutWhenTheGuestStopsReading(t *testing.T) {
	saved := mcpCallTimeout
	t.Cleanup(func() { mcpCallTimeout = saved })
	mcpCallTimeout = 300 * time.Millisecond
	pad := strings.Repeat("x", 8<<10)
	event := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"pad\":\"" + pad + "\"}}\n\n"
	rig := newMCPRig(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"hang"`) {
			mcpResult(w, r)
			return
		}
		// Streams for as long as the broker reads.
		w.Header().Set("Content-Type", "text/event-stream")
		for r.Context().Err() == nil {
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	pool := caPool(t, a)
	raw, err := net.Dial("unix", a.DivertSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(raw, "BRIG-DIVERT/1 %s 443 10.100.0.2 40000\n", internalA); err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: injector.BrigInternal})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	// The whole call is sent, and not a byte of the answer is read.
	if _, err := fmt.Fprintf(c, "POST /brig-a/mcp/nofire HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Accept: application/json, text/event-stream\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
		injector.BrigInternal, tok, len(rpcCallHang), rpcCallHang); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to take its slot", func() bool { return rig.inFlight("brig-a") == 1 })
	// The failed write closes the connection, and crypto/tls gives its
	// close_notify alert 5 s of its own to go out to the guest that is not
	// reading, so the slot comes back up to 5 s after the call timeout.
	// Without the write deadline it never does.
	waitWithin(t, 10*time.Second, "the unread call's slot to be given back",
		func() bool { return rig.inFlight("brig-a") == 0 })
	g := guestClient(a.DivertSocket, internalA+":443", pool)
	if got, _, err := rpc(g, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK {
		t.Errorf("a call after the unread one: %d %v, want 200", got, err)
	}
}
