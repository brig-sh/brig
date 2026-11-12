package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// hostRouteOf is a route in mode host named name, at the test upstream.
func hostRouteOf(t *testing.T, name string, up *upstream) Route {
	t.Helper()
	r := customRoute(t, name, up.URL, nil)
	r.Route.Mode = v1alpha1.ModeHost
	return r
}

// doHost sends one request to the broker with the given Host, and waits for
// its audit line.
func (r *rig) doHost(host, method, path string, h http.Header) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = host
	for k, v := range h {
		req.Header[k] = v
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		r.t.Fatal(err)
	}
	r.lines++
	r.waitAudit(r.lines)
	return resp
}

// arrivesOn makes every connection arrive on the divert socket of sandbox.
func arrivesOn(sandbox string) rigOption {
	return func(c *Config) {
		c.ArrivalCheck = func(string, string) (string, bool) { return sandbox, true }
	}
}

func TestHostRouteOfAnotherSandboxIsNotFound(t *testing.T) {
	t.Run("route only in another sandbox", func(t *testing.T) {
		up := newUpstream(t, nil)
		r := newRig(t, up, nil, arrivesOn("brig-a"), withSandboxes(
			Sandbox{ID: "brig-a", Profile: "claude-code"},
			Sandbox{ID: "brig-b", Profile: "claude-code", Routes: []Route{hostRouteOf(t, "github-api", up)}},
		))
		resp := r.doHost("github-api.brig.internal", "GET", "/user", bearer(r.mint("brig-b", "github-api")))
		wantStatus(t, resp, http.StatusNotFound)
		wantAudit(t, r.lastAudit(), "check=route")
		if n := up.count(); n != 0 {
			t.Fatalf("the upstream saw %d requests", n)
		}
	})
	t.Run("token of another sandbox", func(t *testing.T) {
		up := newUpstream(t, nil)
		r := newRig(t, up, nil, arrivesOn("brig-a"), withSandboxes(
			Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []Route{hostRouteOf(t, "github-api", up)}},
			Sandbox{ID: "brig-b", Profile: "claude-code", Routes: []Route{hostRouteOf(t, "github-api", up)}},
		))
		resp := r.doHost("github-api.brig.internal", "GET", "/user", bearer(r.mint("brig-b", "github-api")))
		wantStatus(t, resp, http.StatusUnauthorized)
		wantAudit(t, r.lastAudit(), "check=signature")
		if n := up.count(); n != 0 {
			t.Fatalf("the upstream saw %d requests", n)
		}
	})
}

