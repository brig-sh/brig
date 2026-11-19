package v1alpha1

import (
	"strings"
	"testing"
)

// passthrough is a custom passthrough route, complete as written.
func passthrough() Route {
	return Route{
		Name:        "models",
		Upstream:    "https://api.example.com",
		Passthrough: true,
		Guest:       &Guest{Env: map[string]string{"MODELS_URL": PlaceholderRouteURL}},
		Request:     &RequestPolicy{Allow: &RequestAllow{Methods: []string{"POST"}}},
	}
}

func TestPassthroughRouteValidates(t *testing.T) {
	r := passthrough()
	if err := r.Validate(); err != nil {
		t.Fatalf("a complete passthrough route was refused: %v", err)
	}
	if err := r.validateWritten(); err != nil {
		t.Fatalf("a passthrough route as written was refused: %v", err)
	}
}

// Each refusal keeps a value from the broker or the guest that a passthrough
// route has no use for, or that would take the place of the guest's own
// credential.
func TestPassthroughRouteRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Route)
		want string
	}{
		{"a credential", func(r *Route) {
			r.Credential = &CredentialRef{SecretRef: SecretRef{Name: "api-key"}}
		}, "takes no credential"},
		{"an inject", func(r *Route) {
			r.Inject = &Inject{Headers: &HeaderInject{Remove: []string{"Authorization"}}}
		}, "takes no inject"},
		{"a session token in the guest", func(r *Route) {
			r.Guest.Env["MODELS_TOKEN"] = PlaceholderSessionToken
		}, "has no session token"},
		{"git in the guest", func(r *Route) {
			r.Guest.Git = &GitGuest{InsteadOf: []string{"https://api.example.com/"}}
		}, "takes no guest.git"},
		{"the aws preset", func(r *Route) { r.Preset, r.Upstream = PresetAWS, "" }, "not aws"},
		{"the gcp preset", func(r *Route) {
			r.Preset, r.Upstream, r.Services = PresetGCP, "", []Service{{Name: "storage"}}
		}, "not gcp"},
		{"the github-api preset", func(r *Route) { r.Preset = PresetGitHubAPI }, "not github-api"},
		{"the github-token preset", func(r *Route) { r.Preset = PresetGitHubToken }, "not github-token"},
		{"no upstream", func(r *Route) { r.Upstream = "" }, "upstream is required"},
		{"no guest", func(r *Route) { r.Guest = nil }, "guest is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := passthrough()
			c.edit(&r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want an error with %q", err, c.want)
			}
		})
	}
}

// A route that is not passthrough still needs a credential, so leaving it out
// by mistake never turns a route into one that forwards the guest's headers.
func TestARouteWithoutCredentialIsNotPassthrough(t *testing.T) {
	r := passthrough()
	r.Passthrough = false
	r.Inject = &Inject{Headers: &HeaderInject{Set: []Header{{Name: "X-Api-Key", Value: PlaceholderValue}}}}
	if err := r.validateWritten(); err == nil || !strings.Contains(err.Error(), "credential.secretRef.name is required") {
		t.Fatalf("validateWritten = %v, want the missing credential named", err)
	}
}

func TestDecodeReadsPassthrough(t *testing.T) {
	var r Route
	if err := UnmarshalStrict([]byte("name: models\nupstream: https://api.example.com\npassthrough: true\n"+
		"guest:\n  env:\n    MODELS_URL: \"{route.url}\"\n"), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Passthrough {
		t.Fatal("passthrough: true decoded as false")
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("the decoded route was refused: %v", err)
	}
}
