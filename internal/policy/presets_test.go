package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"sigs.k8s.io/yaml"
)

// reviewedBetas is the reviewed Anthropic-Beta list. A change to it is a
// reviewed change to the claude-code preset, and this test makes it a visible
// one.
var reviewedBetas = []string{
	"oauth-2025-04-20",
	"claude-code-20250219",
	"interleaved-thinking-2025-05-14",
	"fine-grained-tool-streaming-2025-05-14",
	"context-1m-2025-08-07",
	"context-management-2025-06-27",
}

func withSecret(name v1alpha1.Preset) v1alpha1.Route {
	return v1alpha1.Route{
		Name:       "r",
		Preset:     name,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "s"}},
	}
}

func TestShippedPresetsValidate(t *testing.T) {
	names := PresetNames()
	want := []v1alpha1.Preset{
		v1alpha1.PresetAnthropicAPIKey, v1alpha1.PresetClaudeCode, v1alpha1.PresetGitHubToken,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("PresetNames() = %v, want %v", names, want)
	}
	for _, name := range names {
		t.Run(string(name), func(t *testing.T) {
			if _, err := PresetRoute(name); err != nil {
				t.Fatalf("the preset file does not decode: %v", err)
			}
			if _, err := ExpandRoute(withSecret(name)); err != nil {
				t.Fatalf("the preset does not validate once a policy names its secret: %v", err)
			}
		})
	}
}

func TestClaudeCodePreset(t *testing.T) {
	r, err := ExpandRoute(withSecret(v1alpha1.PresetClaudeCode))
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		"ANTHROPIC_BASE_URL":      v1alpha1.PlaceholderRouteURL,
		"CLAUDE_CODE_OAUTH_TOKEN": v1alpha1.PlaceholderSessionToken,
	}
	if !reflect.DeepEqual(r.Guest.Env, wantEnv) {
		t.Errorf("guest.env = %v, want %v", r.Guest.Env, wantEnv)
	}
	if r.Upstream != "https://api.anthropic.com" {
		t.Errorf("upstream = %q", r.Upstream)
	}
	if r.Credential.Lifecycle != v1alpha1.LifecycleAnthropicOAuth || r.Credential.Field != "claudeAiOauth.accessToken" {
		t.Errorf("credential = %+v", r.Credential)
	}
	h := r.Inject.Headers
	if len(h.Set) != 1 || h.Set[0].Name != "Authorization" || h.Set[0].Value != "Bearer {value}" {
		t.Errorf("inject.headers.set = %+v; Claude Code sends the OAuth beta itself, so only the bearer is set", h.Set)
	}
	if !slices.Contains(h.Remove, "X-Api-Key") {
		t.Errorf("inject.headers.remove = %v, want X-Api-Key in it", h.Remove)
	}
	if len(h.Allow) != 1 || h.Allow[0].Name != "Anthropic-Beta" || !reflect.DeepEqual(h.Allow[0].Values, reviewedBetas) {
		t.Errorf("inject.headers.allow = %+v, want the reviewed Anthropic-Beta list", h.Allow)
	}
}

