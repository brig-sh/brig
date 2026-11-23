package broker

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// gitPath is a request the github-token route of claudeRoutes serves.
const gitPath = "/brig-sh/brig.git/info/refs?service=git-upload-pack"

// gitHeader carries tok the way git does, as the basic password.
func gitHeader(tok string) http.Header {
	req := http.Request{Header: http.Header{}}
	req.SetBasicAuth("x-access-token", tok)
	return req.Header
}

// A route removed by SetRoutes answers 404 from the next request on, with the
// token the sandbox already holds, and no token is minted for it again.
func TestSetRoutesRemovedRouteIsNotServed(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	gh := r.mint("brig-a", "github")
	claude := r.mint("brig-a", "anthropic")

	if err := r.h.SetRoutes("brig-a", claudeRoutes(t, up)[:1]); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.do("GET", "/brig-a/r/github"+gitPath, gitHeader(gh), "")
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "status=404", "check=route")
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests for a removed route", n)
	}
	if _, err := r.h.Mint("brig-a", "", "github", DefaultTokenTTL); err == nil {
		t.Error("a token was minted for a removed route")
	}
	// The route that stays keeps its key, so the token minted before works.
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(claude), userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	// The other sandbox with the same routes is untouched.
	resp, _ = r.do("GET", "/brig-b/r/github"+gitPath, gitHeader(r.mint("brig-b", "github")), "")
	wantStatus(t, resp, http.StatusOK)
}

// A rule tightened by SetRoutes applies to the next request of the sandbox,
// under the token minted before it.
func TestSetRoutesTightenedRuleAppliesToTheNextRequest(t *testing.T) {
	up := newUpstream(t, nil)
	loose := presetRouteWith(t, v1alpha1.PresetClaudeCode, "anthropic", "claude-credentials", up.URL,
		&v1alpha1.RequestPolicy{Body: &v1alpha1.RequestBody{Models: []string{"claude-*"}}})
	r := newRig(t, up, []Route{loose})
	tok := r.mint("brig-a", "anthropic")
	opus := userTurn("claude-opus-4-1", `"hi"`)
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), opus)
	wantStatus(t, resp, http.StatusOK)

	tight := presetRouteWith(t, v1alpha1.PresetClaudeCode, "anthropic", "claude-credentials", up.URL,
		&v1alpha1.RequestPolicy{Body: &v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5"}}})
	if err := r.h.SetRoutes("brig-a", []Route{tight}); err != nil {
		t.Fatal(err)
	}
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), opus)
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "status=403", "check=body", "rule=spec.routes[anthropic].request.body.models")
	if n := up.count(); n != 1 {
		t.Errorf("the upstream saw %d requests, want the one before the update", n)
	}
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
}

// A route that stays under another credential injects that one from the
// next request on.
func TestSetRoutesChangedCredentialAppliesToTheNextRequest(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up)[1:])
	tok := r.mint("brig-a", "github")
	r.creds.set("gh-token-2", []byte("fake-second-github-token"))
	moved := presetRoute(t, v1alpha1.PresetGitHubToken, "github", "gh-token-2", up.URL,
		&v1alpha1.RequestAllow{Repos: []string{"brig-sh/*"}})
	if err := r.h.SetRoutes("brig-a", []Route{moved}); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.do("GET", "/brig-a/r/github"+gitPath, gitHeader(tok), "")
	wantStatus(t, resp, http.StatusOK)
	auth := up.last(t).header.Get("Authorization")
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:fake-second-github-token")); auth != want {
		t.Errorf("the upstream got Authorization %q, want the second secret", auth)
	}
}

// SetRoutes refuses a table the sandbox cannot serve and leaves the one it
// has, and CheckRoutes refuses it without changing anything.
func TestSetRoutesRefusalKeepsTheOldTable(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "github")
	dup := append(claudeRoutes(t, up), claudeRoutes(t, up)[1])
	// The sandbox was registered with no route mode besides endpoint.
	splice := claudeRoutes(t, up)[:1]
	splice[0].Route.Mode = v1alpha1.RouteMode("splice")
	for name, routes := range map[string][]Route{"duplicate": dup, "unserved mode": splice} {
		if err := r.h.CheckRoutes("brig-a", routes); err == nil {
			t.Errorf("%s: CheckRoutes took it", name)
		}
		if err := r.h.SetRoutes("brig-a", routes); err == nil {
			t.Errorf("%s: SetRoutes took it", name)
		}
	}
	resp, _ := r.do("GET", "/brig-a/r/github"+gitPath, gitHeader(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if err := r.h.SetRoutes("brig-none", nil); err == nil {
		t.Error("SetRoutes took a sandbox nobody registered")
	}
}

// Requests in flight while the table swaps each run under one table. The two
// tables refuse the request for different reasons: A allows its model and
// denies its tool, B allows its tool and denies its model. A request that
// read the model rule of one table and the tool rule of the other would
// reach the upstream. Run with -race.
func TestSetRoutesSwapIsNeverAMix(t *testing.T) {
	up := newUpstream(t, nil)
	route := func(models []string, deny []string) Route {
		return presetRouteWith(t, v1alpha1.PresetClaudeCode, "anthropic", "claude-credentials", up.URL,
			&v1alpha1.RequestPolicy{Body: &v1alpha1.RequestBody{Models: models,
				Tools: &v1alpha1.ToolRules{Deny: deny}}})
	}
	a := []Route{route([]string{"claude-opus-4-1"}, []string{"Bash"})}
	b := []Route{route([]string{"claude-sonnet-4-5"}, []string{"Read"})}
	r := newRig(t, up, a)
	tok := r.mint("brig-a", "anthropic")
	body := `{"model":"claude-opus-4-1","max_tokens":16,"tools":[{"name":"Bash","input_schema":{}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`

	const n = 200
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range n / 8 {
				req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", strings.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+tok)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				codes <- resp.StatusCode
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 100 {
			next := a
			if i%2 == 0 {
				next = b
			}
			if err := r.h.SetRoutes("brig-a", next); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusForbidden {
			t.Errorf("a request during the swap got %d, want 403 from either table", code)
		}
	}
	if c := up.count(); c != 0 {
		t.Errorf("the upstream saw %d requests that one table or the other refuses", c)
	}
}
