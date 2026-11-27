package v1alpha1

import "slices"

// MCPTransport is how the broker reaches an MCP server.
type MCPTransport string

// MCPTransportHTTP is a remote MCP server over HTTP. It is the only
// transport so far; stdio is a follow-up.
const MCPTransportHTTP MCPTransport = "http"

// MCPPreset names a ready-made server, which fills the fields that the
// preset knows. The presets are not part of this package.
type MCPPreset string

// MCPPresetNofire is the NOFire AI MCP server.
const MCPPresetNofire MCPPreset = "nofire"

// MCPAllTools in tools.allow admits every tool of the server.
const MCPAllTools = "*"

// MCPReservedName is the server name that brig serves its own memory store
// under, so a policy cannot take it.
const MCPReservedName = "brig-memory"

// MCPServer is one MCP server that the guest may call through the broker.
type MCPServer struct {
	// Name is required, and is unique within the policy. It follows the
	// route name grammar, and is not brig-memory.
	Name string `json:"name"`
	// Preset is optional. It fills Transport, URL and Inject, and a server
	// that names one does not set URL or Inject itself.
	Preset MCPPreset `json:"preset,omitempty"`
	// Transport is required without a preset, and is "http".
	Transport MCPTransport `json:"transport,omitempty"`
	// URL is required for http without a preset. It is an https:// URL of
	// the MCP endpoint, with no user name, password or query.
	URL string `json:"url,omitempty"`
	// Credential is optional. It names the secret that Inject sends
	// upstream. The secret is never delivered to the guest.
	Credential *CredentialRef `json:"credential,omitempty"`
	// Inject is optional. Only inject.headers is accepted.
	Inject *Inject `json:"inject,omitempty"`
	// Tools is required. A preset never provides it.
	Tools *MCPTools `json:"tools,omitempty"`
}

// MCPTools is the tool allowlist of a server.
type MCPTools struct {
	// Allow is required and not empty. Each entry is a tool name matched
	// exactly, or "*" for every tool.
	Allow []string `json:"allow"`
}

// Allows reports whether tool is on the list: an exact match, or "*".
func (t *MCPTools) Allows(tool string) bool {
	if t == nil {
		return false
	}
	return slices.Contains(t.Allow, MCPAllTools) || slices.Contains(t.Allow, tool)
}

// DeepCopy returns a copy of s that shares no map, slice or pointer with it.
func (s *MCPServer) DeepCopy() *MCPServer {
	if s == nil {
		return nil
	}
	out := *s
	// Route.DeepCopy owns the copy of the shared credential and inject
	// types, so the two copies cannot drift.
	c := (&Route{Credential: s.Credential, Inject: s.Inject}).DeepCopy()
	out.Credential, out.Inject = c.Credential, c.Inject
	if s.Tools != nil {
		out.Tools = &MCPTools{Allow: slices.Clone(s.Tools.Allow)}
	}
	return &out
}
