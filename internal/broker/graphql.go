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
	"path"
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

// checkGraphQL checks a request to /graphql. It returns what the broker must
// look up under repos, or why the request is refused. It reads only a whole
// JSON body in a POST, so GitHub runs the request the broker checked.
func checkGraphQL(r *http.Request, held *heldBody, g *v1alpha1.GraphQLAllow) (graphql.Result, string) {
	if r.Method != http.MethodPost {
		return graphql.Result{}, "GraphQL request uses " + r.Method + "; use POST"
	}
	if r.URL.RawQuery != "" {
		return graphql.Result{}, "GraphQL request has a query string; send the query in the JSON body"
	}
	// Header.Get reads the first line and every line goes upstream, so a
	// second line could make GitHub read the body another way.
	if len(r.Header.Values("Content-Type")) > 1 || len(r.Header.Values("Content-Encoding")) > 1 {
		return graphql.Result{}, "GraphQL request repeats Content-Type or Content-Encoding; send each header once"
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		return graphql.Result{}, "GraphQL request body is compressed; send it uncompressed"
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return graphql.Result{}, "GraphQL request body is not JSON; send it with Content-Type: application/json"
	}
	if held == nil {
		return graphql.Result{}, "GraphQL request has no body; send the query in a JSON body"
	}
	body, ok := held.complete()
	if !ok {
		return graphql.Result{}, "GraphQL request body exceeds 8 MiB; send a smaller query"
	}
	req, err := graphql.ReadRequest(body)
	if err != nil {
		return graphql.Result{}, err.Error()
	}
	res, err := graphql.Check(req, graphql.Rules{Queries: g.Queries, Mutations: g.Mutations, Repos: g.Repos})
	if err != nil {
		return graphql.Result{}, err.Error()
	}
	if len(res.Nodes) > maxNodes {
		return graphql.Result{}, fmt.Sprintf("mutation names %d node IDs; send at most %d in one request", len(res.Nodes), maxNodes)
	}
	return res, ""
}

// repoConfirmed is how long the broker keeps a repository confirmation or a
// node's repository. A node can move: an issue transferred to another
// repository is seen within this time.
const repoConfirmed = 5 * time.Minute

// maxNodes is the most node IDs GitHub's nodes(ids:) takes at once.
const maxNodes = 100

// repoSeen is a repository the broker looked up, and when.
type repoSeen struct {
	id   string // "" when it does not exist under that name
	name string // owner/name as GitHub answers it, for a node
	// headID and headName are a pull request's head repository, which
	// updatePullRequestBranch writes to. "" for any other node.
	headID, headName string
	at               time.Time
}

// errLookup is a lookup the broker could not complete. The guest gets the
// forward path's 502, and the cause goes on the audit line.
type errLookup struct{ err error }

func (e errLookup) Error() string { return e.err.Error() }

// cached returns what m holds for key, if it is recent. The caller holds
// rt.repoMu.
func (h *Handler) cached(m map[string]repoSeen, key string) (repoSeen, bool) {
	s, ok := m[key]
	return s, ok && h.now().Sub(s.at) < repoConfirmed
}

// repoIDOf returns the ID of the repository owner/name, or "" when no
// repository has exactly that name. It asks with followRenames: false, because
// GitHub otherwise resolves an old name to the renamed or transferred
// repository.
func (h *Handler) repoIDOf(r *http.Request, rt *route, upstream *url.URL, value string, repo graphql.Repo) (string, error) {
	key := strings.ToLower(repo.String())
	rt.repoMu.Lock()
	s, ok := h.cached(rt.repoOK, key)
	rt.repoMu.Unlock()
	if ok {
		return s.id, nil
	}
	var out struct {
		Data struct {
			Repository *struct{ ID string } `json:"repository"`
		} `json:"data"`
	}
	err := h.ask(r.Context(), upstream, value,
		"query($o:String!,$n:String!){repository(owner:$o,name:$n,followRenames:false){id}}",
		map[string]any{"o": repo.Owner, "n": repo.Name}, &out)
	if err != nil {
		return "", errLookup{fmt.Errorf("cannot confirm that repo %s exists: %w", repo, err)}
	}
	id := ""
	if out.Data.Repository != nil {
		id = out.Data.Repository.ID
	}
	rt.repoMu.Lock()
	if rt.repoOK == nil {
		rt.repoOK = map[string]repoSeen{}
	}
	rt.repoOK[key] = repoSeen{id: id, at: h.now()}
	rt.repoMu.Unlock()
	return id, nil
}

