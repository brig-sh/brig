package broker

import (
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
)

// guestKey is the API key a guest sends on a passthrough route. The broker
// holds no credential for the route, so this is the one that goes upstream.
const guestKey = "sk-ant-api03-guest-own-key-0123456789"

// passthroughRoute is the anthropic-api-key preset as a passthrough route to
// upstream, with req as its request policy.
func passthroughRoute(t *testing.T, upstream string, req *v1alpha1.RequestPolicy) Route {
	t.Helper()
	r, err := policy.ExpandRoute(v1alpha1.Route{
		Name:        "anthropic",
		Preset:      v1alpha1.PresetAnthropicAPIKey,
		Passthrough: true,
		Request:     req,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Upstream = upstream
	return Route{Policy: "test-anthropic", Route: r}
}

// arrivedAs binds every request to sandbox, as the address listener does for
// the peer a sandbox registered.
func arrivedAs(sandbox string) rigOption {
	return func(c *Config) {
		c.ArrivalCheck = func(want, _ string) (string, bool) { return sandbox, want == sandbox }
	}
}

// guestHeader is what a client in the guest sends with its own key.
func guestHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "X-Api-Key": {guestKey}}
}

func TestPassthroughForwardsTheGuestCredential(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{passthroughRoute(t, up.URL, nil)}, arrivedAs("brig-a"))
	h := guestHeader()
	h.Set("Authorization", "Bearer "+guestKey)
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if v := got.header.Get("X-Api-Key"); v != guestKey {
		t.Errorf("upstream X-Api-Key = %q, want the guest's key", v)
	}
	if v := got.header.Get("Authorization"); v != "Bearer "+guestKey {
		t.Errorf("upstream Authorization = %q, want the guest's", v)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Errorf("Resolve ran %d times for a passthrough route", res)
	}
	line := r.lastAudit()
	wantAudit(t, line, "sandbox=brig-a", "route=anthropic", "status=200")
	if strings.Contains(r.audit.String(), guestKey) {
		t.Errorf("the audit line carries the guest's key:\n%s", r.audit.String())
	}
}

// Without a session token, the arrival is all that ties a request to a
// sandbox: one on the loopback port names none, and one from another
// sandbox's address names that sandbox.
func TestPassthroughRefusesAnUnboundArrival(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []rigOption
	}{
		{"loopback", nil},
		{"another sandbox", []rigOption{arrivedAs("brig-b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, nil)
			r := newRig(t, up, []Route{passthroughRoute(t, up.URL, nil)}, tc.opts...)
			resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", guestHeader(), userTurn("claude-sonnet-4-5", `"hi"`))
			wantStatus(t, resp, http.StatusNotFound)
			wantAudit(t, r.lastAudit(), "check=arrival")
			if n := up.count(); n != 0 {
				t.Errorf("the upstream saw %d requests", n)
			}
		})
	}
}

func TestPassthroughKeepsTheRequestRules(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{passthroughRoute(t, up.URL, &v1alpha1.RequestPolicy{
		Allow: &v1alpha1.RequestAllow{Methods: []string{"POST"}, Paths: []string{"/v1/messages"}},
		Body:  &v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5*"}},
	})}, arrivedAs("brig-a"))

	resp, _ := r.do("GET", "/brig-a/r/anthropic/v1/models", guestHeader(), "")
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "check=request.allow.")

	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages", guestHeader(), userTurn("claude-opus-4-1", `"hi"`))
	wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
	wantAudit(t, r.lastAudit(), "check=body", `reason="model=claude-opus-4-1"`)

	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d refused requests", n)
	}
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", guestHeader(), userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
}

func TestPassthroughRewritesADeniedToolCall(t *testing.T) {
	up := newUpstream(t, answerJSON(http.StatusOK, answer("tool_use", textAnswerBlock, bashBlock)))
	r := newRig(t, up, []Route{passthroughRoute(t, up.URL, &v1alpha1.RequestPolicy{
		Body: &v1alpha1.RequestBody{ToolUse: bashRules(v1alpha1.ToolUseRefuse)},
	})}, arrivedAs("brig-a"))
	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages", guestHeader(), userTurn("claude-sonnet-4-5", `"clean up"`))
	m := readAnswer(t, resp, body)
	if len(m.Content) != 2 || m.Content[1].Text != refusalText("Bash") {
		t.Errorf("content %+v, want the text and the refusal", m.Content)
	}
	if strings.Contains(body, "rm -rf") {
		t.Errorf("the Bash call reached the guest:\n%s", body)
	}
}

