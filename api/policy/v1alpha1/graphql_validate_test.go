package v1alpha1

import "testing"

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
