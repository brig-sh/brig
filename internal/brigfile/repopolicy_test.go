package brigfile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/policy"
)

const repoPolicyBrigfile = `apiVersion: brig.sh/v1alpha1
profiles:
  tool: {file: tool.yaml}
policies:
  gh: {file: gh.yaml}
targets:
  dev: {profile: tool, policies: [gh]}
`

// repoPolicy is a policy with one route, written the way a file writes it.
func repoPolicy(route string) string {
	return "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: gh}\nspec:\n  routes:\n" + route
}

// loadRepoPolicy loads a Brigfile whose one policy is doc, from a file and
// then inline, and returns both results.
func loadRepoPolicy(t *testing.T, doc string) map[string]error {
	t.Helper()
	out := map[string]error{}
	dir := project(t, map[string]string{FileName: repoPolicyBrigfile, "tool.yaml": repoProfileBase, "gh.yaml": doc})
	_, out["file"] = Load(filepath.Join(dir, FileName))
	inline := "    inline:\n      " + strings.ReplaceAll(strings.TrimRight(doc, "\n"), "\n", "\n      ") + "\n"
	bf := strings.Replace(repoPolicyBrigfile, "  gh: {file: gh.yaml}\n", "  gh:\n"+inline, 1)
	dir = project(t, map[string]string{FileName: bf, "tool.yaml": repoProfileBase})
	_, out["inline"] = Load(filepath.Join(dir, FileName))
	return out
}

// forge is a route in full to a forge of the repository's own, taking over
// git for the prefixes given.
func forge(prefixes string) string {
	return "    - name: forge\n      upstream: https://git.example.com\n" +
		"      credential: {secretRef: {name: forge-token}}\n" +
		"      inject: {basicAuth: {username: x-access-token}}\n" +
		"      guest: {git: {insteadOf: [" + prefixes + "]}}\n"
}

// preset is a route on a preset with the lines given after the secret.
func preset(name, extra string) string {
	return "    - name: r\n      preset: " + name + "\n      credential: {secretRef: {name: tok}}\n" + extra
}

// A route of a repository policy may take over git only for the host it
// sends to. Each prefix below would put git meant for another host, or for
// more than the upstream, on the route, and is refused in a file and inline.
// The file outside the rule, the reader's own policy, is the test after.
func TestARepositoryRouteTakesGitOnlyForItsOwnUpstream(t *testing.T) {
	cases := map[string]string{
		"another host":                         forge(`"https://github.com/"`),
		"another host, the SSH form":           forge(`"git@github.com:"`),
		"one right and one wrong":              forge(`"https://git.example.com/", "https://github.com/"`),
		"a host that only starts so":           forge(`"https://git.example.com.elsewhere.net/"`),
		"no slash after the host":              forge(`"https://git.example.com"`),
		"the upstream as a user name":          forge(`"https://git.example.com@elsewhere.net/"`),
		"a password":                           forge(`"https://u:p@git.example.com/"`),
		"another port":                         forge(`"https://git.example.com:8443/"`),
		"another port, the scheme in capitals": forge(`"HTTPS://git.example.com:8443/"`),
		"ssh:// to another host":               forge(`"ssh://git@github.com/"`),
		"no host at all":                       forge(`"git@:"`),
		"a path, not a URL":                    forge(`"/srv/git/"`),
		"a preset, rewrites written":           preset("github-token", "      guest: {git: {insteadOf: [\"https://gitlab.com/\"]}}\n"),
		"a moved preset, old rewrites": preset("github-token", "      upstream: https://git.example.com\n"+
			"      guest: {git: {insteadOf: [\"https://github.com/\"]}}\n"),
	}
	for name, route := range cases {
		for form, err := range loadRepoPolicy(t, repoPolicy(route)) {
			t.Run(name+"/"+form, func(t *testing.T) {
				if err == nil {
					t.Fatal("Load accepted a route that takes over git for another host")
				}
				for _, want := range []string{"policies.gh", "guest.git.insteadOf", "repository policy may take over git only"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error does not say %q: %v", want, err)
					}
				}
			})
		}
	}
}

// The forms of a URL on the upstream's own host are all accepted, and so is
// a preset moved to another host, whose rewrites move with it: that is a
// GitHub Enterprise host in four lines, with the preset's repository rule.
func TestARepositoryRouteMayTakeGitForItsOwnUpstream(t *testing.T) {
	for name, route := range map[string]string{
		"https":                      forge(`"https://git.example.com/"`),
		"https and the port":         forge(`"https://git.example.com:443/"`),
		"a path under it":            forge(`"https://git.example.com/team/"`),
		"the host in capitals":       forge(`"https://GIT.example.com/"`),
		"git@host:":                  forge(`"git@git.example.com:"`),
		"ssh://":                     forge(`"ssh://git@git.example.com/"`),
		"ssh:// with a port":         forge(`"ssh://git@git.example.com:2222/"`),
		"the preset as it ships":     preset("github-token", ""),
		"the preset on another host": preset("github-token", "      upstream: https://ghe.example.com\n"),
		"the preset on another host, with repositories": preset("github-token",
			"      upstream: https://ghe.example.com\n      request: {allow: {repos: [\"team/*\"]}}\n"),
		"what the preset already says, written out": preset("github-token", "      upstream: https://github.com\n"+
			"      inject: {basicAuth: {username: x-access-token}}\n"),
	} {
		for form, err := range loadRepoPolicy(t, repoPolicy(route)) {
			if err != nil {
				t.Errorf("%s, %s: Load: %v", name, form, err)
			}
		}
	}
}

