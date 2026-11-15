package policy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

func githubRoute(upstream string, insteadOf ...string) v1alpha1.Route {
	r := v1alpha1.Route{Name: "gh", Preset: v1alpha1.PresetGitHubToken, Upstream: upstream,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "tok"}}}
	if len(insteadOf) > 0 {
		r.Guest = &v1alpha1.Guest{Git: &v1alpha1.GitGuest{InsteadOf: insteadOf}}
	}
	return r
}

// A preset's git rewrites follow its upstream. github-token on another host
// rewrites git for that host, and no longer for github.com: naming a GitHub
// Enterprise host is enough to use it, and git for github.com does not go to
// it. Rewrites the route writes out are kept as written.
func TestAPresetsGitRewritesFollowItsUpstream(t *testing.T) {
	for name, c := range map[string]struct {
		route v1alpha1.Route
		want  string
	}{
		"as it ships":      {githubRoute(""), "https://github.com/ git@github.com:"},
		"its own upstream": {githubRoute("https://github.com"), "https://github.com/ git@github.com:"},
		"another host":     {githubRoute("https://ghe.example.com"), "https://ghe.example.com/ git@ghe.example.com:"},
		"another host and port": {githubRoute("https://ghe.example.com:8443"),
			"https://ghe.example.com:8443/ git@ghe.example.com:"},
		"rewrites written out": {githubRoute("https://mirror.example.com", "https://github.com/"), "https://github.com/"},
	} {
		got, err := ExpandRoute(c.route)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if have := strings.Join(got.Guest.Git.InsteadOf, " "); have != c.want {
			t.Errorf("%s: git rewrites %q, want %q", name, have, c.want)
		}
		// The broker expands what brig sends it, which is already expanded.
		again, err := ExpandRoute(got)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Errorf("%s: expanding the expanded route changed it (%v):\n%+v\n%+v", name, err, got, again)
		}
	}
	// The preset itself is not changed by a route that moved.
	if _, err := ExpandRoute(githubRoute("https://ghe.example.com")); err != nil {
		t.Fatal(err)
	}
	base, err := PresetRoute(v1alpha1.PresetGitHubToken)
	if err != nil || strings.Join(base.Guest.Git.InsteadOf, " ") != "https://github.com/ git@github.com:" {
		t.Errorf("the preset's own rewrites are %v (%v)", base.Guest.Git.InsteadOf, err)
	}
}
