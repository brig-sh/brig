package policy

import (
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// A passthrough route on an Anthropic preset keeps the preset's upstream and
// base URL, and none of what carries the broker's credential.
func TestExpandRouteDropsTheBrokerCredentialOfAPassthroughRoute(t *testing.T) {
	for _, c := range []struct {
		preset   v1alpha1.Preset
		tokenVar string
	}{
		{v1alpha1.PresetAnthropicAPIKey, "ANTHROPIC_API_KEY"},
		{v1alpha1.PresetClaudeCode, "CLAUDE_CODE_OAUTH_TOKEN"},
	} {
		t.Run(string(c.preset), func(t *testing.T) {
			out, err := ExpandRoute(v1alpha1.Route{Name: "anthropic", Preset: c.preset, Passthrough: true,
				Request: &v1alpha1.RequestPolicy{Body: &v1alpha1.RequestBody{Models: []string{"claude-*"}}}})
			if err != nil {
				t.Fatal(err)
			}
			if !out.Passthrough {
				t.Error("the expanded route is not passthrough")
			}
			if out.Credential != nil || out.Inject != nil {
				t.Errorf("the expanded route has credential %+v and inject %+v", out.Credential, out.Inject)
			}
			if out.Upstream != "https://api.anthropic.com" {
				t.Errorf("upstream %q, want the preset's", out.Upstream)
			}
			if got := out.Guest.Env["ANTHROPIC_BASE_URL"]; got != v1alpha1.PlaceholderRouteURL {
				t.Errorf("ANTHROPIC_BASE_URL = %q, want %s", got, v1alpha1.PlaceholderRouteURL)
			}
			if v, ok := out.Guest.Env[c.tokenVar]; ok {
				t.Errorf("guest.env %s = %q; the guest's own value would be replaced", c.tokenVar, v)
			}
			if out.Request == nil || out.Request.Body == nil {
				t.Error("the expanded route lost its request.body rules")
			}
		})
	}
}

// Expanding a passthrough route leaves the preset as it is for the next
// route that names it.
func TestExpandRoutePassthroughLeavesThePresetAlone(t *testing.T) {
	if _, err := ExpandRoute(v1alpha1.Route{Name: "a", Preset: v1alpha1.PresetAnthropicAPIKey, Passthrough: true}); err != nil {
		t.Fatal(err)
	}
	out, err := ExpandRoute(withSecret(v1alpha1.PresetAnthropicAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	if out.Inject == nil || out.Guest.Env["ANTHROPIC_API_KEY"] != v1alpha1.PlaceholderSessionToken {
		t.Errorf("a credential route after a passthrough one has inject %+v and env %v", out.Inject, out.Guest.Env)
	}
}

// A passthrough route that names a credential anyway is refused, and so is
// one on a preset that needs a credential to check its requests.
func TestExpandRouteRefusesAPassthroughRouteWithACredential(t *testing.T) {
	r := withSecret(v1alpha1.PresetAnthropicAPIKey)
	r.Passthrough = true
	if _, err := ExpandRoute(r); err == nil {
		t.Error("a passthrough route with credential.secretRef expanded")
	}
	for _, preset := range []v1alpha1.Preset{v1alpha1.PresetGitHubAPI, v1alpha1.PresetGitHubToken} {
		if _, err := ExpandRoute(v1alpha1.Route{Name: "gh", Preset: preset, Passthrough: true}); err == nil {
			t.Errorf("a passthrough route on %s expanded", preset)
		}
	}
}
