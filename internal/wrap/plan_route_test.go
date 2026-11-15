package wrap

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// The row of a route says which of the guest's requests go through it: the
// git URLs it takes over and the variables it sets. It names the preset,
// and says when a preset is used in place of its own upstream. A route with
// none of that reads as it did.
func TestTheRouteRowSaysWhatPutsTrafficOnTheRoute(t *testing.T) {
	plain := PlanRoute{Name: "api", Policy: "brigfile p (inline)", Upstream: "https://api.example",
		Secrets: []string{"tok"}}
	cases := map[string]struct {
		route PlanRoute
		want  string
	}{
		"a plain route is unchanged": {plain,
			"route api to https://api.example with your secret tok, from brigfile p (inline)"},
		"a preset as it ships": {
			PlanRoute{Name: "github", Policy: "p", Preset: "github-token", Upstream: "https://github.com",
				Secrets: []string{"tok"}, GitInsteadOf: []string{"https://github.com/", "git@github.com:"}},
			"route github to https://github.com with your secret tok, from p; preset github-token; " +
				"git in the guest uses it for https://github.com/, git@github.com:"},
		"a preset pointed elsewhere": {
			PlanRoute{Name: "github", Policy: "p", Preset: "github-token", Upstream: "https://git.example.com",
				Secrets: []string{"tok"}, GitInsteadOf: []string{"https://github.com/"}},
			"route github to https://git.example.com with your secret tok, from p; preset github-token, " +
				"in place of its own upstream https://github.com; git in the guest uses it for https://github.com/"},
		"a route in full that takes git": {
			PlanRoute{Name: "github", Policy: "p", Upstream: "https://git.example.com", Secrets: []string{"tok"},
				GitInsteadOf: []string{"https://github.com/"}},
			"route github to https://git.example.com with your secret tok, from p; " +
				"git in the guest uses it for https://github.com/"},
		"a preset route shown as written, with no upstream yet": {
			PlanRoute{Name: "github", Policy: "p", Preset: "github-token", Secrets: []string{"tok"}},
			"route github to  with your secret tok, from p; preset github-token"},
		"a route that sets a guest variable": {
			PlanRoute{Name: "api", Policy: "p", Upstream: "https://api.example", Secrets: []string{"tok"},
				GuestEnv: []string{"ANTHROPIC_BASE_URL"}},
			"route api to https://api.example with your secret tok, from p; it sets ANTHROPIC_BASE_URL in the guest"},
	}
	for name, c := range cases {
		if got := routeText(c.route); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
	}
}

// The plan takes the rewrites and the variables from the route, sorted, and
// leaves both out of a route that has neither.
func TestPlanRoutesCarryTheGuestWiring(t *testing.T) {
	routes := []v1alpha1.Route{
		{Name: "forge", Upstream: "https://git.example.com", Guest: &v1alpha1.Guest{
			Git: &v1alpha1.GitGuest{InsteadOf: []string{"https://github.com/"}},
			Env: map[string]string{"B_URL": "{route.url}", "A_URL": "{route.url}"}}},
		{Name: "bare", Upstream: "https://api.example"},
	}
	got := planRoutesOf(routes, func(string) string { return "p" })
	if len(got) != 2 || got[0].Name != "bare" || got[0].GitInsteadOf != nil || got[0].GuestEnv != nil {
		t.Fatalf("bare: %+v", got)
	}
	if strings.Join(got[1].GitInsteadOf, ",") != "https://github.com/" || strings.Join(got[1].GuestEnv, ",") != "A_URL,B_URL" {
		t.Errorf("forge: %+v", got[1])
	}
}