func TestAnthropicAPIKeyPresetLeavesOutTheOAuthBeta(t *testing.T) {
	r, err := ExpandRoute(withSecret(v1alpha1.PresetAnthropicAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(slices.Clone(reviewedBetas), func(v string) bool { return v == "oauth-2025-04-20" })
	if got := r.Inject.Headers.Allow[0].Values; !reflect.DeepEqual(got, want) {
		t.Errorf("Anthropic-Beta allow = %v, want %v", got, want)
	}
	if r.Credential.Lifecycle != "" {
		t.Errorf("a static key has lifecycle %q", r.Credential.Lifecycle)
	}
	if r.Guest.Env["ANTHROPIC_API_KEY"] != v1alpha1.PlaceholderSessionToken {
		t.Errorf("guest.env = %v", r.Guest.Env)
	}
}

func TestExpandRouteOverridesThePreset(t *testing.T) {
	r := withSecret(v1alpha1.PresetClaudeCode)
	r.Name = "claude"
	r.Guest = &v1alpha1.Guest{Env: map[string]string{"ANTHROPIC_BASE_URL": v1alpha1.PlaceholderRouteURL,
		"EXTRA_URL": v1alpha1.PlaceholderRouteURL}}
	r.Inject = &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{Remove: []string{"X-Other", "x-api-key"}}}

	got, err := ExpandRoute(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "claude" || got.Preset != v1alpha1.PresetClaudeCode {
		t.Errorf("name/preset = %q/%q", got.Name, got.Preset)
	}
	if got.Credential.SecretRef.Name != "s" {
		t.Errorf("credential = %+v, want the route's secret", got.Credential)
	}
	// The fields the route leaves out keep the preset's value.
	if got.Credential.Lifecycle != v1alpha1.LifecycleAnthropicOAuth || got.Credential.Field != "claudeAiOauth.accessToken" {
		t.Errorf("credential = %+v, want the preset's lifecycle and field", got.Credential)
	}
	if got.Guest.Env["CLAUDE_CODE_OAUTH_TOKEN"] != v1alpha1.PlaceholderSessionToken ||
		got.Guest.Env["EXTRA_URL"] != v1alpha1.PlaceholderRouteURL {
		t.Errorf("guest.env = %v, want the preset's names and the route's own", got.Guest.Env)
	}
	// inject.headers.remove is the union of both lists, so a route cannot
	// stop the preset from dropping a header.
	if want := []string{"Authorization", "X-Api-Key", "X-Other"}; !reflect.DeepEqual(got.Inject.Headers.Remove, want) {
		t.Errorf("inject.headers.remove = %v, want %v", got.Inject.Headers.Remove, want)
	}
	if got.Mode != v1alpha1.ModeEndpoint {
		t.Errorf("mode = %q, want the endpoint default", got.Mode)
	}

	// A preset with no lifecycle takes another upstream and field.
	key := withSecret(v1alpha1.PresetAnthropicAPIKey)
	key.Upstream = "https://anthropic.proxy.example.com"
	key.Credential.Field = "token"
	got, err = ExpandRoute(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Upstream != key.Upstream || got.Credential.Field != "token" {
		t.Errorf("upstream/field = %q/%q, want the route's own", got.Upstream, got.Credential.Field)
	}
}

// The refresh a lifecycle runs is for the preset's endpoint and field, so a
// route cannot point it elsewhere. Writing the preset's own value is not a
// change, and passes.
func TestExpandRouteKeepsWhatALifecycleNeeds(t *testing.T) {
	upstream := withSecret(v1alpha1.PresetClaudeCode)
	upstream.Upstream = "https://anthropic.proxy.example.com"
	field := withSecret(v1alpha1.PresetClaudeCode)
	field.Credential.Field = "token"
	for name, r := range map[string]v1alpha1.Route{"upstream": upstream, "credential.field": field} {
		if _, err := ExpandRoute(r); err == nil || !strings.Contains(err.Error(), name+" cannot be changed") {
			t.Errorf("ExpandRoute = %v, want %s refused", err, name)
		}
	}
	same := withSecret(v1alpha1.PresetClaudeCode)
	same.Upstream = "https://api.anthropic.com"
	same.Credential.Field = "claudeAiOauth.accessToken"
	if _, err := ExpandRoute(same); err != nil {
		t.Errorf("the preset's own values were refused: %v", err)
	}
}

// A lifecycle is the preset's. A route that carries one without the preset
// having set it is refused by the public Validate, however it got there.
func TestRouteValidateRefusesALifecycleNoPresetSet(t *testing.T) {
	r, err := ExpandRoute(withSecret(v1alpha1.PresetClaudeCode))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("the expanded preset route was refused: %v", err)
	}
	// The same route, as data a document could have produced.
	var written v1alpha1.Route
	if err := v1alpha1.UnmarshalStrict(mustMarshal(t, r), &written); err != nil {
		t.Fatal(err)
	}
	if err := written.Validate(); err == nil || !strings.Contains(err.Error(), "set by a preset") {
		t.Errorf("Validate = %v, want the decoded lifecycle refused", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// ExpandRoute hands back a route of its own: a caller that changes it must
// not change the policy it came from.
func TestExpandRouteSharesNothingWithItsInput(t *testing.T) {
	in := v1alpha1.Route{
		Name:       "github",
		Preset:     v1alpha1.PresetGitHubToken,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gh-token"}},
		Inject:     &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{Remove: []string{"X-Other"}}},
		Guest: &v1alpha1.Guest{
			Env: map[string]string{"GH_URL": v1alpha1.PlaceholderRouteURL},
			Git: &v1alpha1.GitGuest{InsteadOf: []string{"https://github.com/"}},
		},
		Request: &v1alpha1.RequestPolicy{Allow: &v1alpha1.RequestAllow{Repos: []string{"brig-sh/*"}}},
	}
	before := *in.DeepCopy()
	out, err := ExpandRoute(in)
	if err != nil {
		t.Fatal(err)
	}
	out.Credential.SecretRef.Name = "changed"
	out.Inject.Headers.Remove[0] = "changed"
	out.Inject.BasicAuth.Username = "changed"
	out.Guest.Env["GH_URL"] = "changed"
	out.Guest.Git.InsteadOf[0] = "changed"
	out.Request.Allow.Repos[0] = "changed"
	if !reflect.DeepEqual(in, before) {
		t.Errorf("changing the expanded route changed its input:\n got %+v\nwant %+v", in, before)
	}
	again, err := ExpandRoute(before)
	if err != nil {
		t.Fatal(err)
	}
	if again.Inject.BasicAuth.Username != "x-access-token" {
		t.Errorf("changing an expanded route changed the preset: %+v", again.Inject.BasicAuth)
	}
}

// A list the route sets replaces the preset's list whole, so a set that drops
// the bearer leaves no header carrying the credential, and is refused.
func TestExpandRouteRefusesAnOverrideThatDropsTheCredential(t *testing.T) {
	r := withSecret(v1alpha1.PresetClaudeCode)
	r.Inject = &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
		Set: []v1alpha1.Header{{Name: "X-Extra", Value: "1"}},
	}}
	if _, err := ExpandRoute(r); err == nil || !strings.Contains(err.Error(), "never reach upstream") {
		t.Errorf("ExpandRoute = %v, want a refusal naming the missing {value}", err)
	}
	r.Inject = &v1alpha1.Inject{BasicAuth: &v1alpha1.BasicAuth{Username: "u"}}
	if _, err := ExpandRoute(r); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("ExpandRoute = %v, want the two injects refused", err)
	}
}

// The guest's own Authorization carries a session token. Whatever the route
// writes, it never reaches upstream.
func TestExpandRouteAlwaysRemovesAuthorization(t *testing.T) {
	custom := v1alpha1.Route{
		Name:       "api",
		Upstream:   "https://api.example.com",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "tok"}},
		Guest:      &v1alpha1.Guest{Env: map[string]string{"API_URL": v1alpha1.PlaceholderRouteURL}},
	}
	headers := custom
	headers.Inject = &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
		Set:    []v1alpha1.Header{{Name: "X-Api-Key", Value: "{value}"}},
		Remove: []string{"X-Other"},
	}}
	basic := custom
	basic.Inject = &v1alpha1.Inject{BasicAuth: &v1alpha1.BasicAuth{Username: "x"}}
	override := withSecret(v1alpha1.PresetClaudeCode)
	override.Inject = &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{Remove: []string{"X-Other"}}}

	for name, r := range map[string]v1alpha1.Route{
		"custom headers":                    headers,
		"custom basicAuth":                  basic,
		"preset with its remove overridden": override,
		"claude-code":                       withSecret(v1alpha1.PresetClaudeCode),
		"anthropic-api-key":                 withSecret(v1alpha1.PresetAnthropicAPIKey),
		"github-token":                      withSecret(v1alpha1.PresetGitHubToken),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ExpandRoute(r)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(got.Inject.Headers.Remove, "Authorization") {
				t.Errorf("inject.headers.remove = %v, want Authorization in it", got.Inject.Headers.Remove)
			}
		})
	}
	// The default is added to the expanded copy, never to the caller's route.
	if !reflect.DeepEqual(headers.Inject.Headers.Remove, []string{"X-Other"}) {
		t.Errorf("ExpandRoute changed the route it was given: %v", headers.Inject.Headers.Remove)
	}
	if basic.Inject.Headers != nil {
		t.Errorf("ExpandRoute changed the route it was given: %+v", basic.Inject.Headers)
	}
}

