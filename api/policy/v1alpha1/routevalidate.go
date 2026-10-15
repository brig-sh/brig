package v1alpha1

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Validate checks a complete route, as it is after preset expansion: every
// required field is set, the mode is one this version serves, and
// Authorization is in inject.headers.remove. A credential lifecycle passes
// only when SetLifecycleFromPreset set it.
func (r *Route) Validate() error {
	return r.wrap(r.validate(true))
}

// validateWritten checks a route as a policy document writes it.
func (r *Route) validateWritten() error {
	return r.wrap(r.validate(false))
}

// wrap names the route in an error, so a policy with several routes says
// which one is wrong.
func (r *Route) wrap(err error) error {
	if err == nil {
		return nil
	}
	if r.Name == "" {
		return fmt.Errorf("route: %w", err)
	}
	return fmt.Errorf("route %s: %w", r.Name, err)
}

// validate checks r. expanded is true for a route after preset expansion and
// defaulting. It is false for a route as written, where a preset may still
// fill the required fields.
func (r *Route) validate(expanded bool) error {
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := CheckRouteName(r.Name); err != nil {
		return err
	}
	switch r.Preset {
	case "", PresetClaudeCode, PresetAnthropicAPIKey, PresetGitHubToken:
	default:
		return fmt.Errorf("preset %q is not one of %s, %s or %s",
			r.Preset, PresetClaudeCode, PresetAnthropicAPIKey, PresetGitHubToken)
	}
	switch r.Mode {
	case "", ModeEndpoint:
	default:
		return fmt.Errorf("mode %q is not supported; this version serves %q only", r.Mode, ModeEndpoint)
	}
	// A route as written without a preset has nothing to fill it, so it is
	// held to the required fields too.
	complete := expanded || r.Preset == ""

	if r.Upstream != "" {
		if err := checkOrigin(r.Upstream); err != nil {
			return err
		}
	} else if complete {
		return fmt.Errorf("upstream is required")
	}

	if r.Credential == nil {
		return fmt.Errorf("credential.secretRef.name is required")
	}
	if r.Credential.Lifecycle != "" && !r.lifecycleFromPreset {
		return fmt.Errorf("credential.lifecycle is set by a preset, and a policy does not name it")
	}
	if err := r.Credential.validate(); err != nil {
		return err
	}

	if r.Inject != nil {
		if err := r.Inject.validate(complete, expanded); err != nil {
			return err
		}
	} else if complete {
		return fmt.Errorf("inject is required: headers.set or basicAuth")
	}

	if r.Guest != nil {
		if err := r.Guest.validate(complete); err != nil {
			return err
		}
	} else if complete {
		return fmt.Errorf("guest is required: env, git or both")
	}

	if r.Request != nil && r.Request.Allow != nil {
		if err := r.Request.Allow.validate(r.Preset); err != nil {
			return err
		}
	}
	return nil
}

// checkOrigin refuses anything but a bare https:// origin in canonical form.
// The route path is the guest's, so an upstream path would be a second path
// the broker has to join to it. The canonical form makes the string the
// broker dials the string a reviewer reads.
func checkOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("upstream %q is not a URL: %w", origin, err)
	}
	host := u.Host
	switch {
	case !strings.HasPrefix(origin, "https://"):
		return fmt.Errorf("upstream %q is not an https:// origin", origin)
	case u.Opaque != "" || u.Hostname() == "":
		return fmt.Errorf("upstream %q has no host", origin)
	case u.User != nil:
		return fmt.Errorf("upstream %q holds a user name or password; the credential goes in credential:", origin)
	case u.Path != "" || u.RawPath != "":
		return fmt.Errorf("upstream %q has a path; write the bare origin, https://host[:port]", origin)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("upstream %q has a query; write the bare origin, https://host[:port]", origin)
	case u.Fragment != "" || strings.Contains(origin, "#"):
		return fmt.Errorf("upstream %q has a fragment; write the bare origin, https://host[:port]", origin)
	case host != strings.ToLower(host):
		return fmt.Errorf("upstream %q has upper case in its host; write it in lower case", origin)
	case strings.Contains(host, "*"):
		return fmt.Errorf("upstream %q has a wildcard; an upstream is one host", origin)
	case strings.Contains(host, "%"):
		return fmt.Errorf("upstream %q has an IPv6 zone", origin)
	case strings.HasSuffix(u.Hostname(), "."):
		return fmt.Errorf("upstream %q ends its host with a dot", origin)
	case strings.HasSuffix(host, ":"):
		return fmt.Errorf("upstream %q has an empty port", origin)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || port[0] == '0' {
			return fmt.Errorf("upstream %q has port %q; a port is 1-65535", origin, port)
		}
	}
	if strings.HasPrefix(host, "[") {
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil || !addr.Is6() {
			return fmt.Errorf("upstream %q has an address that is not IPv6 in brackets", origin)
		}
	} else if !isDNSName(u.Hostname()) {
		return fmt.Errorf("upstream %q has a host that is not a DNS name or an IP address", origin)
	}
	if canonical := (&url.URL{Scheme: "https", Host: host}).String(); origin != canonical {
		return fmt.Errorf("upstream %q is not in canonical form; write %q", origin, canonical)
	}
	return nil
}

