package policy

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

func nofireServer() v1alpha1.MCPServer {
	return v1alpha1.MCPServer{
		Name:       "nofire",
		Preset:     v1alpha1.MCPPresetNofire,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "nofire-token"}},
		Tools:      &v1alpha1.MCPTools{Allow: []string{"search", "get"}},
	}
}

func TestExpandMCPNofirePreset(t *testing.T) {
	got, err := ExpandMCP(nofireServer())
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://mcp.nofire.ai/mcp" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Transport != v1alpha1.MCPTransportHTTP {
		t.Errorf("transport = %q", got.Transport)
	}
	if got.Inject == nil || got.Inject.Headers == nil || len(got.Inject.Headers.Set) != 1 {
		t.Fatalf("inject = %+v", got.Inject)
	}
	if h := got.Inject.Headers.Set[0]; h.Name != "Authorization" || h.Value != "Bearer {value}" {
		t.Errorf("header = %+v", h)
	}
	if got.Tools == nil || strings.Join(got.Tools.Allow, ",") != "search,get" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if got.Credential == nil || got.Credential.SecretRef.Name != "nofire-token" {
		t.Errorf("credential = %+v", got.Credential)
	}
}

// Each expansion decodes the preset afresh, so a caller changing the result
// cannot reach the next server's header.
func TestExpandMCPSharesNothingBetweenCalls(t *testing.T) {
	a, err := ExpandMCP(nofireServer())
	if err != nil {
		t.Fatal(err)
	}
	a.Inject.Headers.Set[0].Value = "changed"
	b, err := ExpandMCP(nofireServer())
	if err != nil {
		t.Fatal(err)
	}
	if b.Inject.Headers.Set[0].Value != "Bearer {value}" {
		t.Errorf("second expansion saw the first's change: %q", b.Inject.Headers.Set[0].Value)
	}
}

// Tools are the policy author's decision, so a preset that carried them would
// grant tools nobody listed.
func TestExpandMCPPresetCannotSetTools(t *testing.T) {
	for _, field := range []string{"tools:\n  allow: [x]", "credential:\n  secretRef:\n    name: s", "name: other"} {
		blob := "transport: http\nurl: https://mcp.example.com/mcp\n" + field + "\n"
		if _, err := parseMCPPreset("test", []byte(blob)); err == nil {
			t.Errorf("a preset file with %q loaded", field)
		}
	}
	good := "transport: http\nurl: https://mcp.example.com/mcp\n"
	if _, err := parseMCPPreset("test", []byte(good)); err != nil {
		t.Errorf("a plain preset file was refused: %v", err)
	}
}

func TestExpandMCPPresetFieldsLocked(t *testing.T) {
	set := map[string]func(*v1alpha1.MCPServer){
		"url": func(s *v1alpha1.MCPServer) { s.URL = "https://evil.example.com/mcp" },
		"inject": func(s *v1alpha1.MCPServer) {
			s.Inject = &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
				Set: []v1alpha1.Header{{Name: "X-Key", Value: "{value}"}}}}
		},
	}
	for field, mutate := range set {
		s := nofireServer()
		mutate(&s)
		_, err := ExpandMCP(s)
		if err == nil {
			t.Errorf("%s: a server with a preset set it", field)
			continue
		}
		if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "nofire") {
			t.Errorf("%s: error does not name the field and preset: %v", field, err)
		}
	}
}

// The nofire preset sends the credential in a header, so a server that names
// it with no credential is refused at load rather than failing every call.
func TestExpandMCPPresetNeedsACredential(t *testing.T) {
	s := nofireServer()
	s.Credential = nil
	_, err := ExpandMCP(s)
	if err == nil || !strings.Contains(err.Error(), "names no credential") {
		t.Errorf("a preset server with no credential: %v, want it refused", err)
	}
}

func TestExpandMCPWithoutPresetValidates(t *testing.T) {
	s := v1alpha1.MCPServer{Name: "x", Transport: v1alpha1.MCPTransportHTTP, URL: "https://m.example.com/mcp"}
	if _, err := ExpandMCP(s); err == nil {
		t.Error("a server with no tools expanded")
	}
}

func TestExpandMCPUnknownPreset(t *testing.T) {
	s := nofireServer()
	s.Preset = "nope"
	if _, err := ExpandMCP(s); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
}

func mcpPolicy(name string, servers ...v1alpha1.MCPServer) Policy {
	return Policy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: APIVersion, Kind: v1alpha1.Kind},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.PolicySpec{MCP: servers},
	}
}

func TestMCPServersRefusesDuplicateAcrossPolicies(t *testing.T) {
	_, err := MCPServers([]Policy{mcpPolicy("one", nofireServer()), mcpPolicy("two", nofireServer())})
	if err == nil {
		t.Fatal("two policies declared the same server name")
	}
	if !strings.Contains(err.Error(), "one") || !strings.Contains(err.Error(), "two") {
		t.Errorf("error does not name both policies: %v", err)
	}
}

func TestMCPServersRefusesBrigMemory(t *testing.T) {
	s := nofireServer()
	s.Name = "brig-memory"
	if _, err := MCPServers([]Policy{mcpPolicy("one", s)}); err == nil ||
		!strings.Contains(err.Error(), "brig-memory") {
		t.Errorf("err = %v", err)
	}
}

func TestMCPServersExpandsInOrder(t *testing.T) {
	b := nofireServer()
	b.Name = "second"
	got, err := MCPServers([]Policy{mcpPolicy("one", nofireServer()), mcpPolicy("two", b)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Policy != "one" || got[0].Server.Name != "nofire" ||
		got[1].Policy != "two" || got[1].Server.URL != "https://mcp.nofire.ai/mcp" {
		t.Errorf("entries = %+v", got)
	}
}

// Expand returns the servers expanded, and shares none of the caller's.
func TestExpandExpandsMCP(t *testing.T) {
	p := mcpPolicy("one", nofireServer())
	e, err := Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.MCP[0].URL == "" {
		t.Error("Expand left the server unexpanded")
	}
	if p.Spec.MCP[0].URL != "" {
		t.Error("Expand changed the caller's server")
	}
}

func TestParseRefusesABrokenMCPPreset(t *testing.T) {
	doc := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: p\nspec:\n  mcp:\n" +
		"    - name: n\n      preset: nofire\n      tools:\n        allow: [a]\n      credential:\n" +
		"        secretRef:\n          name: s\n      url: https://evil.example.com/mcp\n"
	if _, err := Parse([]byte(doc)); err == nil {
		t.Error("Parse accepted a preset server that sets url")
	}
}
