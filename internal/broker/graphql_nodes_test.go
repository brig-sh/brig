package broker

import (
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// A mutation names its target by node ID. With repos, the broker looks up
// the repository of each ID and refuses the request unless every one is
// listed, so "comment on brig-sh/brig" cannot comment elsewhere.
func TestGraphQLReposLimitMutations(t *testing.T) {
	allow := &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{
		Queries:   []string{"repository"},
		Mutations: []string{"addComment"},
		Repos:     []string{"brig-sh/brig"},
	}}
	const js = "application/json"
	for _, c := range []struct {
		name, body string
		status     int
		audit      []string
		bodyHas    string
	}{
		{"comment in the listed repository", `{"query":"mutation{addComment(input:{subjectId:\"PR_x\",body:\"hi\"}){clientMutationId}}"}`,
			200, []string{`resolved="PR_x=brig-sh/brig"`}, ""},
		{"the repository's own ID", `{"query":"mutation{addComment(input:{subjectId:\"R_1\",body:\"hi\"}){clientMutationId}}"}`,
			200, []string{`resolved="R_1=brig-sh/brig"`}, ""},
		{"comment in another repository", `{"query":"mutation{addComment(input:{subjectId:\"I_other\",body:\"hi\"}){clientMutationId}}"}`,
			403, []string{"check=request.allow.graphql", `resolved="I_other=NOFireAI/engineering"`},
			`node I_other at addComment.input.subjectId is in a repository that is not in the allow list [brig-sh/brig]`},
		{"the ID in a variable", `{"query":"mutation($s:ID!){addComment(input:{subjectId:$s,body:\"hi\"}){clientMutationId}}","variables":{"s":"I_other"}}`,
			403, []string{"check=request.allow.graphql"}, `node I_other at addComment.input.subjectId is in a repository that is not in the allow list`},
		{"the input in a variable", `{"query":"mutation($i:AddCommentInput!){addComment(input:$i){clientMutationId}}","variables":{"i":{"subjectId":"I_other","body":"hi"}}}`,
			403, []string{"check=request.allow.graphql"}, `node I_other at addComment.input.subjectId is in a repository that is not in the allow list`},
		{"behind an alias, after an allowed one", `{"query":"mutation{a: addComment(input:{subjectId:\"PR_x\",body:\"x\"}){clientMutationId} b: addComment(input:{subjectId:\"I_other\",body:\"y\"}){clientMutationId}}"}`,
			403, []string{"check=request.allow.graphql", `resolved="PR_x=brig-sh/brig,I_other=NOFireAI/engineering"`}, `node I_other`},
		{"an ID that resolves to nothing", `{"query":"mutation{addComment(input:{subjectId:\"I_nope\",body:\"hi\"}){clientMutationId}}"}`,
			403, []string{"check=request.allow.graphql"}, `node I_nope at addComment.input.subjectId did not resolve; Could not resolve to a node with the global id of 'I_nope'`},
		{"an ID with no repository", `{"query":"mutation{addComment(input:{subjectId:\"U_user\",body:\"hi\"}){clientMutationId}}"}`,
			403, []string{"check=request.allow.graphql"}, `node U_user at addComment.input.subjectId is a User, which is in no repository`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, up := gqlRig(t, allow)
			resp, body := ghGraphQL(r, js, c.body)
			wantStatus(t, resp, c.status)
			wantAudit(t, r.lastAudit(), c.audit...)
			if c.bodyHas != "" && !strings.Contains(body, c.bodyHas) {
				t.Errorf("body lacks %q:\n%s", c.bodyHas, body)
			}
			if c.status == 403 {
				if n := countGuest(t, up); n != 0 {
					t.Fatalf("the upstream saw %d guest requests", n)
				}
			}
		})
	}
}

// A glob matches the repository's current name, as GitHub returns it.
func TestGraphQLReposGlobLimitsMutations(t *testing.T) {
	allow := &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"addComment"}, Repos: []string{"brig-sh/*"}}}
	r, _ := gqlRig(t, allow)
	resp, _ := ghGraphQL(r, "application/json", `{"query":"mutation{addComment(input:{subjectId:\"I_here\",body:\"hi\"}){clientMutationId}}"}`)
	wantStatus(t, resp, http.StatusOK)
	resp, body := ghGraphQL(r, "application/json", `{"query":"mutation{addComment(input:{subjectId:\"I_other\",body:\"hi\"}){clientMutationId}}"}`)
	wantStatus(t, resp, http.StatusForbidden)
	if !strings.Contains(body, "is in a repository that is not in the allow list [brig-sh/*]") {
		t.Errorf("body: %s", body)
	}
}

