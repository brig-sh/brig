package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
)

// githubRouteDoc binds a GitHub route whose credential is a secret the profile
// does not name, so withRoutes leaves the profile's GH_TOKEN binding alone.
func githubRouteDoc(preset string) string {
	return `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: gh
      preset: ` + preset + `
      credential: {secretRef: {name: gh-route-pat}}
`
}

// A sandbox that reaches GitHub through a route gets the route's session
// token, not the real token as well. GH_TOKEN and GITHUB_TOKEN are withheld
// whether they come from the environment, from the store or from
// BRIG_FORWARD_ENV, and every report agrees.
func TestAGitHubRouteWithholdsGHToken(t *testing.T) {
	for _, preset := range []string{"github-api", "github-token"} {
		t.Run(preset, func(t *testing.T) {
			t.Setenv("BRIG_FORWARD_ENV", "GITHUB_TOKEN")
			t.Setenv("GITHUB_TOKEN", "ambient-github-token-value")
			c := routedConfig(t, githubRouteDoc(preset), fakeRuntime{})
			for _, b := range append(c.Env, c.Profile.Env...) {
				if b.Name == "GH_TOKEN" || b.Name == "GITHUB_TOKEN" {
					t.Errorf("%s is still bound: %+v", b.Name, b)
				}
			}
			for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
				want := "not forwarding " + name + ": this sandbox reaches GitHub through route gh, which keeps the token on the host"
				if !strings.Contains(strings.Join(c.envWarnings, "\n"), want) {
					t.Errorf("no notice %q in %q", want, c.envWarnings)
				}
			}

			// The store holds gh-token too: the secret ref is withheld with
			// the environment one.
			os.Unsetenv("GH_TOKEN")
			c.OpenStore = func() (creds.SecretReader, error) {
				return fakeSecretStore{"gh-token": "store-gh-token-value", "gh-route-pat": "{}"}, nil
			}
			set, err := c.BuildEnv()
			if err != nil {
				t.Fatal(err)
			}
			if set.Has("GH_TOKEN") || set.Has("GITHUB_TOKEN") {
				t.Fatalf("the guest gets a GitHub token: %v", set.Names)
			}
			out := &bytes.Buffer{}
			c.Out = out
			c.Status(set)
			// BRIG_ROUTE_GH_TOKEN is the route's session token, so match the
			// name alone.
			if regexp.MustCompile(`\b(GH|GITHUB)_TOKEN\b`).MatchString(out.String()) || strings.Contains(out.String(), "token-value") {
				t.Errorf("the report names a GitHub token:\n%s", out)
			}
		})
	}
}