// confirmRepos confirms that each repository a query names exists under that
// name. It returns why the request is refused, or an errLookup.
func (h *Handler) confirmRepos(r *http.Request, rt *route, upstream *url.URL, value string, repos []graphql.Repo) (string, error) {
	for _, repo := range repos {
		id, err := h.repoIDOf(r, rt, upstream, value, repo)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "repo " + repo.String() + " not found under that name; if it was renamed or moved, use its current name", nil
		}
	}
	return "", nil
}

// confirmNodes finds the repository of each node ID a mutation names and
// refuses the request unless every one is in repos. An exact entry matches by
// repository ID, so a renamed repository matches only its current name. A
// glob matches the current owner/name. It returns the IDs and their
// repositories for the audit line, why the request is refused, or an
// errLookup.
func (h *Handler) confirmNodes(r *http.Request, rt *route, upstream *url.URL, value string, nodes []graphql.Node, repos []string) (string, string, error) {
	if len(nodes) == 0 {
		return "", "", nil
	}
	found, reason, err := h.nodeRepos(r, rt, upstream, value, nodes)
	if err != nil {
		return "", "", err
	}
	var resolved []string
	for _, n := range nodes {
		if s, ok := found[n.ID]; ok && s.id != "" {
			resolved = append(resolved, n.ID+"="+s.name)
			if s.headID != "" && s.headID != s.id {
				resolved = append(resolved, n.ID+".head="+s.headName)
			}
		}
	}
	audit := strings.Join(resolved, ",")
	if reason != "" {
		return audit, reason, nil
	}
	// The reason does not name the repository: the guest would learn the
	// repository of any node ID it holds. The audit line has it.
	allow := "[" + strings.Join(repos, ", ") + "]"
	for _, n := range nodes {
		s := found[n.ID]
		ok, err := h.repoListed(r, rt, upstream, value, s, repos)
		if err != nil {
			return audit, "", err
		}
		if !ok {
			return audit, fmt.Sprintf("node %s at %s is in a repository that is not in the allow list %s", n.ID, n.Path, allow), nil
		}
		if s.headID == "" || s.headID == s.id {
			continue
		}
		ok, err = h.repoListed(r, rt, upstream, value, repoSeen{id: s.headID, name: s.headName}, repos)
		if err != nil {
			return audit, "", err
		}
		if !ok {
			return audit, fmt.Sprintf("node %s at %s is a pull request whose head repository is not in the allow list %s", n.ID, n.Path, allow), nil
		}
	}
	return audit, "", nil
}

// repoListed reports whether the repository s matches an entry of repos.
func (h *Handler) repoListed(r *http.Request, rt *route, upstream *url.URL, value string, s repoSeen, repos []string) (bool, error) {
	for _, g := range repos {
		if strings.ContainsAny(g, "*?[\\") {
			if ok, err := path.Match(asciiLower(g), asciiLower(s.name)); err == nil && ok {
				return true, nil
			}
			continue
		}
		owner, name, _ := strings.Cut(g, "/")
		id, err := h.repoIDOf(r, rt, upstream, value, graphql.Repo{Owner: owner, Name: name})
		if err != nil {
			return false, err
		}
		if id != "" && id == s.id {
			return true, nil
		}
	}
	return false, nil
}