func (c *CredentialRef) validate() error {
	if c.SecretRef.Name == "" {
		return fmt.Errorf("credential.secretRef.name is required")
	}
	if err := checkSecretName(c.SecretRef.Name); err != nil {
		return err
	}
	if strings.TrimSpace(c.Field) != c.Field {
		return fmt.Errorf("credential.field %q has leading or trailing space", c.Field)
	}
	switch c.Lifecycle {
	case "", LifecycleAnthropicOAuth:
	default:
		return fmt.Errorf("credential.lifecycle %q is not %q", c.Lifecycle, LifecycleAnthropicOAuth)
	}
	return nil
}

// maxSecretName is the longest secret name brig stores.
const maxSecretName = 128

// checkSecretName applies the grammar of a secret name. It is a copy of
// internal/secret.ValidName, because a package under api/ does not import
// internal/.
func checkSecretName(name string) error {
	if len(name) > maxSecretName {
		return fmt.Errorf("credential.secretRef.name %q is longer than %d characters", name, maxSecretName)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9' || c == '_' || c == '-') && i > 0:
		default:
			return fmt.Errorf("credential.secretRef.name %q may use only letters, digits, - and _, "+
				"and must start with a letter", name)
		}
	}
	return nil
}

// validate checks where the credential goes. complete requires that
// something carries it. expanded requires Authorization in headers.remove.
func (in *Inject) validate(complete, expanded bool) error {
	h := in.Headers
	setsHeaders := h != nil && len(h.Set) > 0
	switch {
	case setsHeaders && in.BasicAuth != nil:
		return fmt.Errorf("inject takes headers.set or basicAuth, not both")
	case !setsHeaders && in.BasicAuth == nil && complete:
		return fmt.Errorf("inject needs headers.set or basicAuth")
	}
	if in.BasicAuth != nil {
		u := in.BasicAuth.Username
		if u == "" {
			return fmt.Errorf("inject.basicAuth.username is required")
		}
		if strings.ContainsAny(u, ":\r\n") {
			return fmt.Errorf("inject.basicAuth.username %q holds a colon or a line break", u)
		}
	}
	if h != nil {
		if err := h.validate(complete && setsHeaders); err != nil {
			return err
		}
	}
	if expanded && (h == nil || !containsFold(h.Remove, "Authorization")) {
		return fmt.Errorf("inject.headers.remove must hold Authorization")
	}
	return nil
}

// validate checks the header edits. needsValue requires that a set header
// carries the credential, so a route never forwards without it.
func (h *HeaderInject) validate(needsValue bool) error {
	removed := map[string]bool{}
	for _, name := range h.Remove {
		if err := checkHeaderName("inject.headers.remove", name); err != nil {
			return err
		}
		removed[strings.ToLower(name)] = true
	}
	set := map[string]bool{}
	carries := false
	for _, hd := range h.Set {
		if err := checkHeaderName("inject.headers.set", hd.Name); err != nil {
			return err
		}
		// Remove applies first, so a header in both lists is either a
		// mistake or a set written twice. Authorization is the exception:
		// it is always removed, and a route may set its own.
		if removed[strings.ToLower(hd.Name)] && !strings.EqualFold(hd.Name, "Authorization") {
			return fmt.Errorf("inject.headers.set names %s, which inject.headers.remove drops first; "+
				"name it in one list", hd.Name)
		}
		for _, p := range []string{PlaceholderRouteURL, PlaceholderSessionToken} {
			if strings.Contains(hd.Value, p) {
				return fmt.Errorf("inject.headers.set %s holds %s, which only guest.env takes", hd.Name, p)
			}
		}
		key := strings.ToLower(hd.Name)
		if set[key] {
			return fmt.Errorf("inject.headers.set names %s twice", hd.Name)
		}
		set[key] = true
		if strings.ContainsAny(hd.Value, "\r\n") {
			return fmt.Errorf("inject.headers.set %s has a line break in its value", hd.Name)
		}
		switch strings.Count(hd.Value, PlaceholderValue) {
		case 0:
		case 1:
			carries = true
		default:
			return fmt.Errorf("inject.headers.set %s holds %s more than once", hd.Name, PlaceholderValue)
		}
	}
	if needsValue && !carries {
		return fmt.Errorf("no inject.headers.set value holds %s, so the credential would never reach upstream",
			PlaceholderValue)
	}
	allowed := map[string]bool{}
	for _, a := range h.Allow {
		if err := checkHeaderName("inject.headers.allow", a.Name); err != nil {
			return err
		}
		key := strings.ToLower(a.Name)
		switch {
		case key == "authorization":
			return fmt.Errorf("inject.headers.allow cannot name Authorization, which is always removed")
		case set[key] || removed[key]:
			return fmt.Errorf("inject.headers.allow names %s, which set or remove already names", a.Name)
		case allowed[key]:
			return fmt.Errorf("inject.headers.allow names %s twice", a.Name)
		case len(a.Values) == 0:
			return fmt.Errorf("inject.headers.allow %s lists no values; remove it instead", a.Name)
		}
		allowed[key] = true
		for _, v := range a.Values {
			if v == "" || strings.ContainsAny(v, ", \t\r\n") {
				return fmt.Errorf("inject.headers.allow %s value %q is empty or holds a comma or a space", a.Name, v)
			}
		}
	}
	return nil
}

