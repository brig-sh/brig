package broker

import (
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

func ghDo(r *rig, method, path, ctype, body string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	req.Host = "github-api.brig.internal"
	req.Header.Set("Authorization", "token "+r.mint("brig-a", "github-api"))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	r.lines++
	r.waitAudit(r.lines)
	return resp
}

// The ways around request.allow.graphql that a review found, each refused
// before the upstream sees it.
func TestGraphQLPolicyCannotBeWalkedAround(t *testing.T) {
	merge := `{"query":"mutation{mergePullRequest(input:{pullRequestId:\"PR_x\"}){clientMutationId}}"}`
	comment := `{"query":"mutation{addComment(input:{subjectId:\"PR_x\",body:\"hi\"}){clientMutationId}}"}`
	gql := &v1alpha1.GraphQLAllow{Queries: []string{"repository"}, Mutations: []string{"addComment"}, Repos: []string{"brig-sh/brig"}}
	only := &v1alpha1.RequestAllow{GraphQL: gql}
	for _, c := range []struct {
		name                      string
		allow                     *v1alpha1.RequestAllow
		method, path, ctype, body string
		want                      int
	}{
		{"REST merge", only, "PUT", "/api/v3/repos/brig-sh/brig/pulls/1/merge", "application/json", `{}`, 403},
		{"REST dispatch with methods [GET, POST]", &v1alpha1.RequestAllow{Methods: []string{"GET", "POST"}, GraphQL: gql},
			"POST", "/api/v3/repos/NOFireAI/engineering/actions/workflows/deploy.yml/dispatches", "application/json", `{"ref":"main"}`, 403},
		{"REST DELETE", only, "DELETE", "/api/v3/repos/brig-sh/brig", "", "", 403},
		{"/api/v3/GraphQL", only, "POST", "/api/v3/GraphQL", "application/json", merge, 403},
		{"/api/v3/graphql/", only, "POST", "/api/v3/graphql/", "application/json", merge, 403},
		{"GET /api/v3/GRAPHQL", only, "GET", "/api/v3/GRAPHQL?query=mutation", "", "", 403},
		{"/api/v3/graphql/x", only, "POST", "/api/v3/graphql/x", "application/json", merge, 403},
		{"/api/v3/graphql", only, "POST", "/api/v3/graphql", "application/json", merge, 403},
		{"graphq%6C", only, "POST", "/api/v3/graphq%6C", "application/json", merge, 403},
		{"GET with a query string", only, "GET", "/api/graphql?query=mutation%7BmergePullRequest%7D", "", "", 403},
		// methods applies to REST alone, so it does not block GraphQL.
		{"comment with methods [GET]", &v1alpha1.RequestAllow{Methods: []string{"GET"}, GraphQL: gql},
			"POST", "/api/graphql", "application/json", comment, 200},
		{"REST read stays", only, "GET", "/api/v3/repos/brig-sh/brig/pulls/1", "", "", 200},
		{"REST read under paths", &v1alpha1.RequestAllow{Paths: []string{"/api/v3/user"}, GraphQL: gql},
			"GET", "/api/v3/repos/brig-sh/brig", "", "", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, up := gqlRig(t, c.allow)
			resp := ghDo(r, c.method, c.path, c.ctype, c.body)
			wantStatus(t, resp, c.want)
			if c.want == 403 && up.count() != 0 {
				t.Fatalf("the upstream saw %s", up.last(t).path)
			}
		})
	}
}