// nodeRepos returns the repository of each node ID, from the cache or one
// nodes(ids:) query. It returns why the request is refused when an ID does not
// resolve or belongs to no repository.
func (h *Handler) nodeRepos(r *http.Request, rt *route, upstream *url.URL, value string, nodes []graphql.Node) (map[string]repoSeen, string, error) {
	found := map[string]repoSeen{}
	var ask []string
	rt.repoMu.Lock()
	for _, n := range nodes {
		if s, ok := h.cached(rt.nodeOK, n.ID); ok {
			found[n.ID] = s
		} else if _, dup := found[n.ID]; !dup {
			found[n.ID] = repoSeen{}
			ask = append(ask, n.ID)
		}
	}
	rt.repoMu.Unlock()
	if len(ask) == 0 {
		return found, "", nil
	}
	type repoRef struct{ ID, NameWithOwner string }
	var out struct {
		Data struct {
			Nodes []*struct {
				Typename       string   `json:"__typename"`
				ID             string   `json:"id"`
				NameWithOwner  string   `json:"nameWithOwner"`
				Repository     *repoRef `json:"repository"`
				HeadRepository *repoRef `json:"headRepository"`
				Discussion     *struct {
					Repository *repoRef `json:"repository"`
				} `json:"discussion"`
			} `json:"nodes"`
		} `json:"data"`
		Errors []struct {
			Path    []any  `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	// Ref, Commit, PullRequestReviewThread and DiscussionComment are in a
	// repository but are not RepositoryNodes.
	const rf = "{id nameWithOwner}"
	err := h.ask(r.Context(), upstream, value,
		"query($ids:[ID!]!){nodes(ids:$ids){__typename ...on Repository"+rf+
			" ...on RepositoryNode{repository"+rf+"} ...on Label{repository"+rf+"}"+
			" ...on Milestone{repository"+rf+"} ...on Ref{repository"+rf+"}"+
			" ...on Commit{repository"+rf+"} ...on PullRequestReviewThread{repository"+rf+"}"+
			" ...on DiscussionComment{discussion{repository"+rf+"}}"+
			" ...on PullRequest{headRepository"+rf+"}}}",
		map[string]any{"ids": ask}, &out)
	if err != nil {
		return nil, "", errLookup{fmt.Errorf("cannot look up the repository of %s: %w", strings.Join(ask, ", "), err)}
	}
	if len(out.Data.Nodes) != len(ask) {
		return nil, "", errLookup{fmt.Errorf("upstream answered %d nodes for %d IDs", len(out.Data.Nodes), len(ask))}
	}
	why := map[int]string{}
	for _, e := range out.Errors {
		if len(e.Path) == 2 {
			if i, ok := e.Path[1].(float64); ok {
				why[int(i)] = e.Message
			}
		}
	}
	at := h.now()
	reason := ""
	rt.repoMu.Lock()
	if rt.nodeOK == nil {
		rt.nodeOK = map[string]repoSeen{}
	}
	for i, id := range ask {
		n := out.Data.Nodes[i]
		var s repoSeen
		switch {
		case n == nil:
			if reason == "" {
				reason = "node " + id + " " + pathOf(nodes, id) + " did not resolve"
				if why[i] != "" {
					reason += "; " + why[i]
				}
			}
			continue
		case n.Typename == "Repository" && n.ID != "":
			s = repoSeen{id: n.ID, name: n.NameWithOwner, at: at}
		case n.Repository != nil && n.Repository.ID != "":
			s = repoSeen{id: n.Repository.ID, name: n.Repository.NameWithOwner, at: at}
			// A pull request whose head repository was deleted, or that
			// the credential cannot see, has none to write to.
			if h := n.HeadRepository; h != nil && h.ID != "" {
				s.headID, s.headName = h.ID, h.NameWithOwner
			}
		case n.Discussion != nil && n.Discussion.Repository != nil && n.Discussion.Repository.ID != "":
			s = repoSeen{id: n.Discussion.Repository.ID, name: n.Discussion.Repository.NameWithOwner, at: at}
		default:
			if reason == "" {
				reason = "node " + id + " " + pathOf(nodes, id) + " is a " + n.Typename + ", which is in no repository"
			}
			continue
		}
		found[id] = s
		rt.nodeOK[id] = s
	}
	rt.repoMu.Unlock()
	return found, reason, nil
}

// pathOf returns "at <path>" for the first node with that ID.
func pathOf(nodes []graphql.Node, id string) string {
	for _, n := range nodes {
		if n.ID == id {
			return "at " + n.Path
		}
	}
	return ""
}

// ask sends a GraphQL query upstream with the route's credential and decodes
// the answer into out.
func (h *Handler) ask(ctx context.Context, upstream *url.URL, value, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	u := *upstream
	u.Path = "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("upstream response is not GraphQL JSON")
	}
	return nil
}

// asciiLower folds ASCII letters alone, as repos matching does in queries.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
