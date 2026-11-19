package v1alpha1

import (
	"fmt"
	"net/url"
	"strings"
)

// Validate checks a server after preset expansion: the transport, the URL and
// the tool list are all present.
func (s *MCPServer) Validate() error {
	return s.wrap(s.validate(true))
}

// validateWritten checks a server as a policy document writes it. A preset
// fills transport, url and inject, so they may be absent when it is set.
func (s *MCPServer) validateWritten() error {
	return s.wrap(s.validate(false))
}

// wrap names the server in an error, so a policy with several servers says
// which one is wrong.
func (s *MCPServer) wrap(err error) error {
	if err == nil {
		return nil
	}
	if s.Name == "" {
		return fmt.Errorf("mcp: %w", err)
	}
	return fmt.Errorf("mcp %s: %w", s.Name, err)
}

// validate checks s. expanded is true after preset expansion, and false for a
// server as written, where a preset may still fill the required fields.
func (s *MCPServer) validate(expanded bool) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	// The name is a path segment and a guest-visible name, as a route name
	// is, so it takes the same grammar.
	if err := CheckRouteName(s.Name); err != nil {
		return err
	}
	if s.Name == MCPReservedName {
		return fmt.Errorf("name %q is reserved for brig's own memory store", s.Name)
	}
	switch s.Preset {
	case "", MCPPresetNofire:
	default:
		return fmt.Errorf("preset %q is not %s", s.Preset, MCPPresetNofire)
	}
	// The preset supplies the rest, so a missing field is only an error
	// once nothing can fill it.
	complete := expanded || s.Preset == ""
	switch s.Transport {
	case MCPTransportHTTP:
	case "":
		if complete {
			return fmt.Errorf("transport is required (%s)", MCPTransportHTTP)
		}
	case "stdio":
		return fmt.Errorf("transport %q is not supported yet; use %s", s.Transport, MCPTransportHTTP)
	default:
		return fmt.Errorf("transport %q is not %s", s.Transport, MCPTransportHTTP)
	}
	if s.URL == "" {
		if complete {
			return fmt.Errorf("url is required")
		}
	} else if err := checkMCPURL(s.URL); err != nil {
		return err
	}
	if s.Credential != nil {
		if err := s.Credential.validate(); err != nil {
			return err
		}
		// A lifecycle is a preset's to set, as on a route.
		if s.Credential.Lifecycle != "" {
			return fmt.Errorf("credential.lifecycle is set by a preset, not by a policy")
		}
		// The MCP proxy reads no field of a secret document. Accepted here,
		// the server would fail on every request instead of at load.
		if s.Credential.Field != "" {
			return fmt.Errorf("credential.field is not supported on an mcp server; it takes the whole secret value")
		}
	}
	if err := s.validateInject(complete); err != nil {
		return err
	}
	if s.Tools == nil || len(s.Tools.Allow) == 0 {
		return fmt.Errorf("tools.allow is required; list the tools the guest may call, or %q for all", MCPAllTools)
	}
	for _, tool := range s.Tools.Allow {
		if tool == "" || strings.TrimSpace(tool) != tool {
			return fmt.Errorf("tools.allow entry %q is empty or has leading or trailing space", tool)
		}
	}
	return nil
}

// validateInject checks where the credential goes. Only headers are sent: a
// TLS block or a basicAuth has no meaning for an MCP server yet, and an
// accepted block that nothing reads would be a silent no-op.
func (s *MCPServer) validateInject(complete bool) error {
	in := s.Inject
	if in == nil {
		if s.Credential != nil && complete {
			return fmt.Errorf("credential needs inject.headers.set to carry it upstream")
		}
		return nil
	}
	if in.TLS != nil || in.BasicAuth != nil {
		return fmt.Errorf("inject takes only headers on an mcp server")
	}
	if in.Headers == nil {
		return fmt.Errorf("inject needs headers")
	}
	// With no credential, {value} has nothing to stand for: the server would
	// load, and the proxy would refuse every request it sends.
	if complete && s.Credential == nil {
		for _, hd := range in.Headers.Set {
			if strings.Contains(hd.Value, PlaceholderValue) {
				return fmt.Errorf("inject.headers.set %s holds %s, but the server names no credential", hd.Name, PlaceholderValue)
			}
		}
	}
	return in.Headers.validate(complete && s.Credential != nil)
}

// checkMCPURL refuses an MCP endpoint that is not an https:// URL, or that
// carries a secret in the URL. Unlike a route upstream, an MCP endpoint has a
// path.
func checkMCPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q is not a URL: %w", raw, err)
	}
	switch {
	case !strings.HasPrefix(raw, "https://"):
		return fmt.Errorf("url %q is not an https:// URL", raw)
	case u.Opaque != "" || u.Hostname() == "":
		return fmt.Errorf("url %q has no host", raw)
	case u.User != nil:
		return fmt.Errorf("url %q holds a user name or password (userinfo); the credential goes in credential:", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("url %q has a query; the credential goes in credential:, not in the URL", raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("url %q has a fragment", raw)
	case u.Hostname() != strings.ToLower(u.Hostname()) || strings.ContainsAny(u.Host, "*%"):
		return fmt.Errorf("url %q has an upper case, wildcard or zoned host; write one lower case host", raw)
	}
	return nil
}
