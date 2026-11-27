package policy

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// mcpPresetFiles holds the MCP presets brig ships. They sit apart from the
// route presets, whose loader requires a route and a modes list that an MCP
// server has no use for.
//
//go:embed mcppresets/*.yaml
var mcpPresetFiles embed.FS

// mcpPresetFile is one MCP preset as its file writes it. It has no name,
// credential or tools field, so the strict decoder refuses a file that sets
// one: the policy author names the secret and the tools, and a preset that
// carried either would grant what nobody listed.
type mcpPresetFile struct {
	Transport v1alpha1.MCPTransport `json:"transport"`
	URL       string                `json:"url"`
	Inject    *v1alpha1.Inject      `json:"inject,omitempty"`
}

// MCPPresetNames lists the MCP presets brig ships, sorted.
func MCPPresetNames() []v1alpha1.MCPPreset {
	entries, err := mcpPresetFiles.ReadDir("mcppresets")
	if err != nil {
		return nil
	}
	var names []v1alpha1.MCPPreset
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, v1alpha1.MCPPreset(name))
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func mcpPresetList() string {
	var names []string
	for _, n := range MCPPresetNames() {
		names = append(names, string(n))
	}
	return strings.Join(names, ", ")
}

// parseMCPPreset decodes the text of a preset file.
func parseMCPPreset(name string, blob []byte) (mcpPresetFile, error) {
	var p mcpPresetFile
	if err := v1alpha1.UnmarshalStrict(blob, &p); err != nil {
		return mcpPresetFile{}, fmt.Errorf("mcp preset %s: %w", name, err)
	}
	return p, nil
}

// decodeMCPPreset reads one preset file. Each call decodes it again, so a
// caller can change the result without reaching the next one.
func decodeMCPPreset(name v1alpha1.MCPPreset) (mcpPresetFile, error) {
	blob, err := mcpPresetFiles.ReadFile("mcppresets/" + string(name) + ".yaml")
	if err != nil {
		return mcpPresetFile{}, fmt.Errorf("mcp preset %q is not one brig ships (%s)", name, mcpPresetList())
	}
	return parseMCPPreset(string(name), blob)
}

// ExpandMCP fills s from its preset and validates the result with
// MCPServer.Validate. The result shares no map, slice or pointer with s.
//
// A server with a preset may not set url or inject: the preset fixes where
// the credential is sent, and a server that could redirect it would let a
// policy send a host secret anywhere. This holds in every policy.
func ExpandMCP(s v1alpha1.MCPServer) (v1alpha1.MCPServer, error) {
	out := *s.DeepCopy()
	if s.Preset != "" {
		if s.URL != "" {
			return v1alpha1.MCPServer{}, fmt.Errorf("mcp %s: the %s preset sets url; remove url from the server", s.Name, s.Preset)
		}
		if s.Inject != nil {
			return v1alpha1.MCPServer{}, fmt.Errorf("mcp %s: the %s preset sets inject; remove inject from the server", s.Name, s.Preset)
		}
		base, err := decodeMCPPreset(s.Preset)
		if err != nil {
			return v1alpha1.MCPServer{}, fmt.Errorf("mcp %s: %w", s.Name, err)
		}
		if out.Transport == "" {
			out.Transport = base.Transport
		}
		out.URL = base.URL
		out.Inject = base.Inject
	}
	if err := out.Validate(); err != nil {
		return v1alpha1.MCPServer{}, err
	}
	return out, nil
}

// MCPEntry is one expanded server and the policy that declared it.
type MCPEntry struct {
	Policy string
	Server v1alpha1.MCPServer
}

// MCPServers expands the servers of every policy in ps, in order, and refuses
// two servers that share a name and the reserved name brig-memory.
//
// A server name is the last segment of the URL the guest calls, so two
// policies bound to one run cannot share one.
func MCPServers(ps []Policy) ([]MCPEntry, error) {
	owner := map[string]string{}
	var out []MCPEntry
	for _, p := range ps {
		for _, s := range p.Spec.MCP {
			// Checked here as well as in Validate, so the refusal names the
			// reserved name even for a policy built in code.
			if s.Name == v1alpha1.MCPReservedName {
				return nil, fmt.Errorf("policy %s: mcp server name %q is reserved for brig's own memory store",
					p.Metadata.Name, s.Name)
			}
			e, err := ExpandMCP(s)
			if err != nil {
				return nil, fmt.Errorf("policy %s: %w", p.Metadata.Name, err)
			}
			if prev, ok := owner[e.Name]; ok {
				return nil, fmt.Errorf("mcp server %s is declared by both %s and %s; a server name is "+
					"unique across the policies bound to a run", e.Name, prev, p.Metadata.Name)
			}
			owner[e.Name] = p.Metadata.Name
			out = append(out, MCPEntry{Policy: p.Metadata.Name, Server: e})
		}
	}
	return out, nil
}