// The path of a host request is forwarded, never read as a route prefix.
func TestHostRouteIgnoresAPathNamingAnotherSandbox(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, nil, arrivesOn("brig-a"), withSandboxes(
		Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []Route{hostRouteOf(t, "github-api", up)}},
		Sandbox{ID: "brig-b", Profile: "claude-code", Routes: []Route{hostRouteOf(t, "github-api", up)}},
	))
	resp := r.doHost("github-api.brig.internal", "GET", "/brig-b/r/github-api/user", bearer(r.mint("brig-b", "github-api")))
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=signature")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostRouteRefusesATargetThatIsNotAPath(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{hostRouteOf(t, "github-api", up)}, arrivesOn("brig-a"))
	// The client cannot send OPTIONS *, so the handler is called directly.
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.URL.Path, req.URL.RawPath = "*", ""
	req.Host, req.RemoteAddr = "github-api.brig.internal", "127.0.0.1:1"
	req.Header = bearer(r.mint("brig-a", "github-api"))
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	r.lines++
	wantAudit(t, r.waitAudit(r.lines), "check=path")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostRouteOnLoopbackIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{hostRouteOf(t, "github-api", up)})
	resp := r.doHost("github-api.brig.internal", "GET", "/user", bearer(r.mint("brig-a", "github-api")))
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "check=arrival")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostRouteInClearTextIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{hostRouteOf(t, "github-api", up)}, arrivesOn("brig-a"),
		func(c *Config) { c.VendOnly = func(*http.Request) bool { return true } })
	resp := r.doHost("github-api.brig.internal", "GET", "/user", bearer(r.mint("brig-a", "github-api")))
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "check=arrival")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostNamingAnEndpointRouteIsNotFound(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{customRoute(t, "github", up.URL, nil)}, arrivesOn("brig-a"))
	resp := r.doHost("github.brig.internal", "GET", "/user", bearer(r.mint("brig-a", "github")))
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "check=route.mode")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostRouteByPathIsNotFound(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{hostRouteOf(t, "github-api", up)})
	resp, _ := r.do("GET", "/brig-a/r/github-api/user", bearer(r.mint("brig-a", "github-api")), "")
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "check=route.mode")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestHostRouteParsesHostForms(t *testing.T) {
	for host, want := range map[string]string{
		"github-api.brig.internal":           "github-api",
		"GitHub-API.brig.internal.:443":      "github-api",
		"brig.internal":                      "",
		"a.b.brig.internal":                  "",
		"github-api.brig.internal.evil":      "",
		"-x.brig.internal":                   "",
		"":                                   "",
		"github-api.brig.internal:443:80":    "",
		"github-api.brig.internal.evil.test": "",
	} {
		got, ok := hostRoute(host)
		if ok != (want != "") || got != want {
			t.Errorf("hostRoute(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestHostRouteForwardsWholePath(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{hostRouteOf(t, "github-api", up)}, arrivesOn("brig-a"))
	resp := r.doHost("github-api.brig.internal", "GET", "/user", bearer(r.mint("brig-a", "github-api")))
	wantStatus(t, resp, http.StatusOK)
	wantAudit(t, r.lastAudit(), "sandbox=brig-a", "route=github-api")
	if got := up.last(t).path; got != "/user" {
		t.Fatalf("the upstream saw path %q, want /user", got)
	}
	if strings.Contains(r.lastAudit(), "brig-a/r") {
		t.Errorf("the audit path carries a route prefix:\n%s", r.lastAudit())
	}
}

// gh sends its token as "Authorization: token <t>". Only the github-api
// route reads that scheme, and it injects the PAT as a Bearer value.
func TestTokenSchemeIsReadOnGitHubAPI(t *testing.T) {
	for _, scheme := range []string{"token", "Token"} {
		t.Run(scheme, func(t *testing.T) {
			up := newUpstream(t, nil)
			r := newRig(t, up, []Route{presetRoute(t, v1alpha1.PresetGitHubAPI, "github-api", "gh-token", up.URL, nil)}, arrivesOn("brig-a"))
			tok := r.mint("brig-a", "github-api")
			resp := r.doHost("github-api.brig.internal", "GET", "/api/v3/user", http.Header{"Authorization": {scheme + " " + tok}})
			wantStatus(t, resp, http.StatusOK)
			got := up.last(t).header.Get("Authorization")
			if got != "Bearer "+fakeGHToken {
				t.Errorf("the upstream saw Authorization %q, want the PAT as a Bearer value", got)
			}
			if strings.Contains(got, tok) {
				t.Errorf("the session token reached the upstream: %q", got)
			}
		})
	}
}

func TestTokenSchemeIsRefusedElsewhere(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{presetRoute(t, v1alpha1.PresetGitHubToken, "github", "gh-token", up.URL, nil)})
	tok := r.mint("brig-a", "github")
	resp, _ := r.do("GET", "/brig-a/r/github/user", http.Header{"Authorization": {"token " + tok}}, "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestTokenSchemeDoesNotHideASecondToken(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{presetRoute(t, v1alpha1.PresetGitHubAPI, "github-api", "gh-token", up.URL, nil)}, arrivesOn("brig-a"))
	tok := r.mint("brig-a", "github-api")
	resp := r.doHost("github-api.brig.internal", "GET", "/api/v3/user?t="+tok, http.Header{"Authorization": {"token " + tok}})
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "check=placeholder")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// ghAPIDo sends one request to the github-api route as gh does, and returns
// the response with its body.
func ghAPIDo(r *rig, method, path string) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = "github-api.brig.internal"
	req.Header.Set("Authorization", "token "+r.mint("brig-a", "github-api"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	r.lines++
	r.waitAudit(r.lines)
	return resp, string(b)
}

func ghAPIRig(t *testing.T, allow *v1alpha1.RequestAllow) (*rig, *upstream) {
	t.Helper()
	up := newUpstream(t, nil)
	return newRig(t, up, []Route{presetRoute(t, v1alpha1.PresetGitHubAPI, "github-api", "gh-token", up.URL, allow)}, arrivesOn("brig-a")), up
}

// gh addresses a GitHub Enterprise host; api.github.com serves the same
// calls without the /api/v3 and /api prefixes.
func TestGitHubAPIMapsEnterprisePaths(t *testing.T) {
	for _, tc := range []struct{ method, in, path, query string }{
		{"GET", "/api/v3", "/", ""},
		{"GET", "/api/v3/user", "/user", ""},
		{"POST", "/api/graphql", "/graphql", ""},
		{"GET", "/api/v3/repos/brig-sh/brig/pulls?state=open", "/repos/brig-sh/brig/pulls", "state=open"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			r, up := ghAPIRig(t, nil)
			resp, _ := ghAPIDo(r, tc.method, tc.in)
			wantStatus(t, resp, http.StatusOK)
			got := up.last(t)
			if got.path != tc.path || got.rawQuery != tc.query {
				t.Errorf("the upstream saw %q?%q, want %q?%q", got.path, got.rawQuery, tc.path, tc.query)
			}
		})
	}
}

func TestGitHubAPIRefusesOtherPaths(t *testing.T) {
	for _, in := range []string{"/user", "/api/v3x/user", "/api/graphqlx", "/api/graphql/x", "/%61pi/v3/user"} {
		t.Run(in, func(t *testing.T) {
			r, up := ghAPIRig(t, nil)
			resp, body := ghAPIDo(r, "GET", in)
			wantStatus(t, resp, http.StatusNotFound)
			wantAudit(t, r.lastAudit(), "check=github-api.path")
			if !strings.Contains(body, "brig broker: no such route") {
				t.Errorf("body %q", body)
			}
			if n := up.count(); n != 0 {
				t.Fatalf("the upstream saw %d requests", n)
			}
		})
	}
}

// request.allow is written in the form gh sends, before the mapping.
func TestGitHubAPIAllowMatchesTheSentPath(t *testing.T) {
	r, up := ghAPIRig(t, &v1alpha1.RequestAllow{Paths: []string{"/api/v3/user"}})
	resp, _ := ghAPIDo(r, "GET", "/api/v3/user")
	wantStatus(t, resp, http.StatusOK)
	resp, _ = ghAPIDo(r, "GET", "/api/v3/repos/x/y")
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "check=request.allow.paths")
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

func TestGHAPIUserThroughTheBroker(t *testing.T) {
	r, up := ghAPIRig(t, nil)
	resp, _ := ghAPIDo(r, "GET", "/api/v3/user")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if got.path != "/user" {
		t.Errorf("the upstream saw path %q, want /user", got.path)
	}
	if a := got.header.Get("Authorization"); a != "Bearer "+fakeGHToken {
		t.Errorf("the upstream saw Authorization %q, want the PAT as a Bearer value", a)
	}
}