func readExample(t *testing.T, name string) Policy {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "api", "policy", "v1alpha1", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(blob)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

// Every example of the spec parses, which expands its presets and checks each
// route in full.
func TestSpecExamplesParse(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "api", "policy", "v1alpha1", "testdata", "example*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, path := range paths {
		readExample(t, filepath.Base(path))
	}
}

// Example 5 of the spec attaches examples 1 and 3 to one profile. A route in
// either applies to the run, and the egress default is the one example 1
// names.
func TestRoutesMergesExampleFive(t *testing.T) {
	ps := []Policy{readExample(t, "example1-claude-only.yaml"), readExample(t, "example3-github-brig-sh.yaml")}
	routes, err := Routes(ps)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range routes {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"anthropic", "github"}) {
		t.Errorf("routes = %v, want anthropic and github", names)
	}
	if got := Merge(ps); got.Default != v1alpha1.EgressDeny {
		t.Errorf("egress default = %q, want deny", got.Default)
	}
}

func TestRoutesRefusesADuplicate(t *testing.T) {
	claudeOnly := readExample(t, "example1-claude-only.yaml")
	apiKey := readExample(t, "example2-claude-api-key.yaml")

	// Examples 1 and 2 both name their route anthropic.
	if _, err := Routes([]Policy{claudeOnly, apiKey}); err == nil ||
		!strings.Contains(err.Error(), "route anthropic is declared by both claude-only and claude-api-key") {
		t.Errorf("Routes = %v, want the duplicate route refused", err)
	}

	// A second route under another name still sets ANTHROPIC_BASE_URL.
	renamed := apiKey
	renamed.Spec.Routes = slices.Clone(apiKey.Spec.Routes)
	renamed.Spec.Routes[0].Name = "anthropic-metered"
	_, err := Routes([]Policy{claudeOnly, renamed})
	if err == nil || !strings.Contains(err.Error(), "guest.env ANTHROPIC_BASE_URL is set by route anthropic of claude-only") {
		t.Errorf("Routes = %v, want the duplicate guest.env name refused", err)
	}
}

