package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/graphql"
)

// graphQLRules returns the route's request.allow.graphql, or nil.
func graphQLRules(rt *route) *v1alpha1.GraphQLAllow {
	if rt.spec.Request == nil || rt.spec.Request.Allow == nil {
		return nil
	}
	return rt.spec.Request.Allow.GraphQL
}

// graphQLEndpoint reports whether the request is for /api/graphql, or why it
// is refused. Variants of that path are refused, because GitHub routes
// /GraphQL elsewhere. Other paths are REST and read-only.
func graphQLEndpoint(r *http.Request, p routePath) (bool, string) {
	out, ok := githubAPIPath(p)
	if !ok {
		return false, "" // refused later as github-api.path
	}
	if out.rest == "/graphql" {
		return true, ""
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(out.rest, "/"), "/")
	if strings.EqualFold(first, "graphql") {
		return false, "unknown GraphQL path; send GraphQL to /api/graphql"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false, "REST is read-only on this route; use GET or HEAD, or send GraphQL to /api/graphql"
	}
	return false, ""
}

// checkGraphQL checks a request to /graphql. It returns the repositories the
// query names at its root, or why the request is refused. It reads only a
// whole JSON body in a POST, so GitHub runs the request the broker checked.
func checkGraphQL(r *http.Request, held *heldBody, g *v1alpha1.GraphQLAllow) ([]graphql.Repo, string) {
	if r.Method != http.MethodPost {
		return nil, "GraphQL request uses " + r.Method + "; use POST"
	}
	if r.URL.RawQuery != "" {
		return nil, "GraphQL request has a query string; send the query in the JSON body"
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		return nil, "GraphQL request body is compressed; send it uncompressed"
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, "GraphQL request body is not JSON; send it with Content-Type: application/json"
	}
	if held == nil {
		return nil, "GraphQL request has no body; send the query in a JSON body"
	}
	body, ok := held.complete()
	if !ok {
		return nil, "GraphQL request body exceeds 8 MiB; send a smaller query"
	}
	req, err := graphql.ReadRequest(body)
	if err != nil {
		return nil, err.Error()
	}
	repos, err := graphql.Check(req, graphql.Rules{Queries: g.Queries, Mutations: g.Mutations, Repos: g.Repos})
	if err != nil {
		return nil, err.Error()
	}
	return repos, ""
}

// repoConfirmed is how long the broker keeps a repository confirmation.
const repoConfirmed = 5 * time.Minute

// confirmRepos confirms that each repository exists under the given name,
// with followRenames: false. Otherwise GitHub resolves an old name to the
// renamed or transferred repository. It returns why the request is refused,
// or "".
func (h *Handler) confirmRepos(r *http.Request, rt *route, upstream *url.URL, value string, repos []graphql.Repo) string {
	for _, repo := range repos {
		key := strings.ToLower(repo.String())
		now := h.now()
		rt.repoMu.Lock()
		at, ok := rt.repoOK[key]
		rt.repoMu.Unlock()
		if ok && now.Sub(at) < repoConfirmed {
			continue
		}
		exists, err := h.repoExists(r.Context(), upstream, value, repo)
		if err != nil {
			return "cannot confirm that repo " + repo.String() + " exists: " + err.Error()
		}
		if !exists {
			return "repo " + repo.String() + " not found under that name; if it was renamed or moved, use its current name"
		}
		rt.repoMu.Lock()
		if rt.repoOK == nil {
			rt.repoOK = map[string]time.Time{}
		}
		rt.repoOK[key] = now
		rt.repoMu.Unlock()
	}
	return ""
}

// repoExists asks upstream whether owner/name is a repository under exactly
// that name.
func (h *Handler) repoExists(ctx context.Context, upstream *url.URL, value string, repo graphql.Repo) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"query":     "query($o:String!,$n:String!){repository(owner:$o,name:$n,followRenames:false){id}}",
		"variables": map[string]string{"o": repo.Owner, "n": repo.Name},
	})
	if err != nil {
		return false, err
	}
	u := *upstream
	u.Path = "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("upstream returned %s", resp.Status)
	}
	var out struct {
		Data struct {
			Repository *struct{ ID string } `json:"repository"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, fmt.Errorf("upstream response is not GraphQL JSON")
	}
	return out.Data.Repository != nil && out.Data.Repository.ID != "", nil
}
