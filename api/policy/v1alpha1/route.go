package v1alpha1

import "slices"

// Preset names a route type that brig ships. A preset fills upstream,
// inject, guest and the credential lifecycle of a route.
type Preset string

// The presets brig ships.
const (
	// PresetClaudeCode is Claude Code on the subscription its OAuth pair
	// belongs to.
	PresetClaudeCode Preset = "claude-code"
	// PresetAnthropicAPIKey is the Anthropic API on a console API key.
	PresetAnthropicAPIKey Preset = "anthropic-api-key"
	// PresetGitHubToken is git over HTTPS to github.com with a token.
	PresetGitHubToken Preset = "github-token"
	// PresetGitHubAPI is gh against api.github.com with a token, reached as a
	// host route.
	PresetGitHubAPI Preset = "github-api"
	// PresetAWS is the AWS container credential provider. The route answers
	// it with temporary keys, because AWS signs each request with the secret
	// key and a stand-in cannot be injected on the way out.
	PresetAWS Preset = "aws"
	// PresetGCP is the Google Cloud APIs that gcloud and the client
	// libraries call, one host for each service the route lists.
	PresetGCP Preset = "gcp"
)

// RouteMode is how a route reaches the tool in the guest.
type RouteMode string

// ModeEndpoint points the tool at the route URL. It is the default, and the
// one mode every build of brig serves. A build can register others (see
// internal/routemode).
const ModeEndpoint RouteMode = "endpoint"

// ModeHost is an endpoint route that the guest reaches at its own name,
// <route>.brig.internal. A tool that takes a host name and no URL, such as
// gh, needs it.
const ModeHost RouteMode = "host"

// AtBrigInternal reports whether a route in mode m is served by the broker at
// brig.internal: mode endpoint, which an empty mode defaults to, and mode host.
func (m RouteMode) AtBrigInternal() bool {
	return m == "" || m == ModeEndpoint || m == ModeHost
}

// HostName is the name under brig.internal at which the guest reaches the
// host route of that name.
func HostName(route string) string {
	return route + ".brig.internal"
}

// Lifecycle names how the broker keeps a credential valid.
type Lifecycle string

// LifecycleAnthropicOAuth refreshes an Anthropic OAuth pair ahead of its
// expiry and writes the new pair back to the store.
const LifecycleAnthropicOAuth Lifecycle = "anthropic-oauth"

// The placeholders a route may use. Each stands for a value that the guest
// never holds.
const (
	// PlaceholderValue is the credential, in an injected header value or a
	// basic-auth password.
	PlaceholderValue = "{value}"
	// PlaceholderRouteURL is the URL of the route, in a guest.env value.
	PlaceholderRouteURL = "{route.url}"
	// PlaceholderSessionToken is the session token of the route, in a
	// guest.env value.
	PlaceholderSessionToken = "{session.token}"
	// PlaceholderRouteRegion is the region of a route that vends, in a
	// guest.env value.
	PlaceholderRouteRegion = "{route.region}"
	// PlaceholderRouteHost is the host name of a mode: host route, in a
	// guest.env value, for a tool such as gh that takes a host and no URL.
	PlaceholderRouteHost = "{route.host}"
)

// Route is one brokered endpoint. The guest is pointed at the route, and the
// broker forwards each request to Upstream with the credential injected.
type Route struct {
	// Name is required. It is the path segment of the route URL, follows
	// CheckRouteName, and is unique across the policies bound to a run.
	Name string `json:"name"`
	// Preset is optional. It names a route type that brig ships, and a field
	// set here overrides the value of the preset.
	Preset Preset `json:"preset,omitempty"`
	// Mode is optional, and defaults to "endpoint". Any other mode is a
	// policy name. A build of brig that does not serve it refuses the route
	// before boot.
	Mode RouteMode `json:"mode,omitempty"`
	// Upstream is required unless a preset sets it. It is a bare https://
	// origin in canonical form: lower case, no path, query, fragment, user
	// name, wildcard, zone or trailing dot, a host that is a DNS name or an
	// IP address, and a port in 1-65535 when one is given.
	Upstream string `json:"upstream,omitempty"`
	// Credential is required. It names the secret that the broker injects.
	Credential *CredentialRef `json:"credential,omitempty"`
	// Inject is required unless a preset sets it. It says where the
	// credential goes on the upstream request.
	Inject *Inject `json:"inject,omitempty"`
	// Guest is required unless a preset sets it. It says how the tool in the
	// guest is pointed at the route.
	Guest *Guest `json:"guest,omitempty"`
	// Request is optional. Without it, every request is allowed.
	Request *RequestPolicy `json:"request,omitempty"`
	// Services is required for the gcp preset, and only that preset takes it.
	// Each entry is one Google API with its own request policy.
	Services []Service `json:"services,omitempty"`

	// lifecycleFromPreset is whether Credential.Lifecycle came from a preset,
	// through SetLifecycleFromPreset. A decoded document cannot set it.
	lifecycleFromPreset bool
}

