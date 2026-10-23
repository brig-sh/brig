package broker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// errCredentialShape is a resolved secret the route cannot inject: the field
// is missing, not a string, or found twice with two values, or the value would
// break the header it goes in.
var errCredentialShape = errors.New("the credential is not a header value")

// credentialValue returns what the route injects from doc: the field that
// credential.field names, or the whole document, without one trailing "\n"
// or "\r\n".
func credentialValue(c *v1alpha1.CredentialRef, doc []byte) (string, error) {
	v := string(doc)
	if c.Field != "" {
		var root any
		if err := json.Unmarshal(doc, &root); err != nil {
			return "", errCredentialShape
		}
		var err error
		if v, err = field(root, c.Field); err != nil {
			return "", err
		}
	}
	// A secret saved from a file usually ends in one newline.
	if s, ok := strings.CutSuffix(v, "\n"); ok {
		v = strings.TrimSuffix(s, "\r")
	}
	if v == "" {
		return "", errCredentialShape
	}
	for i := 0; i < len(v); i++ {
		if v[i] < ' ' || v[i] == 0x7f {
			return "", errCredentialShape
		}
	}
	return v, nil
}

// field returns the string that name selects in root.
//
// A dotted name, such as claudeAiOauth.accessToken, is first a path from the
// root. Next comes a key at the root that holds the whole name. Only then is
// the name searched at any depth, as profile secret fields are: a key that
// holds the whole name, or the path below a key that holds its first segment.
// A search at depth that finds two different values is refused. A credential
// document can carry tokens of other services under the same key, such as the
// MCP entries of .credentials.json, and map order would pick one at random.
func field(root any, name string) (string, error) {
	if v, ok := walkPath(root, strings.Split(name, ".")); ok {
		return asString(v)
	}
	if m, ok := root.(map[string]any); ok {
		if v, ok := m[name]; ok {
			return asString(v)
		}
	}
	var found []any
	collect(root, name, &found)
	if len(found) == 0 {
		return "", errCredentialShape
	}
	first, err := asString(found[0])
	if err != nil {
		return "", err
	}
	for _, v := range found[1:] {
		if s, err := asString(v); err != nil || s != first {
			return "", errCredentialShape
		}
	}
	return first, nil
}

func asString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errCredentialShape
	}
	return s, nil
}

// walkPath follows the keys of path down from node.
func walkPath(node any, path []string) (any, bool) {
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[key]; !ok {
			return nil, false
		}
	}
	return node, true
}

// collect appends every value below node that name selects: the value of a
// key equal to name, and for a dotted name the rest of the path below a key
// equal to its first segment.
func collect(node any, name string, found *[]any) {
	head, rest, dotted := strings.Cut(name, ".")
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == name {
				*found = append(*found, v)
			} else if dotted && k == head {
				if got, ok := walkPath(v, strings.Split(rest, ".")); ok {
					*found = append(*found, got)
				}
			}
			collect(v, name, found)
		}
	case []any:
		for _, v := range n {
			collect(v, name, found)
		}
	}
}

// sessionToken returns the session token a guest request carries, or "".
//
// The tool in the guest sends the token where it would send the credential.
// So the broker reads Authorization: Bearer, the basic-auth password on a
// basicAuth route (git's credential helper answers with the token), and each
// header the route sets with {value}, such as x-api-key.
func sessionToken(in *v1alpha1.Inject, h http.Header) string {
	auth := h.Get("Authorization")
	if scheme, tok, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	if in.BasicAuth != nil {
		r := http.Request{Header: http.Header{"Authorization": {auth}}}
		if _, pass, ok := r.BasicAuth(); ok {
			return pass
		}
	}
	if in.Headers != nil {
		for _, s := range in.Headers.Set {
			if strings.EqualFold(s.Name, "Authorization") {
				continue
			}
			prefix, suffix, ok := strings.Cut(s.Value, v1alpha1.PlaceholderValue)
			got := h.Get(s.Name)
			if !ok || got == "" {
				continue
			}
			if tok, ok := strings.CutPrefix(got, prefix); ok {
				if tok, ok = strings.CutSuffix(tok, suffix); ok && tok != "" {
					return tok
				}
			}
		}
	}
	return ""
}

