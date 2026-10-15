package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Examples 5 and 6 of the spec reuse the documents of examples 1 and 3, so
// they have no file of their own. internal/policy merges 1 and 3 the way
// example 5 attaches them.
func TestSpecExamplesDecodeAndValidate(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "example*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 5 {
		t.Fatalf("found %d examples, want 5: %v", len(paths), paths)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			blob, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := Decode(blob)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// request.allow is cut to a git enforcer, so the spec's own example 1, which
// still lists models, is refused as an unknown field.
func TestDecodeRefusesTheCutRequestFields(t *testing.T) {
	for _, field := range []string{"models", "tools"} {
		doc := `
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: claude-only}
spec:
  routes:
    - name: anthropic
      preset: claude-code
      credential: {secretRef: {name: claude-credentials}}
      request:
        allow:
          ` + field + `: [claude-sonnet-5]
`
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("request.allow.%s was accepted", field)
		}
	}
}

func TestDecodeRefusesAnUnknownRouteField(t *testing.T) {
	for _, field := range []string{
		"tls: {insecure: true}",
		"inject: {headers: {append: [{name: a, value: b}]}}",
		"guest: {files: {a: b}}",
		"credential: {secretRef: {name: a, key: b}}",
		"status: {}",
	} {
		doc := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n" +
			"    - name: r\n      " + field + "\n"
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("Decode accepted %q", field)
		}
	}
}

// customRoute is example 4's route as it is after expansion: complete, and
// with Authorization in inject.headers.remove.
func customRoute() Route {
	return Route{
		Name:       "inventory",
		Mode:       ModeEndpoint,
		Upstream:   "https://inventory.internal.example.com",
		Credential: &CredentialRef{SecretRef: SecretRef{Name: "inventory-token"}, Field: "token"},
		Inject: &Inject{Headers: &HeaderInject{
			Set:    []Header{{Name: "X-Api-Key", Value: "{value}"}},
			Remove: []string{"Authorization"},
		}},
		Guest: &Guest{Env: map[string]string{
			"INVENTORY_URL":   "{route.url}",
			"INVENTORY_TOKEN": "{session.token}",
		}},
		Request: &RequestPolicy{Allow: &RequestAllow{Methods: []string{"GET"}, Paths: []string{"/v2/items/*"}}},
	}
}

