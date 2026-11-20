package v1alpha1

import (
	"strings"
	"testing"
)

func TestGraphQLAllowValidate(t *testing.T) {
	for _, c := range []struct {
		name   string
		g      GraphQLAllow
		preset Preset
		want   string
	}{
		{"comment only", GraphQLAllow{Queries: []string{"repository"}, Mutations: []string{"addComment"}, Repos: []string{"brig-sh/brig"}}, PresetGitHubAPI, ""},
		{"no repos, any query", GraphQLAllow{Queries: []string{"*"}}, PresetGitHubAPI, ""},
		{"wrong preset", GraphQLAllow{Queries: []string{"*"}}, PresetGitHubToken, "request.allow.graphql needs preset: github-api"},
		{"lists nothing", GraphQLAllow{Repos: []string{"a/b"}}, PresetGitHubAPI, "request.allow.graphql lists no queries and no mutations; list at least one"},
		{"repos with *", GraphQLAllow{Queries: []string{"*"}, Repos: []string{"a/b"}}, PresetGitHubAPI,
			`request.allow.graphql.queries "*" bypasses repos; with repos, list only repository and rateLimit`},
		{"repos with node", GraphQLAllow{Queries: []string{"repository", "node"}, Repos: []string{"a/b"}}, PresetGitHubAPI,
			`request.allow.graphql.queries "node" bypasses repos; with repos, list only repository and rateLimit`},
		{"repos with viewer", GraphQLAllow{Queries: []string{"viewer"}, Repos: []string{"a/b"}}, PresetGitHubAPI,
			`request.allow.graphql.queries "viewer" bypasses repos; with repos, list only repository and rateLimit`},
		{"repo not ASCII", GraphQLAllow{Queries: []string{"repository"}, Repos: []string{"\u212Aelvin/x"}}, PresetGitHubAPI,
			`request.allow.graphql.repos "` + "\u212A" + `elvin/x" must be owner/name in ASCII`},
		{"bad field name", GraphQLAllow{Mutations: []string{"add-comment"}}, PresetGitHubAPI,
			`request.allow.graphql.mutations "add-comment" is not a GraphQL field name; use a name such as addComment, or *`},
	} {
		err := (&RequestAllow{GraphQL: &c.g}).validate(c.preset)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// An empty graphql.repos counted as unset, so queries: ["*"] next to it opened
// every repository the credential reaches. A reader takes it to mean none.
func TestGraphQLReposRefusesAnEmptyList(t *testing.T) {
	const want = "request.allow.graphql.repos is empty; list at least one, or leave the key out"
	doc := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: a}\nspec:\n  routes:\n" +
		"    - name: github-api\n      preset: github-api\n      credential: {secretRef: {name: gh-token}}\n" +
		"      request: {allow: {graphql: {queries: [\"*\"], repos: []}}}\n"
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a policy with graphql.repos: [] passed Validate, err = %v", err)
	}
	r := p.Spec.Routes[0]
	if err := r.DeepCopy().validateWritten(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("DeepCopy turned the empty list into an unset one, err = %v", err)
	}
}