// A routes-only policy adds no egress rule, so it does not turn on filtering
// by itself.
func TestMergeIgnoresAPolicyWithNoEgress(t *testing.T) {
	github := readExample(t, "example3-github-brig-sh.yaml")
	if got := Merge([]Policy{github}); got.Default != "" {
		t.Errorf("a routes-only policy produced a rule set: %+v", got)
	}
}

func TestParseRefusesARouteThatDoesNotExpand(t *testing.T) {
	head := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n"
	for _, c := range []struct{ name, route, want string }{
		{"lifecycle", "    - name: anthropic\n      preset: claude-code\n" +
			"      credential: {secretRef: {name: claude-credentials}, lifecycle: anthropic-oauth}\n",
			"set by a preset"},
		{"unknown preset", "    - name: openai\n      preset: openai\n" +
			"      credential: {secretRef: {name: k}}\n", "not one of"},
		{"unknown mode", "    - name: anthropic\n      preset: claude-code\n      mode: relay\n" +
			"      credential: {secretRef: {name: claude-credentials}}\n", "is not supported"},
		{"two routes on one variable", "    - name: anthropic\n      preset: claude-code\n" +
			"      credential: {secretRef: {name: claude-credentials}}\n" +
			"    - name: metered\n      preset: anthropic-api-key\n" +
			"      credential: {secretRef: {name: anthropic-api-key}}\n", "guest.env ANTHROPIC_BASE_URL"},
		{"override that drops the bearer", "    - name: anthropic\n      preset: claude-code\n" +
			"      credential: {secretRef: {name: claude-credentials}}\n" +
			"      inject: {headers: {set: [{name: X-Extra, value: \"1\"}]}}\n", "never reach upstream"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(head + c.route))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Parse = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

// git rewrites a prefix to one URL, so two github-token routes in two bound
// policies cannot both claim https://github.com/.
func TestRoutesRefusesASharedInsteadOfPrefix(t *testing.T) {
	github := readExample(t, "example3-github-brig-sh.yaml")
	other := github
	other.Metadata.Name = "github-other"
	other.Spec.Routes = []v1alpha1.Route{*github.Spec.Routes[0].DeepCopy()}
	other.Spec.Routes[0].Name = "github-other"
	other.Spec.Routes[0].Credential.SecretRef.Name = "other-token"
	_, err := Routes([]Policy{github, other})
	if err == nil || !strings.Contains(err.Error(), "guest.git.insteadOf https://github.com/ is rewritten by route github of github-brig-sh") {
		t.Errorf("Routes = %v, want the shared prefix refused", err)
	}
}