func TestRouteValidate(t *testing.T) {
	r := customRoute()
	if err := r.Validate(); err != nil {
		t.Fatalf("a complete route was refused: %v", err)
	}
	basic := Route{
		Name:       "git",
		Upstream:   "https://git.example.com:8443",
		Credential: &CredentialRef{SecretRef: SecretRef{Name: "git-token"}},
		Inject: &Inject{
			BasicAuth: &BasicAuth{Username: "x-access-token"},
			Headers:   &HeaderInject{Remove: []string{"authorization"}},
		},
		Guest: &Guest{Git: &GitGuest{InsteadOf: []string{"https://git.example.com/"}}},
	}
	if err := basic.Validate(); err != nil {
		t.Fatalf("a basic-auth route with Authorization in remove was refused: %v", err)
	}

	for _, c := range []struct {
		name string
		edit func(*Route)
		want string
	}{
		{"missing name", func(r *Route) { r.Name = "" }, "name is required"},
		{"name with a slash", func(r *Route) { r.Name = "a/b" }, "lowercase"},
		{"unknown preset", func(r *Route) { r.Preset = "openai" }, "not one of"},
		{"unknown mode", func(r *Route) { r.Mode = "proxy" }, `mode "proxy" is not supported; this version serves "endpoint" only`},
		{"missing upstream", func(r *Route) { r.Upstream = "" }, "upstream is required"},
		{"upstream with a path", func(r *Route) { r.Upstream = "https://api.example.com/v1" }, "has a path"},
		{"upstream with a lone slash", func(r *Route) { r.Upstream = "https://api.example.com/" }, "has a path"},
		{"upstream with a query", func(r *Route) { r.Upstream = "https://api.example.com?a=b" }, "has a query"},
		{"upstream with a fragment", func(r *Route) { r.Upstream = "https://api.example.com#x" }, "has a fragment"},
		{"upstream over http", func(r *Route) { r.Upstream = "http://api.example.com" }, "not an https://"},
		{"upstream with no host", func(r *Route) { r.Upstream = "https://" }, "no host"},
		{"upstream with a password", func(r *Route) { r.Upstream = "https://u:p@api.example.com" }, "password"},
		{"missing credential", func(r *Route) { r.Credential = nil }, "secretRef.name is required"},
		{"missing secret name", func(r *Route) { r.Credential.SecretRef.Name = "" }, "secretRef.name is required"},
		{"secret name with a dot", func(r *Route) { r.Credential.SecretRef.Name = "a.b" }, "letters, digits"},
		{"unknown lifecycle", func(r *Route) { r.Credential.Lifecycle = "aws-sts" }, "lifecycle"},
		{"missing inject", func(r *Route) { r.Inject = nil }, "inject is required"},
		{"two injects", func(r *Route) {
			r.Inject.BasicAuth = &BasicAuth{Username: "x"}
		}, "not both"},
		{"nothing injected", func(r *Route) { r.Inject.Headers.Set = nil }, "needs headers.set or basicAuth"},
		{"{value} twice in one header", func(r *Route) {
			r.Inject.Headers.Set[0].Value = "{value}:{value}"
		}, "more than once"},
		{"no header carries {value}", func(r *Route) {
			r.Inject.Headers.Set[0].Value = "static"
		}, "never reach upstream"},
		{"header set twice", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "x-api-key", Value: "b"})
		}, "twice"},
		{"bad header name", func(r *Route) { r.Inject.Headers.Set[0].Name = "X Api Key" }, "not an HTTP header name"},
		{"Authorization not removed", func(r *Route) { r.Inject.Headers.Remove = []string{"X-Other"} }, "must hold Authorization"},
		{"Authorization allowed through", func(r *Route) {
			r.Inject.Headers.Allow = []HeaderAllow{{Name: "Authorization", Values: []string{"x"}}}
		}, "always removed"},
		{"allow of a removed header", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "Anthropic-Beta")
			r.Inject.Headers.Allow = []HeaderAllow{{Name: "anthropic-beta", Values: []string{"x"}}}
		}, "already names"},
		{"allow with no values", func(r *Route) {
			r.Inject.Headers.Allow = []HeaderAllow{{Name: "Anthropic-Beta"}}
		}, "no values"},
		{"allow value with a comma", func(r *Route) {
			r.Inject.Headers.Allow = []HeaderAllow{{Name: "Anthropic-Beta", Values: []string{"a,b"}}}
		}, "comma"},
		{"missing guest", func(r *Route) { r.Guest = nil }, "guest is required"},
		{"empty guest", func(r *Route) { r.Guest = &Guest{} }, "env, git or both"},
		{"literal in guest.env", func(r *Route) {
			r.Guest.Env["INVENTORY_TOKEN"] = "sk-live-123"
		}, "never a literal"},
		{"placeholder inside a literal", func(r *Route) {
			r.Guest.Env["INVENTORY_URL"] = "{route.url}/v2"
		}, "never a literal"},
		{"bad env name", func(r *Route) {
			r.Guest.Env["1BAD"] = "{route.url}"
		}, "not an environment variable name"},
		{"git with no insteadOf", func(r *Route) { r.Guest.Git = &GitGuest{} }, "insteadOf is required"},
		{"repos on a custom route", func(r *Route) {
			r.Request.Allow.Repos = []string{"brig-sh/*"}
		}, "needs preset: github-token"},
		{"repos on another preset", func(r *Route) {
			r.Preset = PresetClaudeCode
			r.Request.Allow.Repos = []string{"brig-sh/*"}
		}, "needs preset: github-token"},
		{"empty request.allow", func(r *Route) { r.Request.Allow = &RequestAllow{} }, "lists nothing"},
		{"lower-case method", func(r *Route) { r.Request.Allow.Methods = []string{"get"} }, "upper case"},
		{"path without a slash", func(r *Route) { r.Request.Allow.Paths = []string{"v2/items"} }, "start with /"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := customRoute()
			c.edit(&r)
			err := r.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "route") {
				t.Errorf("error %q does not name the route", err)
			}
		})
	}
}

