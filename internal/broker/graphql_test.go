package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// ghGraphQL sends a GraphQL body to the github-api route as gh does.
func ghGraphQL(r *rig, ctype, body string) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest("POST", r.srv.URL+"/api/graphql", strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = "github-api.brig.internal"
	req.Header.Set("Authorization", "token "+r.mint("brig-a", "github-api"))
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r.lines++
	r.waitAudit(r.lines)
	return resp, string(b)
}

// fakeGitHub answers the broker's lookups as GitHub does. With
// followRenames: false, atom/electron is null because it was moved,
// NOFireAI/engineering is R_2, down/* fails, and any other repository is R_1,
// brig-sh/brig. fakeNodes answers nodes(ids:). Any other request gets "ok".
func fakeGitHub(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), "nodes(ids:") {
		fakeNodes(w, b)
		return
	}
	if !strings.Contains(string(b), "followRenames:false") {
		_, _ = io.WriteString(w, "ok")
		return
	}
	var req struct{ Variables map[string]string }
	_ = json.Unmarshal(b, &req)
	switch {
	case strings.EqualFold(req.Variables["o"], "atom"):
		_, _ = io.WriteString(w, `{"data":{"repository":null}}`)
	case strings.EqualFold(req.Variables["o"], "NOFireAI"):
		_, _ = io.WriteString(w, `{"data":{"repository":{"id":"R_2"}}}`)
	case strings.EqualFold(req.Variables["o"], "down"):
		http.Error(w, "unavailable", http.StatusInternalServerError)
	default:
		_, _ = io.WriteString(w, `{"data":{"repository":{"id":"R_1"}}}`)
	}
}