// A lookup that fails is the upstream's failure, not a policy refusal: 502
// with the forward path's fixed body, and the cause on the audit line only.
func TestGraphQLLookupFailureIsBadGateway(t *testing.T) {
	for _, c := range []struct {
		name  string
		allow *v1alpha1.GraphQLAllow
		body  string
	}{
		{"node lookup", &v1alpha1.GraphQLAllow{Mutations: []string{"addComment"}, Repos: []string{"brig-sh/brig"}},
			`{"query":"mutation{addComment(input:{subjectId:\"I_down\",body:\"hi\"}){clientMutationId}}"}`},
		{"repository confirmation", &v1alpha1.GraphQLAllow{Queries: []string{"repository"}, Repos: []string{"down/*"}},
			`{"query":"query{repository(owner:\"down\",name:\"x\"){id}}"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, up := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: c.allow})
			resp, body := ghGraphQL(r, "application/json", c.body)
			wantStatus(t, resp, http.StatusBadGateway)
			if want := "brig broker: the upstream of route github-api could not be reached\n"; body != want {
				t.Errorf("body %q, want %q", body, want)
			}
			wantAudit(t, r.lastAudit(), "check=upstream", "cause=")
			if n := countGuest(t, up); n != 0 {
				t.Fatalf("the upstream saw %d guest requests", n)
			}
		})
	}
}

// A node lookup is kept for a while, like a repository confirmation, so a
// comment does not cost two requests each time.
func TestNodeLookupIsCached(t *testing.T) {
	r, up := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"addComment"}, Repos: []string{"brig-sh/brig"}}})
	q := `{"query":"mutation{addComment(input:{subjectId:\"PR_x\",body:\"hi\"}){clientMutationId}}"}`
	for range 3 {
		resp, _ := ghGraphQL(r, "application/json", q)
		wantStatus(t, resp, http.StatusOK)
	}
	if n, g := up.count(), countGuest(t, up); n-g != 2 || g != 3 {
		t.Fatalf("the upstream saw %d lookups and %d guest requests, want 2 (one node, one repository) and 3", n-g, g)
	}
}

// Without repos, a mutation goes upstream with no lookup.
func TestGraphQLMutationWithoutReposIsNotLookedUp(t *testing.T) {
	r, up := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"addComment"}}})
	resp, _ := ghGraphQL(r, "application/json", `{"query":"mutation{addComment(input:{subjectId:\"I_other\",body:\"hi\"}){clientMutationId}}"}`)
	wantStatus(t, resp, http.StatusOK)
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want the guest's alone", n)
	}
}

// A pull request has two repositories. A mutation such as
// updatePullRequestBranch writes to the head, so a pull request from a fork
// outside repos is refused even though its base is listed.
func TestGraphQLReposCheckBothEndsOfAPullRequest(t *testing.T) {
	q := `{"query":"mutation{updatePullRequestBranch(input:{pullRequestId:\"PR_fork\"}){clientMutationId}}"}`
	r, up := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"updatePullRequestBranch"}, Repos: []string{"brig-sh/brig"}}})
	resp, body := ghGraphQL(r, "application/json", q)
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "check=request.allow.graphql", `resolved="PR_fork=brig-sh/brig,PR_fork.head=someone/brig"`)
	if want := "node PR_fork at updatePullRequestBranch.input.pullRequestId is a pull request whose head repository is not in the allow list [brig-sh/brig]"; !strings.Contains(body, want) {
		t.Errorf("body lacks %q:\n%s", want, body)
	}
	if n := countGuest(t, up); n != 0 {
		t.Fatalf("the upstream saw %d guest requests", n)
	}

	r, _ = gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"updatePullRequestBranch"}, Repos: []string{"brig-sh/brig", "someone/*"}}})
	resp, _ = ghGraphQL(r, "application/json", q)
	wantStatus(t, resp, http.StatusOK)
}

// A ref, a commit, a review thread and a discussion comment are in a
// repository, so their IDs resolve rather than being refused as in none.
func TestGraphQLReposResolveNodesInARepository(t *testing.T) {
	allow := &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"*"}, Repos: []string{"brig-sh/brig"}}}
	for _, id := range []string{"REF_x", "C_x", "PRRT_x", "DC_x"} {
		t.Run(id, func(t *testing.T) {
			r, _ := gqlRig(t, allow)
			resp, _ := ghGraphQL(r, "application/json", `{"query":"mutation{addComment(input:{subjectId:\"`+id+`\",body:\"hi\"}){clientMutationId}}"}`)
			wantStatus(t, resp, http.StatusOK)
			wantAudit(t, r.lastAudit(), `resolved="`+id+`=brig-sh/brig"`)
		})
	}
}

// The guest learns that a node is outside repos, not which repository it is
// in: otherwise any node ID it holds would name its repository. The audit
// line keeps the name.
func TestGraphQLRefusalKeepsTheRepositoryFromTheGuest(t *testing.T) {
	r, _ := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Mutations: []string{"addComment"}, Repos: []string{"brig-sh/brig"}}})
	resp, body := ghGraphQL(r, "application/json", `{"query":"mutation{addComment(input:{subjectId:\"I_other\",body:\"hi\"}){clientMutationId}}"}`)
	wantStatus(t, resp, http.StatusForbidden)
	if strings.Contains(body, "NOFireAI/engineering") {
		t.Errorf("the guest was told the repository:\n%s", body)
	}
	wantAudit(t, r.lastAudit(), `resolved="I_other=NOFireAI/engineering"`)
}