// droppedNames are guest headers that never reach the upstream. Each one
// tells a proxy or a framework where the request came from, or which method
// or path to serve instead of the real one, so it could get around
// request.allow. Every X-Forwarded- header is dropped too.
var droppedNames = []string{
	"forwarded", "via", "x-real-ip",
	"x-http-method-override", "x-http-method", "x-method-override",
	"x-original-url", "x-rewrite-url", "x-original-method",
	"accept-encoding",
}

// fold returns the form of a header name the rules compare: lower case, with
// an underscore read as a dash, as some servers read it.
func fold(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

// dropGuestHeaders removes what the guest sent and upstream must not see. It
// returns the names of the headers that inject.headers.allow filtered, with
// the number of values dropped from each.
//
// It drops every header name that holds an underscore, which Go does not put
// in canonical form, so a spelling such as x_api_key cannot slip past a rule.
// It then drops the droppedNames, every X-Forwarded-, X-Brig- and Proxy-
// header, the headers named by inject.headers.remove and inject.headers.set,
// Authorization always among them, and any header that carries the session
// token. Accept-Encoding goes too, so the transport asks for a compression it
// undoes itself, and the response body the broker redacts is plain. Where the
// transport asks for none, forward asks for identity.
//
// ReverseProxy has already removed the hop-by-hop headers and the ones
// Connection named.
func dropGuestHeaders(in *v1alpha1.Inject, h http.Header, token string) map[string]int {
	rules := []string{"authorization"}
	var allowed []v1alpha1.HeaderAllow
	if in.Headers != nil {
		for _, name := range in.Headers.Remove {
			rules = append(rules, fold(name))
		}
		for _, s := range in.Headers.Set {
			rules = append(rules, fold(s.Name))
		}
		allowed = in.Headers.Allow
	}
	for name, values := range h {
		f := fold(name)
		canon := http.CanonicalHeaderKey(name)
		switch {
		case strings.Contains(name, "_"),
			strings.HasPrefix(f, "x-forwarded-"), strings.HasPrefix(f, "x-brig-"), strings.HasPrefix(f, "proxy-"),
			slices.Contains(droppedNames, f), slices.Contains(rules, f),
			slices.ContainsFunc(values, func(v string) bool { return token != "" && strings.Contains(v, token) }):
			delete(h, name)
		case canon != name:
			// The allow filter reads the canonical name.
			delete(h, name)
			h[canon] = append(h[canon], values...)
		}
	}
	var dropped map[string]int
	for _, a := range allowed {
		if n := filterValues(h, a); n > 0 {
			if dropped == nil {
				dropped = map[string]int{}
			}
			dropped[http.CanonicalHeaderKey(a.Name)] = n
		}
	}
	return dropped
}

// filterValues keeps the values of header a.Name that a.Values lists, item by
// item across comma-separated lists, and returns how many it dropped. The
// header is forwarded once, with the values that survive.
func filterValues(h http.Header, a v1alpha1.HeaderAllow) int {
	values := h.Values(a.Name)
	if len(values) == 0 {
		return 0
	}
	var keep []string
	dropped := 0
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			item = strings.TrimSpace(item)
			switch {
			case item == "":
			case slices.Contains(a.Values, item):
				if !slices.Contains(keep, item) {
					keep = append(keep, item)
				}
			default:
				dropped++
			}
		}
	}
	h.Del(a.Name)
	if len(keep) > 0 {
		h.Set(a.Name, strings.Join(keep, ","))
	}
	return dropped
}

// injectCredential sets the route's headers with value in place of {value},
// or Authorization: Basic for a basicAuth route. A set header replaces any
// guest header of the same name.
func injectCredential(in *v1alpha1.Inject, h http.Header, value string) {
	if in.Headers != nil {
		for _, s := range in.Headers.Set {
			h.Set(s.Name, strings.Replace(s.Value, v1alpha1.PlaceholderValue, value, 1))
		}
	}
	if in.BasicAuth != nil {
		raw := in.BasicAuth.Username + ":" + value
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	}
}