func (g *Guest) validate(complete bool) error {
	if complete && len(g.Env) == 0 && g.Git == nil {
		return fmt.Errorf("guest needs env, git or both")
	}
	for name, value := range g.Env {
		if !isEnvName(name) {
			return fmt.Errorf("guest.env name %q is not an environment variable name", name)
		}
		switch value {
		case PlaceholderRouteURL, PlaceholderSessionToken:
		default:
			// The value is left out: a literal here is most often a pasted
			// credential, and the error goes to a terminal or a CI log.
			return fmt.Errorf("guest.env %s is a literal; a value is %s or %s, never a literal",
				name, PlaceholderRouteURL, PlaceholderSessionToken)
		}
	}
	if g.Git != nil {
		if len(g.Git.InsteadOf) == 0 {
			return fmt.Errorf("guest.git.insteadOf is required when guest.git is set")
		}
		for _, prefix := range g.Git.InsteadOf {
			if prefix == "" || strings.IndexFunc(prefix, unicode.IsSpace) >= 0 {
				return fmt.Errorf("guest.git.insteadOf %q is empty or holds a space", prefix)
			}
		}
	}
	return nil
}

func (a *RequestAllow) validate(preset Preset) error {
	// A list that is set must match, and an empty one would count as unset
	// and allow everything, while a reader takes it to allow nothing.
	for _, l := range []struct {
		field string
		list  []string
	}{{"methods", a.Methods}, {"paths", a.Paths}, {"repos", a.Repos}} {
		if l.list != nil && len(l.list) == 0 {
			return fmt.Errorf("request.allow.%s is empty; list at least one, or leave the key out", l.field)
		}
	}
	if len(a.Methods) == 0 && len(a.Paths) == 0 && len(a.Repos) == 0 {
		return fmt.Errorf("request.allow lists nothing; leave it out to allow every request")
	}
	for _, m := range a.Methods {
		if m == "" || strings.ToUpper(m) != m || !isToken(m) {
			return fmt.Errorf("request.allow.methods %q is not an HTTP method in upper case", m)
		}
	}
	for _, p := range a.Paths {
		if !strings.HasPrefix(p, "/") || strings.IndexFunc(p, unicode.IsSpace) >= 0 {
			return fmt.Errorf("request.allow.paths %q does not start with / or holds a space", p)
		}
	}
	if len(a.Repos) > 0 && preset != PresetGitHubToken {
		return fmt.Errorf("request.allow.repos needs preset: %s", PresetGitHubToken)
	}
	for _, repo := range a.Repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
			strings.IndexFunc(repo, unicode.IsSpace) >= 0 {
			return fmt.Errorf("request.allow.repos %q is not owner/name", repo)
		}
	}
	return nil
}

// checkHeaderName refuses a name that is not an HTTP header name, and a header
// that the broker owns: the ones that frame the request, route it, or speak
// to a proxy, and the other hop-by-hop headers, which never reach upstream.
func checkHeaderName(field, name string) error {
	if name == "" || !isToken(name) {
		return fmt.Errorf("%s name %q is not an HTTP header name", field, name)
	}
	lower := strings.ToLower(name)
	switch {
	case lower == "host", lower == "connection", lower == "content-length",
		lower == "transfer-encoding", lower == "upgrade", lower == "keep-alive",
		lower == "te", lower == "trailer", strings.HasPrefix(lower, "proxy-"):
		return fmt.Errorf("%s names %s, which the broker sets itself", field, name)
	}
	return nil
}

// isToken reports whether s is an HTTP token, the grammar of a header name
// and a method (RFC 9110, section 5.6.2).
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}

// isDNSName reports whether s is a host name of letters, digits and dashes:
// labels of 1-63 characters that neither start nor end with a dash, and 253
// characters at most. An IPv4 address passes as such a name. s has no
// trailing dot; checkOrigin refuses one first.
func isDNSName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func isEnvName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