// A passthrough route has no carrier, so the session token of another route
// of the same sandbox is refused wherever it is, and its credential stays in
// the broker.
func TestPassthroughRefusesASessionTokenOfAnotherRoute(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{
		passthroughRoute(t, up.URL, nil),
		customRoute(t, "inventory", up.URL, nil),
	}, arrivedAs("brig-a"))
	tok := r.mint("brig-a", "inventory")
	for _, h := range []http.Header{
		{"X-Api-Key": {tok}},
		{"Authorization": {"Bearer " + tok}},
	} {
		resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
		wantStatus(t, resp, http.StatusForbidden)
		wantAudit(t, r.lastAudit(), "check=placeholder")
	}
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Errorf("Resolve ran %d times", res)
	}
}

// The headers that would get around the request rules, or tell the upstream
// where the request came from, are dropped on a passthrough route too.
func TestPassthroughDropsTheGuestRoutingHeaders(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{passthroughRoute(t, up.URL, nil)}, arrivedAs("brig-a"))
	h := guestHeader()
	h.Set("X-Forwarded-For", "10.0.0.9")
	h.Set("X-Http-Method-Override", "DELETE")
	h.Set("X-Brig-Sandbox", "brig-b")
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	for _, name := range []string{"X-Forwarded-For", "X-Http-Method-Override", "X-Brig-Sandbox"} {
		if v := got.header.Get(name); v != "" {
			t.Errorf("upstream got %s: %q", name, v)
		}
	}
}

// The preset's Anthropic-Beta list still applies on a passthrough route. The
// mcp-client beta would let the upstream call MCP servers that the body named,
// on hosts the sandbox's egress refuses, past request.body.tools.
func TestPassthroughKeepsTheBetaAllowList(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{passthroughRoute(t, up.URL, &v1alpha1.RequestPolicy{
		Body: &v1alpha1.RequestBody{Tools: &v1alpha1.ToolRules{Allow: []string{"Read"}}},
	})}, arrivedAs("brig-a"))
	h := guestHeader()
	h.Set("Anthropic-Beta", "mcp-client-2025-04-04,claude-code-20250219")
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if v := got.header.Values("Anthropic-Beta"); len(v) != 1 || v[0] != "claude-code-20250219" {
		t.Errorf("upstream Anthropic-Beta = %q, want only the reviewed value", v)
	}
	if v := got.header.Get("X-Api-Key"); v != guestKey {
		t.Errorf("upstream X-Api-Key = %q, want the guest's key", v)
	}
	wantAudit(t, r.lastAudit(), "dropped.anthropic-beta=1")
}

// On the claude-code preset the guest's own OAuth token is its Authorization,
// so a passthrough route keeps that header while it filters Anthropic-Beta.
func TestClaudeCodePassthroughKeepsAuthorization(t *testing.T) {
	up := newUpstream(t, nil)
	rt, err := policy.ExpandRoute(v1alpha1.Route{Name: "anthropic", Preset: v1alpha1.PresetClaudeCode, Passthrough: true})
	if err != nil {
		t.Fatal(err)
	}
	rt.Upstream = up.URL
	r := newRig(t, up, []Route{{Policy: "test-claude-code", Route: rt}}, arrivedAs("brig-a"))
	h := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer " + guestKey},
		"Anthropic-Beta": {"oauth-2025-04-20,mcp-client-2025-04-04"}}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if v := got.header.Get("Authorization"); v != "Bearer "+guestKey {
		t.Errorf("upstream Authorization = %q, want the guest's", v)
	}
	if v := got.header.Values("Anthropic-Beta"); len(v) != 1 || v[0] != "oauth-2025-04-20" {
		t.Errorf("upstream Anthropic-Beta = %q, want only the reviewed value", v)
	}
}

// A route that is not passthrough still never forwards the guest's
// Authorization, which carries its session token.
func TestCredentialRouteStillDropsAuthorization(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{
		presetRoute(t, v1alpha1.PresetAnthropicAPIKey, "anthropic", "anthropic-api-key", up.URL, nil),
	})
	h := http.Header{"X-Api-Key": {r.mint("brig-a", "anthropic")}, "Authorization": {"Basic Z3Vlc3Q6b3du"}}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if v := got.header.Get("Authorization"); v != "" {
		t.Errorf("upstream Authorization = %q on a credential route", v)
	}
	if v := got.header.Get("X-Api-Key"); v != fakeAPIKey {
		t.Errorf("upstream X-Api-Key = %q, want the broker's credential", v)
	}
}