// Vends reports whether the route answers the guest itself with credentials
// it fetched, instead of forwarding the request upstream.
func (r *Route) Vends() bool { return r.Preset == PresetAWS }

// SetLifecycleFromPreset sets the credential lifecycle of a route that a
// preset expansion produced. Validate accepts a lifecycle only when it was set
// this way, so a policy document cannot name one.
func (r *Route) SetLifecycleFromPreset(l Lifecycle) {
	if r.Credential == nil {
		r.Credential = &CredentialRef{}
	}
	r.Credential.Lifecycle = l
	r.lifecycleFromPreset = l != ""
}

// DeepCopy returns a copy of r that shares no map, slice or pointer with it.
func (r *Route) DeepCopy() *Route {
	if r == nil {
		return nil
	}
	out := *r
	if r.Credential != nil {
		c := *r.Credential
		out.Credential = &c
	}
	if r.Inject != nil {
		in := *r.Inject
		if r.Inject.Headers != nil {
			h := HeaderInject{
				Set:    slices.Clone(r.Inject.Headers.Set),
				Remove: slices.Clone(r.Inject.Headers.Remove),
			}
			if r.Inject.Headers.Allow != nil {
				h.Allow = make([]HeaderAllow, 0, len(r.Inject.Headers.Allow))
				for _, a := range r.Inject.Headers.Allow {
					h.Allow = append(h.Allow, HeaderAllow{Name: a.Name, Values: slices.Clone(a.Values)})
				}
			}
			in.Headers = &h
		}
		if r.Inject.BasicAuth != nil {
			b := *r.Inject.BasicAuth
			in.BasicAuth = &b
		}
		if r.Inject.TLS != nil {
			tls := TLSInject{}
			if r.Inject.TLS.ClientCertificate != nil {
				c := *r.Inject.TLS.ClientCertificate
				tls.ClientCertificate = &c
			}
			if r.Inject.TLS.CA != nil {
				c := *r.Inject.TLS.CA
				tls.CA = &c
			}
			in.TLS = &tls
		}
		out.Inject = &in
	}
	if r.Guest != nil {
		g := Guest{}
		if r.Guest.Env != nil {
			g.Env = make(map[string]string, len(r.Guest.Env))
			for k, v := range r.Guest.Env {
				g.Env[k] = v
			}
		}
		if r.Guest.Git != nil {
			g.Git = &GitGuest{InsteadOf: slices.Clone(r.Guest.Git.InsteadOf)}
		}
		out.Guest = &g
	}
	if r.Request != nil {
		out.Request = r.Request.deepCopy()
	}
	if r.Services != nil {
		out.Services = make([]Service, len(r.Services))
		for i, svc := range r.Services {
			out.Services[i] = Service{Name: svc.Name, Request: svc.Request.deepCopy()}
		}
	}
	return &out
}

// CredentialRef names the secret a route injects.
type CredentialRef struct {
	// SecretRef is required. The secret is never delivered to the guest.
	SecretRef SecretRef `json:"secretRef"`
	// Field is optional. For a document-shaped secret, it names the field
	// that is injected, found at any depth. Without it, the whole value is
	// injected.
	Field string `json:"field,omitempty"`
	// Lifecycle is set only by a preset, through
	// Route.SetLifecycleFromPreset. A policy document that names it is
	// refused.
	Lifecycle Lifecycle `json:"lifecycle,omitempty"`
}

// SecretRef names a secret in brig's store.
type SecretRef struct {
	// Name is required. It holds letters, digits, dash and underscore, and
	// starts with a letter.
	Name string `json:"name"`
}

// Inject says where the credential goes. Exactly one of Headers.Set and
// BasicAuth carries it.
type Inject struct {
	// Headers is optional. It sets, removes and filters headers on the
	// upstream request.
	Headers *HeaderInject `json:"headers,omitempty"`
	// BasicAuth is optional. It sends the credential as the password of an
	// Authorization: Basic header.
	BasicAuth *BasicAuth `json:"basicAuth,omitempty"`
	// TLS is optional. It names the client certificate that the broker
	// presents to the upstream, the CA that the upstream's certificate must
	// chain to, or both. It stands on its own or beside one of Headers.Set
	// and BasicAuth.
	TLS *TLSInject `json:"tls,omitempty"`
}

// CertificateOnly reports whether a client certificate is all that a route
// presents: TLS sets ClientCertificate, and neither Headers.Set nor
// BasicAuth carries a credential. Such a route needs no credential, because
// the upstream knows the caller by its certificate alone. It is false on a
// nil Inject.
func (in *Inject) CertificateOnly() bool {
	if in == nil || in.TLS == nil || in.TLS.ClientCertificate == nil {
		return false
	}
	return (in.Headers == nil || len(in.Headers.Set) == 0) && in.BasicAuth == nil
}