// fakeNodes answers nodes(ids:): PR_x, I_here and R_1 are in brig-sh/brig
// (R_1), I_other is in NOFireAI/engineering (R_2), U_user is a user, I_down
// fails the lookup, and any other ID resolves to nothing. PR_fork is a pull
// request into brig-sh/brig from someone/brig (R_3). REF_x, C_x, PRRT_x and
// DC_x are a ref, a commit, a review thread and a discussion comment in
// brig-sh/brig. Like GitHub, it answers a type's fields only when the query
// has a fragment on that type.
func fakeNodes(w http.ResponseWriter, b []byte) {
	var req struct {
		Query     string
		Variables struct{ IDs []string }
	}
	_ = json.Unmarshal(b, &req)
	repo := func(id, name string) string { return `{"id":"` + id + `","nameWithOwner":"` + name + `"}` }
	in := func(id, name string) string {
		return `{"__typename":"Issue","repository":` + repo(id, name) + `}`
	}
	on := func(typ, fields string) string {
		if !strings.Contains(req.Query, "...on "+typ+"{") {
			return `{"__typename":"` + typ + `"}`
		}
		return `{"__typename":"` + typ + `",` + fields + `}`
	}
	here := `"repository":` + repo("R_1", "brig-sh/brig")
	var nodes, errs []string
	for i, id := range req.Variables.IDs {
		switch id {
		case "I_down":
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		case "PR_x", "I_here":
			nodes = append(nodes, in("R_1", "brig-sh/brig"))
		case "R_1":
			nodes = append(nodes, `{"__typename":"Repository","id":"R_1","nameWithOwner":"brig-sh/brig"}`)
		case "I_other":
			nodes = append(nodes, in("R_2", "NOFireAI/engineering"))
		case "U_user":
			nodes = append(nodes, `{"__typename":"User"}`)
		case "PR_fork":
			n := on("PullRequest", here+`,"headRepository":`+repo("R_3", "someone/brig"))
			if n == `{"__typename":"PullRequest"}` {
				n = `{"__typename":"PullRequest",` + here + `}`
			}
			nodes = append(nodes, n)
		case "REF_x":
			nodes = append(nodes, on("Ref", here))
		case "C_x":
			nodes = append(nodes, on("Commit", here))
		case "PRRT_x":
			nodes = append(nodes, on("PullRequestReviewThread", here))
		case "DC_x":
			nodes = append(nodes, on("DiscussionComment", `"discussion":{`+here+`}`))
		default:
			nodes = append(nodes, "null")
			errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["nodes",%d],"message":"Could not resolve to a node with the global id of '%s'"}`, i, id))
		}
	}
	out := `{"data":{"nodes":[` + strings.Join(nodes, ",") + `]}`
	if errs != nil {
		out += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	_, _ = io.WriteString(w, out+"}")
}

// gqlRig is ghAPIRig with fakeGitHub upstream.
func gqlRig(t *testing.T, allow *v1alpha1.RequestAllow) (*rig, *upstream) {
	t.Helper()
	up := newUpstream(t, fakeGitHub)
	return newRig(t, up, []Route{presetRoute(t, v1alpha1.PresetGitHubAPI, "github-api", "gh-token", up.URL, allow)}, arrivesOn("brig-a")), up
}

// countGuest returns how many requests the upstream saw that were not the
// broker's own lookups.
func countGuest(t *testing.T, up *upstream) int {
	t.Helper()
	up.mu.Lock()
	defer up.mu.Unlock()
	n := 0
	for _, s := range up.reqs {
		if !strings.Contains(s.body, "followRenames:false") && !strings.Contains(s.body, "nodes(ids:") {
			n++
		}
	}
	return n
}

func TestGraphQLPolicyOnTheGitHubAPIRoute(t *testing.T) {
	allow := &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{
		Queries:   []string{"repository"},
		Mutations: []string{"addComment"},
		Repos:     []string{"brig-sh/brig", "atom/*"},
	}}
	const js = "application/json; charset=utf-8"
	for _, c := range []struct {
		name, ctype, body string
		status            int
	}{
		{"pr view", js, `{"query":"query($o:String!,$n:String!){repository(owner:$o,name:$n){pullRequest(number:1){title}}}","variables":{"o":"brig-sh","n":"brig"}}`, 200},
		{"comment", js, `{"query":"mutation($i:AddCommentInput!){addComment(input:$i){clientMutationId}}","variables":{"i":{"subjectId":"PR_x","body":"hi"}}}`, 200},
		{"merge", js, `{"query":"mutation{mergePullRequest(input:{pullRequestId:\"PR_x\"}){clientMutationId}}"}`, 403},
		{"other repo", js, `{"query":"query{repository(owner:\"NOFireAI\",name:\"engineering\"){id}}"}`, 403},
		{"form body", "application/x-www-form-urlencoded", `query=query{viewer{login}}`, 403},
		{"moved repository", js, `{"query":"query{repository(owner:\"atom\",name:\"electron\"){id}}"}`, 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, up := gqlRig(t, allow)
			resp, body := ghGraphQL(r, c.ctype, c.body)
			wantStatus(t, resp, c.status)
			t.Logf("%d %s", resp.StatusCode, strings.TrimSpace(body))
			if c.status == 403 {
				wantAudit(t, r.lastAudit(), "check=request.allow.graphql")
				if n := countGuest(t, up); n != 0 {
					t.Fatalf("the upstream saw %d guest requests", n)
				}
				return
			}
			if got := up.last(t).body; got != c.body {
				t.Errorf("the upstream saw body %q, want it unchanged", got)
			}
		})
	}
}

// A confirmation is kept for a while, so gh's queries do not each cost one.
func TestRepoConfirmationIsCached(t *testing.T) {
	r, up := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Queries: []string{"repository"}, Repos: []string{"brig-sh/brig"}}})
	q := `{"query":"query{repository(owner:\"brig-sh\",name:\"brig\"){id}}"}`
	for range 3 {
		resp, _ := ghGraphQL(r, "application/json", q)
		wantStatus(t, resp, http.StatusOK)
	}
	if n, g := up.count(), countGuest(t, up); n-g != 1 {
		t.Fatalf("the upstream saw %d confirmations, want 1", n-g)
	}
}

// REST goes on as before; request.allow.graphql reads /api/graphql alone.
func TestGraphQLPolicyLeavesRESTAlone(t *testing.T) {
	r, _ := gqlRig(t, &v1alpha1.RequestAllow{GraphQL: &v1alpha1.GraphQLAllow{Queries: []string{"viewer"}}})
	resp, _ := ghAPIDo(r, "GET", "/api/v3/user")
	wantStatus(t, resp, http.StatusOK)
}