// A literal in guest.env is most often a pasted credential, and the error
// goes to a terminal or a CI log, so it names the variable and never the
// value.
func TestGuestEnvLiteralIsNotEchoed(t *testing.T) {
	const secret = "sk-ant-api03-not-a-real-key"
	r := customRoute()
	r.Guest.Env["ANTHROPIC_API_KEY"] = secret
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "guest.env ANTHROPIC_API_KEY") {
		t.Fatalf("Validate = %v, want the variable named", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("error %q echoes the value", err)
	}
}

func routePolicy(routes ...Route) Policy {
	return Policy{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: Kind},
		Metadata: ObjectMeta{Name: "routes"},
		Spec:     PolicySpec{Routes: routes},
	}
}

// Policy.Validate reads the document as written, before any preset fills it.
func TestPolicyValidateRoutesAsWritten(t *testing.T) {
	presetRoute := Route{
		Name:       "github",
		Preset:     PresetGitHubToken,
		Credential: &CredentialRef{SecretRef: SecretRef{Name: "gh-token"}},
		Request:    &RequestPolicy{Allow: &RequestAllow{Repos: []string{"brig-sh/*"}}},
	}
	// A custom route as written has no Authorization in remove and no mode.
	written := customRoute()
	written.Mode = ""
	written.Inject.Headers.Remove = nil
	p := routePolicy(presetRoute, written)
	if err := p.Validate(); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}

	for _, c := range []struct {
		name   string
		routes []Route
		want   string
	}{
		{"a route declared twice", []Route{presetRoute, presetRoute}, "declared twice"},
		{"a preset route with no credential", []Route{{Name: "a", Preset: PresetClaudeCode}}, "secretRef.name is required"},
		{"lifecycle in a user document", []Route{{
			Name:   "a",
			Preset: PresetClaudeCode,
			Credential: &CredentialRef{
				SecretRef: SecretRef{Name: "claude-credentials"},
				Lifecycle: LifecycleAnthropicOAuth,
			},
		}}, "set by a preset"},
		{"an unknown mode on a preset route", []Route{{
			Name:       "a",
			Preset:     PresetClaudeCode,
			Mode:       "relay",
			Credential: &CredentialRef{SecretRef: SecretRef{Name: "claude-credentials"}},
		}}, "is not supported"},
		{"a literal on a preset route", []Route{{
			Name:       "a",
			Preset:     PresetClaudeCode,
			Credential: &CredentialRef{SecretRef: SecretRef{Name: "claude-credentials"}},
			Guest:      &Guest{Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-123"}},
		}}, "never a literal"},
		{"a custom route with no inject", []Route{{
			Name:       "a",
			Upstream:   "https://api.example.com",
			Credential: &CredentialRef{SecretRef: SecretRef{Name: "tok"}},
			Guest:      &Guest{Env: map[string]string{"A": "{route.url}"}},
		}}, "inject is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := routePolicy(c.routes...)
			err := p.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// The route-level probes of the lookalike-key finding: each would set the
// field it resembles through encoding/json's folding.
func TestDecodeRefusesALookalikeRouteKey(t *testing.T) {
	head := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n    - name: r\n"
	for _, c := range []struct{ name, route, want string }{
		{"upſtream", "      upstream: https://api.example.com\n      upſtream: https://evil.example.com\n",
			`unknown field "spec.routes[0].upſtream"`},
		{"secretref", "      credential: {secretRef: {name: a}, secretref: {name: b}}\n",
			`unknown field "spec.routes[0].credential.secretref"`},
		{"insteadof", "      guest: {git: {insteadOf: [\"https://a/\"], insteadof: [\"https://b/\"]}}\n",
			`unknown field "spec.routes[0].guest.git.insteadof"`},
		{"Name beside name", "      Name: s\n", `unknown field "spec.routes[0].Name"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(head + c.route))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Decode = %v, want %q", err, c.want)
			}
		})
	}
	// guest.env names are data, not fields, so case is the user's own.
	ok := head + "      guest: {env: {Mixed_Case: \"{route.url}\"}}\n"
	if _, err := Decode([]byte(ok)); err != nil {
		t.Errorf("a guest.env name was judged as a field: %v", err)
	}
}

func TestUpstreamIsACanonicalOrigin(t *testing.T) {
	for _, origin := range []string{
		"https://api.example.com",
		"https://api.example.com:8443",
		"https://10.0.0.1",
		"https://[2001:db8::1]:443",
		"https://a-1.b2.example",
		"https://" + strings.Repeat("a", 63) + ".com",
		"https://" + strings.Repeat("a.", 125) + "com",
	} {
		if err := checkOrigin(origin); err != nil {
			t.Errorf("checkOrigin(%q) = %v", origin, err)
		}
	}
	for origin, want := range map[string]string{
		"HTTPS://api.example.com":                      "not an https://",
		"https://API.example.com":                      "upper case",
		"https://api.example.com:":                     "empty port",
		"https://api.example.com:0":                    "a port is 1-65535",
		"https://api.example.com:65536":                "a port is 1-65535",
		"https://api.example.com:0443":                 "a port is 1-65535",
		"https://*.example.com":                        "wildcard",
		"https://[fe80::1%25en0]":                      "zone",
		"https://api.example.com.":                     "with a dot",
		"https://api.example.com/":                     "has a path",
		"https://api.example.com?":                     "has a query",
		"https://u@api.example.com":                    "user name",
		"https://[10.0.0.1]":                           "not a URL",
		"https://api.example.com#":                     "fragment",
		"https://api.example.com:443/v1":               "has a path",
		"https://example.com;x":                        "not a DNS name",
		"https://ex$ample.com":                         "not a DNS name",
		"https://.example.com":                         "not a DNS name",
		"https://a..b":                                 "not a DNS name",
		"https://-x.com":                               "not a DNS name",
		"https://x-.com":                               "not a DNS name",
		"https://api_1.example.com":                    "not a DNS name",
		"https://" + strings.Repeat("a", 64) + ".com":  "not a DNS name",
		"https://" + strings.Repeat("a.", 127) + "com": "not a DNS name",
	} {
		err := checkOrigin(origin)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkOrigin(%q) = %v, want %q", origin, err, want)
		}
	}
}

func TestHeaderRules(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Route)
		want string
	}{
		{"route.url in a header", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "X-Base", Value: "{route.url}"})
		}, "only guest.env takes"},
		{"session.token in a header", func(r *Route) {
			r.Inject.Headers.Set[0].Value = "{value} {session.token}"
		}, "only guest.env takes"},
		{"Host in set", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "Host", Value: "evil.example.com"})
		}, "the broker sets itself"},
		{"Transfer-Encoding in set", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "transfer-encoding", Value: "chunked"})
		}, "the broker sets itself"},
		{"Content-Length in remove", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "Content-Length")
		}, "the broker sets itself"},
		{"Connection in remove", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "Connection")
		}, "the broker sets itself"},
		{"Upgrade in set", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "Upgrade", Value: "h2c"})
		}, "the broker sets itself"},
		{"Proxy-Authorization in set", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "Proxy-Authorization", Value: "{value}"})
		}, "the broker sets itself"},
		{"Keep-Alive holding the credential", func(r *Route) {
			r.Inject.Headers.Set = []Header{{Name: "Keep-Alive", Value: "{value}"}}
		}, "the broker sets itself"},
		{"TE in set", func(r *Route) {
			r.Inject.Headers.Set = append(r.Inject.Headers.Set, Header{Name: "Te", Value: "{value}"})
		}, "the broker sets itself"},
		{"Trailer in remove", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "Trailer")
		}, "the broker sets itself"},
		{"Proxy-Connection in remove", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "Proxy-Connection")
		}, "the broker sets itself"},
		{"a header both removed and set", func(r *Route) {
			r.Inject.Headers.Remove = append(r.Inject.Headers.Remove, "x-api-key")
		}, "drops first"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := customRoute()
			c.edit(&r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want %q", err, c.want)
			}
		})
	}
	// Authorization is always removed, and a route may still set it.
	r := customRoute()
	r.Inject.Headers.Set = []Header{{Name: "Authorization", Value: "Bearer {value}"}}
	if err := r.Validate(); err != nil {
		t.Errorf("setting Authorization was refused: %v", err)
	}
}

// A lifecycle that a document names is refused by both doors: the policy as
// written, and the route on its own.
func TestRouteValidateRefusesADocumentLifecycle(t *testing.T) {
	r := customRoute()
	r.Credential.Lifecycle = LifecycleAnthropicOAuth
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "set by a preset") {
		t.Errorf("Validate = %v, want the lifecycle refused", err)
	}
	r.SetLifecycleFromPreset(LifecycleAnthropicOAuth)
	if err := r.Validate(); err != nil {
		t.Errorf("a lifecycle the preset set was refused: %v", err)
	}
	// DeepCopy keeps the mark that the preset set it.
	copied := r.DeepCopy()
	if err := copied.Validate(); err != nil {
		t.Errorf("DeepCopy dropped the preset's lifecycle: %v", err)
	}
}

// A key written with no value decodes to nil, the same as a key left out. For
// request: and request.allow: that is a route that allows every request,
// which is the opposite of what a reader takes from an allow list whose
// rules are all commented out.
func TestDecodeRefusesAKeyWithNoValue(t *testing.T) {
	head := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n" +
		"    - name: gh\n      preset: github-token\n      credential: {secretRef: {name: gh-token}}\n"
	for name, tail := range map[string]string{
		"allow with every rule commented out": "      request:\n        allow:\n        #  methods: [GET]\n",
		"request with no value":               "      request:\n",
		"allow: null":                         "      request: {allow: null}\n",
		"a list with no value":                "      request:\n        allow:\n          methods: [GET]\n          repos:\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(head + tail))
			if err == nil {
				t.Fatal("Decode accepted a key with no value")
			}
			if !strings.Contains(err.Error(), "has no value") {
				t.Errorf("error %q does not say the key has no value", err)
			}
		})
	}
}

// An empty list in request.allow counted as unset, so repos: [] next to
// methods: [GET] allowed every repository. A reader takes it to mean none.
func TestRequestAllowRefusesAnEmptyList(t *testing.T) {
	for name, edit := range map[string]func(*RequestAllow){
		"methods": func(a *RequestAllow) { a.Methods = []string{} },
		"paths":   func(a *RequestAllow) { a.Paths = []string{} },
		"repos":   func(a *RequestAllow) { a.Repos = []string{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := Route{
				Name:       "gh",
				Preset:     PresetGitHubToken,
				Credential: &CredentialRef{SecretRef: SecretRef{Name: "gh-token"}},
				Request: &RequestPolicy{Allow: &RequestAllow{
					Methods: []string{"GET"}, Paths: []string{"/*"}, Repos: []string{"brig-sh/*"},
				}},
			}
			edit(r.Request.Allow)
			err := r.validateWritten()
			if err == nil {
				t.Fatal("an empty list in request.allow was accepted")
			}
			if !strings.Contains(err.Error(), "request.allow."+name+" is empty") {
				t.Errorf("error %q does not name the empty list", err)
			}
			if copied := r.DeepCopy(); copied.Validate() == nil {
				t.Error("DeepCopy turned the empty list into an unset one")
			}
		})
	}
	doc := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n" +
		"    - name: gh\n      preset: github-token\n      credential: {secretRef: {name: gh-token}}\n" +
		"      request: {allow: {methods: [GET], repos: []}}\n"
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil {
		t.Error("a policy with repos: [] passed Validate")
	}
}

// A route name becomes part of an environment variable name in the guest,
// BRIG_ROUTE_<NAME>_TOKEN, with - read as _. A dot there ends the variable
// name for the shell, and a_b would name the same variable as a-b, so a route
// name holds only lowercase letters, digits and dashes, and starts with a
// letter.
func TestRouteNameMapsToOneVariable(t *testing.T) {
	for _, name := range []string{"gh.x", "a_b", "1gh", "-gh", "Gh"} {
		r := customRoute()
		r.Name = name
		err := r.Validate()
		if err == nil {
			t.Errorf("route name %q was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "lowercase letters, digits and dashes") {
			t.Errorf("route name %q: error %q does not give the rule", name, err)
		}
	}
	for _, name := range []string{"gh", "gh-x", "a1", "claude-code-2"} {
		r := customRoute()
		r.Name = name
		if err := r.Validate(); err != nil {
			t.Errorf("route name %q was refused: %v", name, err)
		}
	}
}