// A policy bound to one session by label (`brig policy attach -n`) counts.
func TestAGitHubRouteBoundToASessionWithholdsGHToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brokered.yaml"), []byte(githubRouteDoc("github-api")), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := "sessions:\n  claude-code:\n    foo:\n      - brokered\n"
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_POLICY_DIR", dir)
	t.Setenv("BRIG_STATE_DIR", t.TempDir())
	t.Setenv("BRIG_WORKSPACE", t.TempDir())
	t.Setenv("GH_TOKEN", "ambient-gh-token-value")
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	c, err := Load(p, Options{Name: "foo"}, fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range c.Env {
		if b.Name == "GH_TOKEN" {
			t.Errorf("GH_TOKEN is still bound for session foo: %+v", b)
		}
	}
	other, err := Load(p, Options{Name: "bar"}, fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	if !bindsName(other.Env, "GH_TOKEN") {
		t.Error("GH_TOKEN was withheld from a session with no GitHub route")
	}
}

// Without a GitHub route, GH_TOKEN is forwarded as before, and the report
// says that a route would keep it on the host.
func TestGHTokenWithoutAGitHubRouteIsForwardedWithAHint(t *testing.T) {
	const egressOnly = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  egress: {default: allow}
`
	c := routedConfig(t, egressOnly, fakeRuntime{})
	set, err := c.BuildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has("GH_TOKEN") {
		t.Fatalf("GH_TOKEN was withheld with no GitHub route: %v", set.Names)
	}
	out := &bytes.Buffer{}
	c.Out = out
	c.Status(set)
	want := "GH_TOKEN is in the guest; a github-api or github-token route keeps it on the host, see https://www.brig.sh/docs/broker/#the-github-api-preset"
	if !strings.Contains(out.String(), want) {
		t.Errorf("the report lacks %q:\n%s", want, out)
	}
}

// BRIG_FORWARD_GITHUB_TOKEN forwards GH_TOKEN beside the route, and says
// that the real token is now in the guest.
func TestForwardGitHubTokenKeepsGHTokenBesideAGitHubRoute(t *testing.T) {
	t.Setenv("BRIG_FORWARD_GITHUB_TOKEN", "1")
	c := routedConfig(t, githubRouteDoc("github-api"), fakeRuntime{})
	if !bindsName(c.Env, "GH_TOKEN") {
		t.Fatal("BRIG_FORWARD_GITHUB_TOKEN=1 did not forward GH_TOKEN")
	}
	want := "forwarding GH_TOKEN because BRIG_FORWARD_GITHUB_TOKEN=1 is set: the real token is in the guest beside route gh"
	if !strings.Contains(strings.Join(c.envWarnings, "\n"), want) {
		t.Errorf("no warning %q in %q", want, c.envWarnings)
	}
}

// Without a github-token route, git has no token once GH_TOKEN is withheld,
// because the guest's git credential helper reads GH_TOKEN. The notice says
// so. With a github-token route, git goes through the route and the notice
// does not mention git.
func TestAGitHubAPIRouteAloneSaysGitNeedsAGitHubTokenRoute(t *testing.T) {
	const gitNote = "git over HTTPS to github.com needs a github-token route"
	const both = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: gh
      preset: github-api
      credential: {secretRef: {name: gh-route-pat}}
    - name: git
      preset: github-token
      credential: {secretRef: {name: gh-git-pat}}
`
	for _, tc := range []struct {
		name, doc string
		wantNote  bool
	}{
		{"github-api", githubRouteDoc("github-api"), true},
		{"github-token", githubRouteDoc("github-token"), false},
		{"both", both, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := routedConfig(t, tc.doc, fakeRuntime{})
			warnings := strings.Join(c.envWarnings, "\n")
			if !strings.Contains(warnings, "not forwarding GH_TOKEN") {
				t.Fatalf("GH_TOKEN was not withheld: %q", c.envWarnings)
			}
			if got := strings.Contains(warnings, gitNote); got != tc.wantNote {
				t.Errorf("notice names the git route = %v, want %v: %q", got, tc.wantNote, c.envWarnings)
			}
		})
	}
}

// A github-token route moved to another host does not reach github.com, so
// GH_TOKEN for github.com is forwarded as before.
func TestAGitHubRouteToAnotherHostKeepsGHToken(t *testing.T) {
	const ghe = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: ghe
      preset: github-token
      upstream: https://ghe.example.com
      credential: {secretRef: {name: gh-route-pat}}
`
	c := routedConfig(t, ghe, fakeRuntime{})
	if !bindsName(c.Env, "GH_TOKEN") {
		t.Errorf("GH_TOKEN was withheld for a route to another host: %q", c.envWarnings)
	}
}

// BRIG_ALLOW_DENIED overrides the billing denylist, not the GitHub route: a
// user who sets it for a metered key does not also put the GitHub token in
// the guest.
func TestAllowDeniedDoesNotForwardGHTokenBesideAGitHubRoute(t *testing.T) {
	t.Setenv("BRIG_ALLOW_DENIED", "1")
	c := routedConfig(t, githubRouteDoc("github-api"), fakeRuntime{})
	if bindsName(c.Env, "GH_TOKEN") {
		t.Errorf("BRIG_ALLOW_DENIED=1 forwarded GH_TOKEN beside route gh: %q", c.envWarnings)
	}
}

func bindsName(bindings []profile.EnvBinding, name string) bool {
	for _, b := range bindings {
		if b.Name == name {
			return true
		}
	}
	return false
}