// TLSInject is the TLS that a route speaks to its upstream. At least one of
// ClientCertificate and CA is set.
type TLSInject struct {
	// ClientCertificate is optional. It names the secret that holds the
	// client certificate chain, leaf first, and its unencrypted private key.
	// The broker presents it in the handshake with the upstream. The key
	// stays in the broker process and never reaches the guest.
	ClientCertificate *CertificateRef `json:"clientCertificate,omitempty"`
	// CA is optional. It names the secret that holds the PEM bundle that the
	// upstream's certificate must chain to. It replaces the system roots for
	// the route, so an upstream with a certificate from a public CA is
	// refused on a route that names a private one.
	CA *CertificateRef `json:"ca,omitempty"`
}

// CertificateRef names the secret that holds a certificate or a CA bundle.
type CertificateRef struct {
	// SecretRef is required. The secret is never delivered to the guest.
	SecretRef SecretRef `json:"secretRef"`
}

// HeaderInject edits the headers of the upstream request. Remove applies
// before Set, so a header named in both is refused, except Authorization.
// Neither names Host, Connection, Content-Length, Transfer-Encoding, Upgrade
// or a Proxy- header, which the broker owns.
type HeaderInject struct {
	// Set is optional. Each header replaces a guest header of the same name,
	// and at least one value holds {value} when Set carries the credential.
	// A value never holds {route.url} or {session.token}.
	Set []Header `json:"set,omitempty"`
	// Remove is optional. It lists guest headers dropped before forwarding,
	// and before Set applies. Authorization is always in the list.
	Remove []string `json:"remove,omitempty"`
	// Allow is optional. Each entry keeps only the listed values of one guest
	// header and drops the rest. A header named here is not also in Set or
	// Remove.
	Allow []HeaderAllow `json:"allow,omitempty"`
}

// Header is one header that a route sets.
type Header struct {
	// Name is required, and is an HTTP header name.
	Name string `json:"name"`
	// Value is required. It holds {value} at most once.
	Value string `json:"value"`
}

// HeaderAllow is the allowlist of values for one guest header.
type HeaderAllow struct {
	// Name is required, and is an HTTP header name other than Authorization.
	Name string `json:"name"`
	// Values is required. It lists the values that pass. A header value that
	// holds a comma-separated list is filtered item by item.
	Values []string `json:"values"`
}

// BasicAuth sends the credential as Authorization: Basic
// base64(username:{value}).
type BasicAuth struct {
	// Username is required, and holds no colon.
	Username string `json:"username"`
}

// Guest says how the tool in the guest is pointed at a route. It sets Env,
// Git or both.
type Guest struct {
	// Env is optional. Each name is an environment variable, and each value
	// is {route.url} or {session.token}. A literal value is refused. A route
	// value replaces a profile value of the same name.
	Env map[string]string `json:"env,omitempty"`
	// Git is optional. It points git at the route.
	Git *GitGuest `json:"git,omitempty"`
}

// GitGuest points git in the guest at a route.
type GitGuest struct {
	// InsteadOf is required when Git is set. It lists the URL prefixes that
	// git rewrites to the route.
	InsteadOf []string `json:"insteadOf,omitempty"`
}

// RequestPolicy limits the requests a route forwards.
type RequestPolicy struct {
	// Allow is optional. A request it does not cover is refused with 403
	// before any credential is fetched.
	Allow *RequestAllow `json:"allow,omitempty"`
}

// deepCopy returns a copy of p that shares no slice with it.
func (p *RequestPolicy) deepCopy() *RequestPolicy {
	if p == nil {
		return nil
	}
	req := RequestPolicy{}
	if a := p.Allow; a != nil {
		req.Allow = &RequestAllow{
			Methods: slices.Clone(a.Methods),
			Paths:   slices.Clone(a.Paths),
			Repos:   slices.Clone(a.Repos),
		}
		if g := a.GraphQL; g != nil {
			req.Allow.GraphQL = &GraphQLAllow{
				Queries:   slices.Clone(g.Queries),
				Mutations: slices.Clone(g.Mutations),
				Repos:     slices.Clone(g.Repos),
			}
		}
	}
	return &req
}

// RequestAllow is the allow list of a route. Each list that is set must
// match. At least one list is set.
type RequestAllow struct {
	// Methods is optional. It lists HTTP methods in upper case.
	Methods []string `json:"methods,omitempty"`
	// Paths is optional. It lists globs on the path after the route prefix.
	// Each starts with a slash.
	Paths []string `json:"paths,omitempty"`
	// Repos is optional, and only the github-token preset takes it. It lists
	// owner/name globs.
	Repos []string `json:"repos,omitempty"`
	// GraphQL is optional, and only the github-api preset takes it. It
	// limits a request to /api/graphql by the operation it carries.
	GraphQL *GraphQLAllow `json:"graphql,omitempty"`
}

// GraphQLAllow is what a GraphQL request may do. A list left out allows
// nothing, and "*" allows any field.
type GraphQLAllow struct {
	// Queries lists the top-level fields of Query a query may select.
	Queries []string `json:"queries,omitempty"`
	// Mutations lists the mutations a request may call.
	Mutations []string `json:"mutations,omitempty"`
	// Repos is optional. It lists owner/name globs that a root
	// repository(owner:, name:) field must match. It limits where a query
	// starts, not where it goes, and does not limit mutations.
	Repos []string `json:"repos,omitempty"`
}
