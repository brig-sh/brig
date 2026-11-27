package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
)

const (
	// mcpSecretName and mcpSecretValue are the credential of the MCP
	// server in these tests, as the store holds it.
	mcpSecretName  = "nofire-token"
	mcpSecretValue = "nofire-secret-value"
	rpcCallSearch  = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"q":"SECRETQUERY"}}}`
	rpcCallDelete  = `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_everything","arguments":{}}}`
)

// mcpRig is a broker in this process, with an upstream of its own that
// every MCP server and route reaches.
type mcpRig struct {
	*running
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
	rig := &mcpRig{}
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
	cfg := serverConfig{dir: shortDir(t), logTo: io.Discard, transport: transportTo(up)}
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

// mcpServersOf is the MCP registration of sandbox id, as the broker holds
// it.
func (r *running) mcpServersOf(t *testing.T, id string) mcpServers {
	t.Helper()
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	reg, ok := r.srv.serviceRegs[mcpKind][id]
	if !ok {
		t.Fatalf("sandbox %s has no mcp registration", id)
	}
	return reg.data.(mcpServers)
}

func refusedStatus(err error) int {
	var ref *injector.RefusedError
	if errors.As(err, &ref) {
		return ref.Status
	}
	return 0
}

// rpc sends one JSON-RPC body to https://brig.internal<path> and returns the
// status and the body.
func rpc(c *http.Client, path, token, body string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, "https://"+injector.BrigInternal+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

// addRoutesAndMCP registers brig-a for routes and MCP at internalA, and
// returns the answer to the routes' registration.
func (r *mcpRig) addRoutesAndMCP(t *testing.T) injector.AddResponse {
	t.Helper()
	added, err := r.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	r.addMCP(t, "brig-a", internalA)
	return added
}

// A route token of the same sandbox is refused by MCP as a bad token, and
// so is none, and neither reaches the upstream.
func TestMCPRefusesRouteTokens(t *testing.T) {
	rig := newMCPRig(t, nil)
	added := rig.addRoutesAndMCP(t)
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	for name, tok := range map[string]string{"route": rig.mint(t, "brig-a", "anthropic"), "no": ""} {
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

// An MCP token is refused by a route.
func TestRoutesRefuseMCPToken(t *testing.T) {
	rig := newMCPRig(t, nil)
	added := rig.addRoutesAndMCP(t)
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	tok := rig.mcpToken(t, "brig-a")
	if got, err := post(c, "/brig-a/r/anthropic/v1/messages", tok); err != nil || got != http.StatusUnauthorized {
		t.Errorf("an mcp token on a route: %d %v, want 401", got, err)
	}
	if n := len(rig.upstream()); n != 0 {
		t.Errorf("the upstream saw %d requests, want 0", n)
	}
}

// A host route is named by its Host, and its path is the upstream's: a path
// that looks like an MCP server's is the route's, and reaches the upstream
// with the route's token. The MCP token opens nothing there.
func TestHostRouteWithAnMCPPathReachesTheRoute(t *testing.T) {
	rig := newMCPRig(t, nil)
	if err := rig.store.Update("gh-token", []byte("gh-secret-value")); err != nil {
		t.Fatal(err)
	}
	gh := injector.Route{Policy: "gh", Route: v1alpha1.Route{Name: "gh", Mode: v1alpha1.ModeHost,
		Upstream:   "https://api.github.example",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gh-token"}},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}}}},
		Guest: &v1alpha1.Guest{Env: map[string]string{"GH_TOKEN": "{session.token}"}}}}
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{gh}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := onSocket(t, added, internalA)
	for _, host := range []string{"gh.brig.internal", "GH.Brig.Internal.", "gh.brig.internal:443"} {
		if got := call(t, c, host, "/brig-a/mcp/nofire", tok); got.code != http.StatusUnauthorized ||
			got.body != "brig broker: the session token was refused\n" {
			t.Errorf("an mcp token at %s = %+v, want the route's 401", host, got)
		}
	}
	if n := len(rig.upstream()); n != 0 {
		t.Fatalf("the upstream saw %d requests for an mcp token", n)
	}
	if got := call(t, c, "gh.brig.internal", "/x/mcp/nofire", rig.mint(t, "brig-a", "gh")); got.code != http.StatusOK {
		t.Errorf("the route's token at gh.brig.internal/x/mcp/nofire = %+v, want 200 from the upstream", got)
	}
	seen := rig.upstream()
	if len(seen) != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", len(seen))
	}
	if seen[0].Path != "/x/mcp/nofire" || seen[0].Header.Get("Authorization") != "Bearer gh-secret-value" {
		t.Errorf("the upstream saw %s with Authorization %q, want the route's path and credential",
			seen[0].Path, seen[0].Header.Get("Authorization"))
	}
	if lines := auditWith(t, rig.dir, "kind=mcp"); len(lines) != 0 {
		t.Errorf("a route request wrote an mcp audit line: %q", lines)
	}
}

// A server the sandbox did not register is not found, after the token check,
// and so is anything below a server's path.
func TestMCPUnknownServerName(t *testing.T) {
	rig := newMCPRig(t, nil)
	a := rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(a.DivertSocket, internalA+":443", caPool(t, a))
	for _, path := range []string{"/brig-a/mcp/other", "/brig-a/mcp/", "/brig-a/mcp", "/brig-a/mcp/nofire/x", "/brig-a/mcp/Nofire"} {
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
	reserved.Server.Name = v1alpha1.MCPReservedName
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
	if st, err := rig.client.Status(); err != nil || len(st.Services[mcpKind]) != 0 {
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
		if got := rig.mcpServersOf(t, id).servers["nofire"].URL; got != "https://mcp.nofire.ai/mcp" {
			t.Errorf("%s: the preset server is served at %q", id, got)
		}
	}
}

// A call the policy allows reaches the upstream with the credential from
// the store in place of the guest's token, and the guest's answer holds
// neither. A call the policy does not name never reaches the upstream. Each
// has one audit line, which holds no argument, credential or token. The
// registration, the token and the call go through the control socket and
// the divert socket as brig's do.
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
	if auth := seen[0].Header.Values("Authorization"); len(auth) != 1 || auth[0] != "Bearer "+mcpSecretValue {
		t.Errorf("the upstream's Authorization = %q, want the credential alone", auth)
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

// MCP requests past the bound of one sandbox are answered 503 at once,
// reach no upstream, and leave another sandbox served.
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
	saved, savedHook := mcpReadTimeout, beforeMCPRead
	t.Cleanup(func() { mcpReadTimeout, beforeMCPRead = saved, savedHook })
	mcpReadTimeout = 500 * time.Millisecond
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

// Revoking a sandbox's MCP registration cuts its call in flight, though the
// routes keep the socket and the guest's connection open: the call must not
// run on with the revoked servers and credential.
func TestRevokingMCPCutsACallInFlight(t *testing.T) {
	release := make(chan struct{})
	rig := newMCPRig(t, hangingUpstream(release))
	t.Cleanup(func() { close(release) })
	added := rig.addRoutesAndMCP(t)
	tok := rig.mcpToken(t, "brig-a")
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	type result struct {
		code int
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, body, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallHang)
		done <- result{code, body, err}
	}()
	waitFor(t, "the call to take its slot", func() bool { return rig.inFlight("brig-a") == 1 })
	if _, err := rig.client.RemoveService(mcpKind, "brig-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err == nil && got.code == http.StatusOK && strings.Contains(got.body, "found it") {
			t.Errorf("the revoked call was answered: %d %q", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call still runs after MCP was revoked")
	}
	waitFor(t, "the revoked call's slot to be given back", func() bool { return rig.inFlight("brig-a") == 0 })
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

// A token is served only on the divert socket of its own sandbox: brig-a's
// token for brig-a's server arriving on brig-b's socket is not found, and
// brig-a's token for brig-b's server is not brig-b's token. Each refusal
// leaves an MCP audit line naming the socket it arrived on, and none holds
// the token.
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
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-b", "path=brig-a", "server=nofire",
		"status=404", "check=arrival"); len(lines) != 1 {
		t.Errorf("the audit lines of brig-b asking for brig-a: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "kind=mcp", "sandbox=brig-b", "path=brig-b", "server=nofire",
		"status=401", "check=token"); len(lines) != 1 {
		t.Errorf("the audit lines of brig-a's token on brig-b: %q", lines)
	}
	if lines := auditWith(t, rig.dir, "sandbox=brig-a"); len(lines) != 0 {
		t.Errorf("a request of brig-b wrote a line naming brig-a as its sandbox: %q", lines)
	}
	blob, err := os.ReadFile(filepath.Join(rig.dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), tokA) || strings.Contains(string(blob), "SECRETQUERY") {
		t.Errorf("the audit log holds the token or the body: %q", blob)
	}
}

// MCP is served on brig.internal alone. The same request with a valid token
// on the link-local address, which is clear text, is not found, reaches no
// upstream, and leaves an arrival line.
func TestMCPServedOnlyOnBrigInternal(t *testing.T) {
	rig := newMCPRig(t, nil)
	added, err := rig.client.Add(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}, BrigInternal: internalA})
	if err != nil {
		t.Fatal(err)
	}
	rig.addMCP(t, "brig-a", internalA)
	tok := rig.mcpToken(t, "brig-a")
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
	c := guestClient(added.DivertSocket, internalA+":443", caPool(t, added))
	if got, body, err := rpc(c, "/brig-a/mcp/nofire", tok, rpcCallSearch); err != nil || got != http.StatusOK ||
		!strings.Contains(body, "found it") {
		t.Errorf("mcp on brig.internal: %d %q %v, want 200", got, body, err)
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

// The broker signs MCP tokens with a key file of their own, mcpkey: 0600,
// of a host key's size, and not the host key.
func TestMCPKeyIsItsOwn(t *testing.T) {
	rig := newMCPRig(t, nil)
	mcpKey, err := os.ReadFile(filepath.Join(rig.dir, injector.MCPKeyName))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(rig.dir, injector.MCPKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != hostKeySize {
		t.Errorf("mcp key mode %v size %d, want 0600 and %d", info.Mode().Perm(), info.Size(), hostKeySize)
	}
	hostKey, err := os.ReadFile(filepath.Join(rig.dir, injector.HostKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hostKey, mcpKey) {
		t.Error("the mcp key is the host key")
	}
	rig.srv.mu.Lock()
	svcKey := rig.srv.services[mcpKind].hostKey
	rig.srv.mu.Unlock()
	if !bytes.Equal(svcKey, mcpKey) {
		t.Error("the mcp service does not sign with mcpkey")
	}
}
