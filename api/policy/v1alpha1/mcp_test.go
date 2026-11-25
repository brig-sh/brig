package v1alpha1

import (
	"strings"
	"testing"
)

// mcpPolicy wraps one spec.mcp server in a policy document.
func mcpPolicy(server string) []byte {
	return []byte("apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata:\n  name: p\nspec:\n  mcp:\n" +
		"    - " + strings.ReplaceAll(strings.TrimSpace(server), "\n", "\n      ") + "\n")
}

// decodeValidate decodes doc and validates it, returning the first error.
func decodeValidate(t *testing.T, doc []byte) error {
	t.Helper()
	p, err := Decode(doc)
	if err != nil {
		return err
	}
	return p.Validate()
}

func wantMCPError(t *testing.T, doc []byte, want string) {
	t.Helper()
	err := decodeValidate(t, doc)
	if err == nil {
		t.Fatalf("accepted, want an error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

const mcpOK = `
name: docs
transport: http
url: https://mcp.example.com/mcp
tools:
  allow: [search]
`

func TestMCPValid(t *testing.T) {
	if err := decodeValidate(t, mcpPolicy(mcpOK)); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHTTPNeedsToolList(t *testing.T) {
	for name, tools := range map[string]string{
		"missing": "",
		"empty":   "tools: {allow: []}",
	} {
		t.Run(name, func(t *testing.T) {
			wantMCPError(t, mcpPolicy("name: docs\ntransport: http\nurl: https://h/mcp\n"+tools),
				"tools.allow is required")
		})
	}
}

func TestMCPHTTPURL(t *testing.T) {
	for url, want := range map[string]string{
		"http://h/mcp":        "https",
		"https://u:p@h/mcp":   "user name or password",
		"https://h/mcp?k=v":   "query",
		"https://h/mcp?":      "query",
		"https://h/mcp#frag":  "fragment",
		"https://H.example/m": "lower case",
	} {
		t.Run(url, func(t *testing.T) {
			wantMCPError(t, mcpPolicy("name: docs\ntransport: http\nurl: '"+url+"'\ntools: {allow: [a]}"), want)
		})
	}
	wantMCPError(t, mcpPolicy("name: docs\ntransport: http\ntools: {allow: [a]}"), "url is required")
}

func TestMCPTransportHTTPOnly(t *testing.T) {
	wantMCPError(t, mcpPolicy("name: docs\ntransport: stdio\ntools: {allow: [a]}"), "is not supported; use http")
	wantMCPError(t, mcpPolicy("name: docs\ntransport: grpc\ntools: {allow: [a]}"), "is not supported; use http")
	wantMCPError(t, mcpPolicy("name: docs\nurl: https://h/mcp\ntools: {allow: [a]}"), "transport is required")
}

func TestMCPNameRules(t *testing.T) {
	body := "\ntransport: http\nurl: https://h/mcp\ntools: {allow: [a]}"
	wantMCPError(t, mcpPolicy("name: Docs"+body), "may use only")
	wantMCPError(t, mcpPolicy("name: a_b"+body), "may use only")
	wantMCPError(t, mcpPolicy("name: brig-memory"+body), "reserved")
	wantMCPError(t, mcpPolicy(body), "name is required")

	dup := string(mcpPolicy("name: docs"+body)) +
		"    - name: docs\n      transport: http\n      url: https://h/mcp\n      tools: {allow: [a]}\n"
	wantMCPError(t, []byte(dup), "declared twice")
}

func TestMCPToolsAllows(t *testing.T) {
	for _, tc := range []struct {
		allow []string
		tool  string
		want  bool
	}{
		{[]string{"a"}, "a", true},
		{[]string{"A"}, "a", false},
		{[]string{"a "}, "a", false},
		{[]string{}, "a", false},
		{[]string{"*"}, "a", true},
		{[]string{"b", "*"}, "a", true},
	} {
		if got := (&MCPTools{Allow: tc.allow}).Allows(tc.tool); got != tc.want {
			t.Errorf("Allows(%q) over %q = %v, want %v", tc.tool, tc.allow, got, tc.want)
		}
	}
	var nilTools *MCPTools
	if nilTools.Allows("a") {
		t.Error("a nil tool list allowed a tool")
	}
	wantMCPError(t, mcpPolicy("name: docs\ntransport: http\nurl: https://h/mcp\ntools: {allow: ['a ']}"),
		"leading or trailing space")
}

func TestMCPInjectHeadersOnly(t *testing.T) {
	head := "name: docs\ntransport: http\nurl: https://h/mcp\ntools: {allow: [a]}\n"
	wantMCPError(t, mcpPolicy(head+`inject:
  tls:
    ca:
      secretRef: {name: ca}`), "only headers")
	wantMCPError(t, mcpPolicy(head+`inject:
  basicAuth: {username: u}`), "only headers")

	ok := head + `credential:
  secretRef: {name: tok}
inject:
  headers:
    set:
      - {name: Authorization, value: "Bearer {value}"}`
	if err := decodeValidate(t, mcpPolicy(ok)); err != nil {
		t.Fatal(err)
	}
	// A credential that no header carries would never reach upstream.
	wantMCPError(t, mcpPolicy(head+"credential:\n  secretRef: {name: tok}"), "inject.headers.set")
	wantMCPError(t, mcpPolicy(head+`credential:
  secretRef: {name: tok}
inject:
  headers:
    set:
      - {name: X-Key, value: fixed}`), "{value}")
}

// A header that holds {value} with no credential to fill it would load, then
// fail every request upstream.
func TestMCPValueHeaderNeedsACredential(t *testing.T) {
	head := "name: docs\ntransport: http\nurl: https://h/mcp\ntools: {allow: [a]}\n"
	wantMCPError(t, mcpPolicy(head+`inject:
  headers:
    set:
      - {name: Authorization, value: "Bearer {value}"}`), "needs credential.secretRef")
	fixed := head + `inject:
  headers:
    set:
      - {name: X-Client, value: brig}`
	if err := decodeValidate(t, mcpPolicy(fixed)); err != nil {
		t.Errorf("a fixed header with no credential was refused: %v", err)
	}
}

// The proxy reads no field of a secret document. Accepted, the policy would
// fail on every request instead of at load.
func TestMCPCredentialFieldRefused(t *testing.T) {
	wantMCPError(t, mcpPolicy(`name: docs
transport: http
url: https://h/mcp
tools: {allow: [a]}
credential:
  secretRef: {name: tok}
  field: accessToken
inject:
  headers:
    set:
      - {name: Authorization, value: "Bearer {value}"}`), "whole secret value")
}

func TestMCPPresetUnknown(t *testing.T) {
	wantMCPError(t, mcpPolicy("name: docs\npreset: other\ntools: {allow: [a]}"), "nofire")
}

// A preset fills transport, url and inject, so the written form omits them.
// It never fills tools.
func TestMCPPresetNofire(t *testing.T) {
	doc := `
name: nofire
preset: nofire
credential:
  secretRef: {name: nofire-token}
tools:
  allow: [query]
`
	if err := decodeValidate(t, mcpPolicy(doc)); err != nil {
		t.Fatal(err)
	}
	wantMCPError(t, mcpPolicy("name: nofire\npreset: nofire"), "tools.allow is required")

	// Expanded, nothing is left to fill.
	p, _ := Decode(mcpPolicy(doc))
	if err := p.Spec.MCP[0].Validate(); err == nil || !strings.Contains(err.Error(), "transport is required") {
		t.Fatalf("Validate of an unexpanded preset = %v, want transport is required", err)
	}
}

func TestMCPUnknownKeysRefused(t *testing.T) {
	wantMCPError(t, mcpPolicy(mcpOK+"command: [x]"), "unknown field")
	wantMCPError(t, mcpPolicy(mcpOK+"tools:\n  allow: [a]\n  deny: [b]"), "already set")
	wantMCPError(t, mcpPolicy(strings.Replace(mcpOK, "allow: [search]", "allow: [search]\n  deny: [b]", 1)),
		"unknown field")
}

func TestMCPOnlyPolicy(t *testing.T) {
	if err := decodeValidate(t, mcpPolicy(mcpOK)); err != nil {
		t.Fatalf("a policy with only spec.mcp: %v", err)
	}
}

func TestMCPServerDeepCopy(t *testing.T) {
	p, err := Decode(mcpPolicy(`
name: docs
transport: http
url: https://h/mcp
credential:
  secretRef: {name: tok}
inject:
  headers:
    set:
      - {name: Authorization, value: "Bearer {value}"}
tools: {allow: [a]}
`))
	if err != nil {
		t.Fatal(err)
	}
	orig := &p.Spec.MCP[0]
	cp := orig.DeepCopy()
	cp.Tools.Allow[0] = "changed"
	cp.Credential.SecretRef.Name = "changed"
	cp.Inject.Headers.Set[0].Name = "changed"
	if orig.Tools.Allow[0] != "a" || orig.Credential.SecretRef.Name != "tok" || orig.Inject.Headers.Set[0].Name != "Authorization" {
		t.Fatal("DeepCopy shares memory with the original")
	}
	var nilServer *MCPServer
	if nilServer.DeepCopy() != nil {
		t.Fatal("DeepCopy of nil is not nil")
	}
}