// A preset stays what brig ships in the rest. One the guest finds through a
// variable keeps its upstream, since a variable names no host to hold it
// to. No preset's injection changes, and a request rule a preset ships
// stays.
func TestARepositoryPolicyKeepsAPresetAsItShips(t *testing.T) {
	cases := map[string]struct {
		route string
		want  []string
	}{
		"the upstream of a preset found by a variable": {
			preset("anthropic-api-key", "      upstream: https://llm.example.com\n"),
			[]string{"upstream", "https://api.anthropic.com", "https://llm.example.com"}},
		"the upstream of another preset found by a variable": {
			preset("github-api", "      mode: host\n      upstream: https://api.ghe.example.com\n"),
			[]string{"upstream", "https://api.github.com", "https://api.ghe.example.com"}},
		"inject": {preset("github-token", "      inject: {basicAuth: {username: someone}}\n"),
			[]string{"inject"}},
		"request": {preset("aws", "      request: {allow: {methods: [GET, POST]}}\n"),
			[]string{"request"}},
	}
	for name, c := range cases {
		if _, err := policy.Parse([]byte(repoPolicy(c.route))); err != nil {
			t.Fatalf("%s: the fixture is refused as a policy, before the rule is asked: %v", name, err)
		}
		for form, err := range loadRepoPolicy(t, repoPolicy(c.route)) {
			t.Run(name+"/"+form, func(t *testing.T) {
				if err == nil {
					t.Fatal("Load accepted a preset the repository changed")
				}
				for _, want := range append(c.want, "policies.gh", "route r", "repository policy may not") {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error does not say %q: %v", want, err)
					}
				}
			})
		}
	}
}

// Both rules are the repository's alone. The same routes in the reader's
// own policy directory still load: a mirror in front of github.com, and a
// preset with another way to send the secret.
func TestAPolicyOfYourOwnIsNotHeldToTheRepositoryRules(t *testing.T) {
	for name, route := range map[string]string{
		"git for another host": forge(`"https://github.com/"`),
		"a moved preset with the old rewrites": preset("github-token", "      upstream: https://git.example.com\n"+
			"      guest: {git: {insteadOf: [\"https://github.com/\"]}}\n"),
		"inject": preset("github-token", "      inject: {basicAuth: {username: someone}}\n"),
		"the upstream of a preset found by a variable": preset("anthropic-api-key", "      upstream: https://llm.example.com\n"),
	} {
		if _, err := policy.Parse([]byte(repoPolicy(route))); err != nil {
			t.Errorf("%s: policy.Parse refused what only a repository policy is refused: %v", name, err)
		}
	}
}

// Every preset brig ships, in every mode it supports, loads from a
// repository policy that only names the secret. The rule compares the route
// after the preset fills it in, so a mode that sets a default of its own
// must not read as a change the repository made.
func TestARepositoryPolicyMayUseEveryPresetAsItShips(t *testing.T) {
	for _, preset := range policy.PresetNames() {
		modes, err := policy.PresetModes(preset)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range modes {
			route := "    - name: r\n      preset: " + string(preset) + "\n      mode: " + string(mode) + "\n" +
				"      credential: {secretRef: {name: tok}}\n"
			// The one preset that asks the route for more than a secret.
			if preset == "gcp" {
				route += "      services: [{name: storage}]\n"
			}
			if _, err := policy.Parse([]byte(repoPolicy(route))); err != nil {
				// A mode that needs more than a secret is not this test's
				// to write out; the rule is tried only on what parses.
				t.Logf("%s in mode %s needs more than a secret: %v", preset, mode, err)
				continue
			}
			for form, err := range loadRepoPolicy(t, repoPolicy(route)) {
				if err != nil {
					t.Errorf("%s in mode %s, %s: Load: %v", preset, mode, form, err)
				}
			}
		}
	}
}

// A repository policy may not declare a passthrough route, with or without a
// preset. The refusal names passthrough, not the inject a preset would have
// set, and the same route in the reader's own policy still parses.
func TestARepositoryPolicyMayNotDeclareAPassthroughRoute(t *testing.T) {
	for name, route := range map[string]string{
		"on a preset": "    - name: r\n      preset: anthropic-api-key\n      passthrough: true\n",
		"with no preset": "    - name: r\n      upstream: https://llm.example.com\n      passthrough: true\n" +
			"      guest: {env: {LLM_URL: \"{route.url}\"}}\n",
	} {
		if _, err := policy.Parse([]byte(repoPolicy(route))); err != nil {
			t.Fatalf("%s: the fixture is refused as a policy, before the rule is asked: %v", name, err)
		}
		for form, err := range loadRepoPolicy(t, repoPolicy(route)) {
			t.Run(name+"/"+form, func(t *testing.T) {
				if err == nil {
					t.Fatal("Load accepted a passthrough route in a repository policy")
				}
				for _, want := range []string{"policies.gh", "route r", "passthrough",
					"repository policy may not declare a passthrough route", "a policy of your own"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error does not say %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), "says how the secret is sent") {
					t.Errorf("the error reads the passthrough route as a changed inject: %v", err)
				}
			})
		}
	}
	// The same preset route without passthrough is unchanged.
	for form, err := range loadRepoPolicy(t, repoPolicy(preset("anthropic-api-key", ""))) {
		if err != nil {
			t.Errorf("anthropic-api-key as it ships, %s: Load: %v", form, err)
		}
	}
}
