package broker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/secret"
)

var testHostKey = bytes.Repeat([]byte{7}, 32)

// Fake values only. The audit test looks for each of them.
const (
	fakeAccess  = "fake-access-token-stale"
	fakeFresh   = "fake-access-token-fresh"
	fakeAPIKey  = "fake-console-api-key"
	fakeGHToken = "fake-github-token"
)

func oauthDoc(access string) []byte {
	return []byte(`{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"fake-refresh","expiresAt":1}}`)
}

// fakeCreds is a secret.Broker and a Renewer that counts its calls.
type fakeCreds struct {
	mu       sync.Mutex
	docs     map[string][]byte
	err      error
	resolves int
	// renewTo is what ForceRenew returns, and renewErr its error.
	renewTo  []byte
	renewErr error
	renews   int
	stale    []byte
}

func newFakeCreds() *fakeCreds {
	return &fakeCreds{docs: map[string][]byte{
		"claude-credentials": oauthDoc(fakeAccess),
		"anthropic-api-key":  []byte(fakeAPIKey),
		"gh-token":           []byte(fakeGHToken),
		"inventory-token":    []byte(`{"token":"fake-inventory-token","expires":1}`),
	}, renewTo: oauthDoc(fakeFresh)}
}

func (f *fakeCreds) Resolve(_ context.Context, name string, _ secret.Workload) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return nil, f.err
	}
	doc, ok := f.docs[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return doc, nil
}

func (f *fakeCreds) ForceRenew(_ context.Context, _ string, _ secret.Workload, stale []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	f.stale = stale
	return f.renewTo, f.renewErr
}

func (f *fakeCreds) counts() (resolves, renews int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.renews
}

// resolveOnly hides ForceRenew, as a static store does.
type resolveOnly struct{ f *fakeCreds }

func (r resolveOnly) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	return r.f.Resolve(ctx, name, w)
}

// seen is one request as the upstream received it.
type seen struct {
	method, path, rawQuery, host string
	header                       http.Header
	body                         string
}

// upstream is an httptest TLS server that records every request.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }
	}
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, r.Header.Clone(), string(body)})
		u.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	// The TLS failure test makes the server log a refused handshake.
	u.Config.ErrorLog = log.New(io.Discard, "", 0)
	u.StartTLS()
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) last(t *testing.T) seen {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("the upstream saw no request")
	}
	return u.reqs[len(u.reqs)-1]
}

// syncBuffer is the audit sink. The audit line is written after the response
// is sent, so readers wait for it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClock is a clock the tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// presetRoute expands a preset route the way a policy names it, then points
// it at the test upstream. A preset with a lifecycle keeps its upstream
// through ExpandRoute, so the test sets it after the expansion.
func presetRoute(t *testing.T, preset v1alpha1.Preset, name, secretName, upstream string,
	allow *v1alpha1.RequestAllow) Route {
	t.Helper()
	in := v1alpha1.Route{
		Name:       name,
		Preset:     preset,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: secretName}},
	}
	if allow != nil {
		in.Request = &v1alpha1.RequestPolicy{Allow: allow}
	}
	r, err := policy.ExpandRoute(in)
	if err != nil {
		t.Fatal(err)
	}
	r.Upstream = upstream
	return Route{Policy: "test-" + name, Route: r}
}

// customRoute is example 4 of the spec, a route written in full.
func customRoute(t *testing.T, name, upstream string, allow *v1alpha1.RequestAllow) Route {
	t.Helper()
	in := v1alpha1.Route{
		Name:     name,
		Upstream: upstream,
		Credential: &v1alpha1.CredentialRef{
			SecretRef: v1alpha1.SecretRef{Name: "inventory-token"},
			Field:     "token",
		},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "X-Api-Key", Value: "{value}"}},
		}},
		Guest: &v1alpha1.Guest{Env: map[string]string{
			"INVENTORY_URL":   "{route.url}",
			"INVENTORY_TOKEN": "{session.token}",
		}},
	}
	if allow != nil {
		in.Request = &v1alpha1.RequestPolicy{Allow: allow}
	}
	r, err := policy.ExpandRoute(in)
	if err != nil {
		t.Fatal(err)
	}
	return Route{Policy: "inventory-api", Route: r}
}

// rig is a broker in front of one upstream, serving brig-a and brig-b with
// the same routes.
type rig struct {
	t     *testing.T
	h     *Handler
	srv   *httptest.Server
	up    *upstream
	creds *fakeCreds
	clock *fakeClock
	audit *syncBuffer
	lines int
}

type rigOption func(*Config)

func newRig(t *testing.T, up *upstream, routes []Route, opts ...rigOption) *rig {
	t.Helper()
	r := &rig{
		t:     t,
		up:    up,
		creds: newFakeCreds(),
		clock: &fakeClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)},
		audit: &syncBuffer{},
	}
	cfg := Config{
		HostKey:     testHostKey,
		Credentials: r.creds,
		Audit:       slog.New(slog.NewTextHandler(r.audit, nil)),
		Now:         r.clock.Now,
		Transport:   up.Client().Transport,
		Sandboxes: []Sandbox{
			{ID: "brig-a", Profile: "claude-code", Routes: routes},
			{ID: "brig-b", Profile: "claude-code", Routes: routes},
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) mint(sandbox, route string) string {
	r.t.Helper()
	tok, err := r.h.Mint(sandbox, "refactor", route, DefaultTokenTTL)
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

// do sends one request to the broker without following redirects, and waits
// for its audit line.
func (r *rig) do(method, path string, h http.Header, body string) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	r.lines++
	r.waitAudit(r.lines)
	return resp, string(got)
}

// waitAudit waits for n audit lines and returns the last one.
func (r *rig) waitAudit(n int) string {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := strings.Split(strings.TrimSpace(r.audit.String()), "\n")
		if len(lines) >= n && lines[0] != "" {
			return lines[n-1]
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("waited for %d audit lines, have:\n%s", n, r.audit.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) lastAudit() string { return r.waitAudit(r.lines) }

func claudeRoutes(t *testing.T, up *upstream) []Route {
	return []Route{
		presetRoute(t, v1alpha1.PresetClaudeCode, "anthropic", "claude-credentials", up.URL, nil),
		presetRoute(t, v1alpha1.PresetGitHubToken, "github", "gh-token", up.URL,
			&v1alpha1.RequestAllow{Repos: []string{"brig-sh/*"}}),
	}
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status %d, want %d", resp.StatusCode, want)
	}
}

func wantAudit(t *testing.T, line string, fields ...string) {
	t.Helper()
	for _, f := range fields {
		if !strings.Contains(line, f) {
			t.Errorf("audit line lacks %q:\n%s", f, line)
		}
	}
}

func TestClaudeCodePresetEndToEnd(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	h := bearer(tok)
	h.Set("X-Api-Key", "junk-guest-api-key")
	h.Set("X-Brig-Session", "junk-brig-session")
	h.Set("X-Brig-Debug", "1")
	h.Set("Proxy-Authorization", "Basic Zm9vOmJhcg==")
	h.Set("Proxy-Extra", "1")
	h.Set("Anthropic-Beta", "oauth-2025-04-20")
	h.Set("Content-Type", "application/json")
	// Headers that tell a proxy or a framework where the request came from,
	// or which method or path to serve instead.
	for _, name := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Prefix", "X-Forwarded-Port", "X-Forwarded-Server",
		"Forwarded", "Via", "X-Real-Ip", "X-Http-Method-Override", "X-Method-Override", "X-Original-Url",
		"X-Rewrite-Url",
	} {
		h.Set(name, "/admin")
	}
	// Go keeps a name with an underscore as written. A server that reads _
	// as - would take these for X-Api-Key and Anthropic-Beta.
	h["x_api_key"] = []string{"junk-underscore-api-key"}
	h["anthropic_beta"] = []string{"unlisted-beta-2099-01-01"}
	h.Set("Accept-Encoding", "br")
	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages?beta=true", h, `{"model":"claude-sonnet-5"}`)
	wantStatus(t, resp, http.StatusOK)
	if body != "ok" {
		t.Fatalf("body %q", body)
	}
	got := up.last(t)
	if a := got.header.Get("Authorization"); a != "Bearer "+fakeAccess {
		t.Errorf("upstream Authorization = %q, want the real bearer", a)
	}
	if got.path != "/v1/messages" || got.rawQuery != "beta=true" || got.method != "POST" {
		t.Errorf("upstream got %s %s?%s", got.method, got.path, got.rawQuery)
	}
	if got.host != strings.TrimPrefix(up.URL, "https://") {
		t.Errorf("upstream Host = %q", got.host)
	}
	if got.body != `{"model":"claude-sonnet-5"}` {
		t.Errorf("upstream body = %q", got.body)
	}
	if got.header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
		t.Errorf("upstream Anthropic-Beta = %q", got.header.Get("Anthropic-Beta"))
	}
	if got.header.Get("Accept-Encoding") == "br" {
		t.Error("upstream got the guest's Accept-Encoding")
	}
	if len(got.header) == 0 {
		t.Fatal("upstream got no headers")
	}
	for name, values := range got.header {
		lower := fold(name)
		if lower == "x-api-key" || strings.HasPrefix(lower, "x-brig-") || strings.HasPrefix(lower, "proxy-") ||
			strings.HasPrefix(lower, "x-forwarded-") || (lower != "accept-encoding" && slices.Contains(droppedNames, lower)) ||
			strings.Contains(name, "_") {
			t.Errorf("upstream got header %s", name)
		}
		for _, v := range values {
			if strings.Contains(v, "/admin") || strings.Contains(v, "junk-") || strings.Contains(v, "unlisted-") {
				t.Errorf("upstream header %s holds a guest value %q", name, v)
			}
		}
		for _, v := range values {
			if strings.Contains(v, tok) {
				t.Errorf("upstream header %s holds the session token", name)
			}
		}
	}
	wantAudit(t, r.lastAudit(), "workload=spiffe://brig.local/brig/profile/claude-code/session/refactor",
		"sandbox=brig-a", "route=anthropic", "method=POST", "path=/v1/messages", "status=200", "dur=")
}

func TestTokenOfAnotherSandboxIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	resp, _ := r.do("POST", "/brig-b/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "status=401", "check=signature")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times", res)
	}
}

func TestTokenForAnotherRouteIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	resp, _ := r.do("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs", bearer(r.mint("brig-a", "anthropic")), "")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=token.route")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok, err := r.h.Mint("brig-a", "", "anthropic", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=expired")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestForgedTokenIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	other, err := New(Config{
		HostKey:     bytes.Repeat([]byte{8}, 32),
		Credentials: &fakeCreds{},
		Sandboxes:   []Sandbox{{ID: "brig-a", Routes: claudeRoutes(t, up)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	forged, err := other.Mint("brig-a", "refactor", "anthropic", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(forged), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=signature")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestMissingTokenIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	for _, h := range []http.Header{
		nil,
		{"Authorization": {"Bearer "}},
		{"Authorization": {"Bearer not-a-token"}},
		// claude-code sets Authorization, so x-api-key does not carry a
		// token on its route.
		{"X-Api-Key": {r.mint("brig-a", "anthropic")}},
	} {
		resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
		wantStatus(t, resp, http.StatusUnauthorized)
		wantAudit(t, r.lastAudit(), "check=token")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

func TestArrivalFromAnotherGatewayIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	var mu sync.Mutex
	var asked []string
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) {
		c.ArrivalCheck = func(sandbox, remoteAddr string) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, sandbox+" "+remoteAddr)
			return sandbox == "brig-b"
		}
	})
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=arrival")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "brig-a 127.0.0.1:") {
		t.Fatalf("ArrivalCheck was asked %q", asked)
	}
}

func TestUnknownSandboxOrRouteIsNotFound(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for path, check := range map[string]string{
		"/brig-z/r/anthropic/v1/messages": "check=sandbox",
		"/brig-a/r/nope/v1/messages":      "check=route",
		"/brig-a/x/anthropic/v1/messages": "check=path",
		"/brig-a/r/":                      "check=path",
		"/":                               "check=path",
	} {
		resp, _ := r.do("POST", path, bearer(tok), "{}")
		wantStatus(t, resp, http.StatusNotFound)
		wantAudit(t, r.lastAudit(), check)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times", res)
	}
}

func TestRequestAllowMissIsRefusedBeforeResolve(t *testing.T) {
	up := newUpstream(t, nil)
	routes := append(claudeRoutes(t, up),
		customRoute(t, "inventory", up.URL, &v1alpha1.RequestAllow{
			Methods: []string{"GET"},
			Paths:   []string{"/v2/items/*", "/v2/search"},
		}))
	r := newRig(t, up, routes)
	gitAuth := func() http.Header {
		req := http.Request{Header: http.Header{}}
		req.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
		return req.Header
	}
	for _, tc := range []struct {
		method, path string
		h            http.Header
		want         string
	}{
		{"POST", "/brig-a/r/github/other-org/repo/git-receive-pack", gitAuth(),
			"brig broker refused this request\n" +
				"policy:  test-github\n" +
				"route:   github\n" +
				"rule:    spec.routes[github].request.allow.repos\n" +
				`reason:  repo "other-org/repo" is not in the allow list [brig-sh/*]` + "\n" +
				"sandbox: brig-a\n"},
		{"GET", "/brig-a/r/github/brig-sh", gitAuth(),
			"brig broker refused this request\n" +
				"policy:  test-github\n" +
				"route:   github\n" +
				"rule:    spec.routes[github].request.allow.repos\n" +
				`reason:  path "/brig-sh" names no owner/name repository` + "\n" +
				"sandbox: brig-a\n"},
		{"POST", "/brig-a/r/inventory/v2/items/42", bearer(r.mint("brig-a", "inventory")),
			"brig broker refused this request\n" +
				"policy:  inventory-api\n" +
				"route:   inventory\n" +
				"rule:    spec.routes[inventory].request.allow.methods\n" +
				`reason:  method "POST" is not in the allow list [GET]` + "\n" +
				"sandbox: brig-a\n"},
		{"GET", "/brig-a/r/inventory/v2/items/42/history", bearer(r.mint("brig-a", "inventory")),
			"brig broker refused this request\n" +
				"policy:  inventory-api\n" +
				"route:   inventory\n" +
				"rule:    spec.routes[inventory].request.allow.paths\n" +
				`reason:  path "/v2/items/42/history" is not in the allow list [/v2/items/*, /v2/search]` + "\n" +
				"sandbox: brig-a\n"},
	} {
		resp, body := r.do(tc.method, tc.path, tc.h, "")
		wantStatus(t, resp, http.StatusForbidden)
		if body != tc.want {
			t.Errorf("%s %s: body\n%s\nwant\n%s", tc.method, tc.path, body, tc.want)
		}
		rule := strings.TrimPrefix(strings.Split(tc.want, "\n")[3], "rule:    ")
		wantAudit(t, r.lastAudit(), "status=403", "rule="+rule, "check=request.allow.")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times before a refusal", res)
	}
}

func TestGitRouteTakesTheTokenAsBasicPassword(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	req := http.Request{Header: http.Header{}}
	req.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
	resp, _ := r.do("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs?service=git-upload-pack", req.Header, "")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeGHToken))
	if got.header.Get("Authorization") != want {
		t.Errorf("upstream Authorization = %q, want basic auth with the real token", got.header.Get("Authorization"))
	}
	if got.path != "/brig-sh/brig.git/info/refs" || got.rawQuery != "service=git-upload-pack" {
		t.Errorf("upstream got %s?%s", got.path, got.rawQuery)
	}
}

func TestAPIKeyPresetTakesTheTokenFromXAPIKey(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{
		presetRoute(t, v1alpha1.PresetAnthropicAPIKey, "anthropic", "anthropic-api-key", up.URL, nil),
	})
	h := http.Header{
		"X-Api-Key":      {r.mint("brig-a", "anthropic")},
		"Anthropic-Beta": {"oauth-2025-04-20,claude-code-20250219"},
	}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if got.header.Get("X-Api-Key") != fakeAPIKey || got.header.Get("Authorization") != "" {
		t.Errorf("upstream x-api-key %q, Authorization %q", got.header.Get("X-Api-Key"), got.header.Get("Authorization"))
	}
	// The API key preset has no OAuth flag on its list.
	if got.header.Get("Anthropic-Beta") != "claude-code-20250219" {
		t.Errorf("upstream Anthropic-Beta = %q", got.header.Get("Anthropic-Beta"))
	}
	if _, renews := r.creds.counts(); renews != 0 {
		t.Fatal("a static key was renewed")
	}
}

func TestBetaValuesOutsideTheListAreDropped(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	h := bearer(r.mint("brig-a", "anthropic"))
	h["Anthropic-Beta"] = []string{"claude-code-20250219, unlisted-beta-2099-01-01", "context-1m-2025-08-07"}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if v := got.header.Values("Anthropic-Beta"); !slices.Equal(v, []string{"claude-code-20250219,context-1m-2025-08-07"}) {
		t.Errorf("upstream Anthropic-Beta = %q", v)
	}
	line := r.lastAudit()
	wantAudit(t, line, "dropped.anthropic-beta=1")
	if strings.Contains(line, "unlisted-beta") {
		t.Error("the audit line holds a header value")
	}

	h = bearer(r.mint("brig-a", "anthropic"))
	h.Set("Anthropic-Beta", "unlisted-beta-2099-01-01")
	r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
	if v := up.last(t).header.Values("Anthropic-Beta"); len(v) != 0 {
		t.Errorf("upstream Anthropic-Beta = %q, want none", v)
	}
}

func TestResolveFailureIsBadGateway(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	r.creds.mu.Lock()
	r.creds.err = errors.New("the keychain is locked")
	r.creds.mu.Unlock()
	tok := r.mint("brig-a", "anthropic")
	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusBadGateway)
	if strings.Contains(body, "keychain") {
		t.Errorf("the 502 body carries the backend error: %q", body)
	}
	wantAudit(t, r.lastAudit(), "check=credential")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}

	// A failure is not cached: the next request resolves again.
	r.creds.mu.Lock()
	r.creds.err = nil
	r.creds.mu.Unlock()
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusOK)
	if res, _ := r.creds.counts(); res != 2 {
		t.Fatalf("Resolve ran %d times, want 2", res)
	}
}

func TestMissingCredentialFieldIsBadGateway(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	r.creds.mu.Lock()
	r.creds.docs["claude-credentials"] = []byte(`{"claudeAiOauth":{"refreshToken":"x"}}`)
	r.creds.mu.Unlock()
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusBadGateway)
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// refuseStale answers 401 to any bearer but the fresh one.
func refuseStale(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeFresh {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "stale")
		return
	}
	body, _ := io.ReadAll(r.Body)
	_, _ = w.Write(body)
}

func TestUpstream401RenewsOnceAndRetries(t *testing.T) {
	up := newUpstream(t, refuseStale)
	r := newRig(t, up, claudeRoutes(t, up))
	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")),
		`{"model":"claude-sonnet-5"}`)
	wantStatus(t, resp, http.StatusOK)
	if body != `{"model":"claude-sonnet-5"}` {
		t.Errorf("the retry carried body %q", body)
	}
	if n := up.count(); n != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", n)
	}
	_, renews := r.creds.counts()
	if renews != 1 {
		t.Fatalf("ForceRenew ran %d times, want 1", renews)
	}
	r.creds.mu.Lock()
	stale := r.creds.stale
	r.creds.mu.Unlock()
	if !bytes.Equal(stale, oauthDoc(fakeAccess)) {
		t.Errorf("ForceRenew got stale %q, want the document the request was sent with", stale)
	}
	wantAudit(t, r.lastAudit(), "status=200", "renew=retried")
}

func TestUpstream401AfterRenewalPassesThrough(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	r := newRig(t, up, claudeRoutes(t, up))
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	if n := up.count(); n != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", n)
	}
	if _, renews := r.creds.counts(); renews != 1 {
		t.Fatalf("ForceRenew ran %d times, want 1", renews)
	}
}

func TestUpstream401WithoutRenewerPassesThrough(t *testing.T) {
	up := newUpstream(t, refuseStale)
	creds := newFakeCreds()
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) { c.Credentials = resolveOnly{creds} })
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
	if _, renews := creds.counts(); renews != 0 {
		t.Fatalf("ForceRenew ran %d times", renews)
	}
}

func TestUpstream401WhenRenewalFailsPassesThrough(t *testing.T) {
	up := newUpstream(t, refuseStale)
	r := newRig(t, up, claudeRoutes(t, up))
	r.creds.mu.Lock()
	r.creds.renewErr = errors.New("the provider refused the refresh")
	r.creds.mu.Unlock()
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
	wantAudit(t, r.lastAudit(), "renew=failed")
}

func TestEventStreamIsNotBuffered(t *testing.T) {
	third := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: one\ndata: 1\n\n")
		f.Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "event: two\ndata: 2\n\n")
		f.Flush()
		time.Sleep(100 * time.Millisecond)
		select {
		case <-third:
		case <-time.After(10 * time.Second):
		}
		_, _ = io.WriteString(w, "event: three\ndata: 3\n\n")
	})
	r := newRig(t, up, claudeRoutes(t, up))
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(r.mint("brig-a", "anthropic"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type read struct {
		event string
		err   error
	}
	events := make(chan read)
	go func() {
		br := bufio.NewReader(resp.Body)
		var ev strings.Builder
		for {
			line, err := br.ReadString('\n')
			ev.WriteString(line)
			if err != nil {
				events <- read{ev.String(), err}
				close(events)
				return
			}
			if line == "\n" {
				events <- read{ev.String(), nil}
				ev.Reset()
			}
		}
	}()
	next := func() string {
		select {
		case e := <-events:
			if e.err != nil {
				t.Fatalf("stream ended: %v after %q", e.err, e.event)
			}
			return e.event
		case <-time.After(5 * time.Second):
			t.Fatal("an event did not arrive before the server sent the next one")
		}
		return ""
	}
	if e := next(); e != "event: one\ndata: 1\n\n" {
		t.Fatalf("first event %q", e)
	}
	if e := next(); e != "event: two\ndata: 2\n\n" {
		t.Fatalf("second event %q", e)
	}
	close(third)
	if e := next(); e != "event: three\ndata: 3\n\n" {
		t.Fatalf("third event %q", e)
	}
}

// A body longer than maxReplay goes upstream before it ends. Without full
// duplex, the broker's server reads the rest of the body and closes it when the
// response header goes out. That read competes with the transport for the
// body, or waits for the guest to finish it.
func TestEventStreamStartsBeforeTheRequestBodyEnds(t *testing.T) {
	up := &upstream{}
	reached := make(chan struct{})
	var once sync.Once
	up.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\n")
		_ = rc.Flush()
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "data: %d\n\n", n)
	}))
	up.StartTLS()
	t.Cleanup(up.Close)
	r := newRig(t, up, claudeRoutes(t, up))
	pr, pw := io.Pipe()
	// The cleanup ends the request body, so a broker that waits for it
	// does not keep the servers from closing.
	t.Cleanup(func() { _ = pw.CloseWithError(errors.New("the test ended")) })
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(r.mint("brig-a", "anthropic"))
	type first struct {
		resp  *http.Response
		br    *bufio.Reader
		event string
		err   error
	}
	got := make(chan first, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- first{err: err}
			return
		}
		br := bufio.NewReader(resp.Body)
		a, _ := br.ReadString('\n')
		b, err := br.ReadString('\n')
		got <- first{resp, br, a + b, err}
	}()
	wrote := make(chan struct{})
	go func() {
		_, _ = io.WriteString(pw, strings.Repeat("x", maxReplay+1))
		close(wrote)
	}()
	// The broker scans the first maxReplay bytes before it forwards. Under
	// the race detector with coverage that takes seconds, so the wait for
	// the first event starts once the upstream has the request.
	var f first
	select {
	case <-reached:
		select {
		case f = <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("the first event waited for the end of the request body")
		}
	case f = <-got:
	case <-time.After(time.Minute):
		t.Fatal("the request did not reach the upstream before its body ended")
	}
	if f.resp != nil {
		defer f.resp.Body.Close()
	}
	if f.err != nil || f.event != "data: 1\n\n" {
		t.Fatalf("first event %q, %v", f.event, f.err)
	}
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream did not read the request body")
	}
	_, _ = io.WriteString(pw, "x")
	_ = pw.Close()
	rest, err := io.ReadAll(f.br)
	if want := fmt.Sprintf("data: %d\n\n", maxReplay+2); err != nil || string(rest) != want {
		t.Fatalf("second event %q, %v", rest, err)
	}
}

func TestRedirectToAnotherOriginIsNotFollowed(t *testing.T) {
	other := newUpstream(t, nil)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/collect", http.StatusFound)
	})
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) {
		// The transport trusts both servers, so only the broker's refusal
		// to follow keeps the request away from the other origin.
		pool := up.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		pool.AddCert(other.Certificate())
	})
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusFound)
	if n := other.count(); n != 0 {
		t.Fatalf("the other origin saw %d requests", n)
	}
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

func TestUpstreamTLSFailureIsBadGateway(t *testing.T) {
	up := newUpstream(t, nil)
	// The default transport verifies with the system roots, which do not
	// hold the test server's certificate.
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) { c.Transport = nil })
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=upstream")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream served %d requests over an unverified connection", n)
	}
}

func TestNonCanonicalPathIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	for _, p := range []string{
		"/brig-sh/brig/../../other-org/repo/git-receive-pack",
		"/brig-sh/brig/%2e%2e/%2e%2e/other-org/repo/git-receive-pack",
		"/brig-sh%2fbrig/git-receive-pack",
		"/brig-sh//brig/git-receive-pack",
		"/brig-sh/brig/./git-receive-pack",
		"/brig-sh/brig.git/..;/..;/other-org/repo/git-receive-pack",
		"/brig-sh/brig/.;x/git-receive-pack",
		"/brig-sh/brig/;x/git-receive-pack",
		"/brig-sh/brig/%252e%252e/%252e%252e/other-org/repo/git-receive-pack",
		"/brig-sh/brig%252fother/git-receive-pack",
		"/brig-sh/brig/%EF%BC%8E%EF%BC%8E/%EF%BC%8E%EF%BC%8E/other-org/repo/git-receive-pack",
		"/brig-sh/br%C3%AFg/git-receive-pack",
	} {
		req := http.Request{Header: http.Header{}}
		req.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
		resp, _ := r.do("POST", "/brig-a/r/github"+p, req.Header, "")
		wantStatus(t, resp, http.StatusBadRequest)
		wantAudit(t, r.lastAudit(), "check=path")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times", res)
	}
}

func TestRemovedSandboxTokensAreRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusOK)
	r.h.RemoveSandbox("brig-a")
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusNotFound)
	wantAudit(t, r.lastAudit(), "check=sandbox")
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
	if _, err := r.h.Mint("brig-a", "", "anthropic", time.Hour); err == nil {
		t.Fatal("Mint minted for a removed sandbox")
	}
	// brig-b is untouched.
	resp, _ = r.do("POST", "/brig-b/r/anthropic/v1/messages", bearer(r.mint("brig-b", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusOK)
}

func TestNonASCIIPathOutsideRepoRoutes(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{customRoute(t, "inventory", up.URL, nil)})
	resp, _ := r.do("GET", "/brig-a/r/inventory/v2/caf%C3%A9", bearer(r.mint("brig-a", "inventory")), "")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).path; got != "/v2/caf%C3%A9" {
		t.Fatalf("upstream path %q", got)
	}
	// A segment of fullwidth dots is a dot segment to an upstream that
	// normalizes Unicode, so it is refused on every route.
	resp, _ = r.do("GET", "/brig-a/r/inventory/v2/%EF%BC%8E%EF%BC%8E/admin", bearer(r.mint("brig-a", "inventory")), "")
	wantStatus(t, resp, http.StatusBadRequest)
	wantAudit(t, r.lastAudit(), "check=path")
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

func TestUpgradeIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{customRoute(t, "inventory", up.URL, &v1alpha1.RequestAllow{Methods: []string{"GET"}})})
	tok := r.mint("brig-a", "inventory")
	for _, extra := range []http.Header{
		{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
		{"Connection": {"keep-alive, upgrade"}},
		{"Upgrade": {"h2c"}},
	} {
		h := bearer(tok)
		for k, v := range extra {
			h[k] = v
		}
		resp, _ := r.do("GET", "/brig-a/r/inventory/v2/items/1", h, "")
		wantStatus(t, resp, http.StatusBadRequest)
		wantAudit(t, r.lastAudit(), "check=upgrade")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times", res)
	}
}

// CONNECT opens a tunnel, which carries the credential like an upgrade.
func TestConnectIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for _, target := range []string{"/brig-a/r/anthropic/v1/messages", "brig.internal:443"} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: brig.internal\r\nAuthorization: Bearer %s\r\n\r\n",
			target, tok)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("CONNECT %s: status %d, want 400", target, resp.StatusCode)
		}
		r.lines++
		wantAudit(t, r.lastAudit(), "method=CONNECT", "check=upgrade")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times", res)
	}
}

// An upstream that sends a 1xx, such as 103 Early Hints, sends it before the
// response the broker redacts. None reaches the guest.
func TestInformationalResponseDoesNotReachTheGuest(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("X-Echo-Authorization", auth)
		w.Header().Set("Link", "</style.css>; rel=preload")
		http.SetCookie(w, &http.Cookie{Name: "echo", Value: base64.RawURLEncoding.EncodeToString([]byte(auth))})
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("X-Echo-Authorization")
		w.Header().Del("Set-Cookie")
		_, _ = io.WriteString(w, "ok")
	})
	r := newRig(t, up, claudeRoutes(t, up))
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(r.mint("brig-a", "anthropic"))
	var got []string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		got = append(got, fmt.Sprintf("%d %v", code, h))
		return nil
	}}
	resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	wantStatus(t, resp, http.StatusOK)
	if string(body) != "ok" {
		t.Fatalf("body %q", body)
	}
	if len(got) != 0 {
		t.Fatalf("the guest got an informational response: %q", got)
	}
	r.lines++
	wantAudit(t, r.lastAudit(), "status=200")
}

// The transport undoes only the gzip it asked for. A body in any other
// coding cannot be redacted, so it never reaches the guest.
func TestEncodedResponseIsRefused(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		coding := r.URL.Query().Get("coding")
		if coding == "" {
			coding = "gzip"
		}
		w.Header().Set("Content-Encoding", coding)
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, "data: "+r.Header.Get("Authorization")+"\n\n")
		_ = zw.Close()
	})
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for _, tc := range []struct {
		name, query string
		h           http.Header
	}{
		{"x-gzip", "coding=x-gzip", nil},
		{"br", "coding=br", nil},
		{"deflate", "coding=deflate", nil},
		{"two codings", "coding=gzip,%20gzip", nil},
		// The transport asks for no compression on a range request, and
		// does not undo the gzip the upstream sends anyway.
		{"gzip the transport did not ask for", "", http.Header{"Range": {"bytes=0-"}}},
	} {
		h := bearer(tok)
		for k, v := range tc.h {
			h[k] = v
		}
		resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages?"+tc.query, h, "{}")
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", tc.name, resp.StatusCode)
		}
		wantAudit(t, r.lastAudit(), "check=response.encoding", "status=502")
		if strings.Contains(body, fakeAccess) {
			t.Errorf("%s: the body holds the credential", tc.name)
		}
		if zr, err := gzip.NewReader(strings.NewReader(body)); err == nil {
			if plain, _ := io.ReadAll(zr); strings.Contains(string(plain), fakeAccess) {
				t.Errorf("%s: the guest got the credential compressed: %q", tc.name, plain)
			}
		}
	}
	if got := up.last(t).header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("a range request asked the upstream for %q, want identity", got)
	}

	// The gzip the transport asked for is undone, and the body redacted.
	h := bearer(tok)
	h.Set("Accept-Encoding", "br")
	resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
	wantStatus(t, resp, http.StatusOK)
	if body != "data: Bearer [redacted]\n\n" {
		t.Errorf("body %q", body)
	}
	if got := up.last(t).header.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("the upstream was asked for %q, want gzip", got)
	}
}

// The guest sends two Authorization headers. The upstream gets one: the
// credential.
func TestDuplicateAuthorizationReachesUpstreamOnce(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	h := bearer(r.mint("brig-a", "anthropic"))
	h.Add("Authorization", "Bearer junk-guest-bearer")
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", h, "{}")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+fakeAccess {
		t.Fatalf("upstream Authorization = %q, want only the credential", got)
	}
}

// A refusal before the path is split leaves the path off the audit line.
func TestAuditPathIsTheRest(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	h := bearer(tok)
	h.Set("Upgrade", "websocket")
	r.do("GET", "/brig-a/r/anthropic/v1/messages", h, "")
	wantAudit(t, r.lastAudit(), "check=upgrade")
	if line := r.lastAudit(); strings.Contains(line, "path=") {
		t.Errorf("a refused upgrade has a path:\n%s", line)
	}
	r.do("GET", "/brig-a/x/anthropic/v1/messages", bearer(tok), "")
	wantAudit(t, r.lastAudit(), "check=path")
	if line := r.lastAudit(); strings.Contains(line, "path=") {
		t.Errorf("a path that does not split is on the audit line:\n%s", line)
	}
	r.do("GET", "/brig-a/r/anthropic/v1/messages", bearer(tok), "")
	wantAudit(t, r.lastAudit(), "path=/v1/messages")
}

// echo reflects the request's Authorization in a header, a cookie, a
// trailer and the body. The body splits the value across two flushes.
func echo(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	w.Header().Set("Trailer", "X-Echo-Trailer")
	w.Header().Set("X-Echo-Authorization", auth)
	w.Header().Set("X-Unrelated", "kept")
	http.SetCookie(w, &http.Cookie{Name: "echo", Value: base64.RawURLEncoding.EncodeToString([]byte(auth))})
	http.SetCookie(w, &http.Cookie{Name: "plain", Value: "x"})
	w.Header().Set("Content-Type", "text/event-stream")
	half := len(auth) / 2
	_, _ = io.WriteString(w, "data: "+auth[:half])
	w.(http.Flusher).Flush()
	time.Sleep(50 * time.Millisecond)
	_, _ = io.WriteString(w, auth[half:]+"\n\n")
	_, _ = io.WriteString(w, "data: again "+auth+"\n\n")
	w.Header().Set("X-Echo-Trailer", auth)
}

func TestReflectedCredentialIsRedacted(t *testing.T) {
	up := newUpstream(t, echo)
	r := newRig(t, up, claudeRoutes(t, up))
	for _, tc := range []struct {
		path   string
		h      http.Header
		secret string
	}{
		{"/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), fakeAccess},
		{"/brig-a/r/github/brig-sh/brig.git/info/refs", func() http.Header {
			req := http.Request{Header: http.Header{}}
			req.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
			return req.Header
		}(), base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeGHToken))},
	} {
		req, err := http.NewRequest("GET", r.srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = tc.h
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, resp, http.StatusOK)
		if strings.Contains(string(body), tc.secret) {
			t.Errorf("%s: the body holds the credential: %q", tc.path, body)
		}
		if want := "data: Bearer [redacted]\n\ndata: again Bearer [redacted]\n\n"; tc.secret == fakeAccess && string(body) != want {
			t.Errorf("%s: body %q, want %q", tc.path, body, want)
		}
		if strings.Count(string(body), redacted) != 2 {
			t.Errorf("%s: body %q does not redact both copies", tc.path, body)
		}
		if v := resp.Header.Values("Set-Cookie"); len(v) != 0 {
			t.Errorf("%s: the guest got Set-Cookie %q", tc.path, v)
		}
		if resp.Header.Get("X-Echo-Authorization") != "" {
			t.Errorf("%s: the guest got the reflected header", tc.path)
		}
		if resp.Header.Get("X-Unrelated") != "kept" {
			t.Errorf("%s: an unrelated header was dropped", tc.path)
		}
		if v := resp.Trailer.Get("X-Echo-Trailer"); v != "" {
			t.Errorf("%s: the guest got trailer %q", tc.path, v)
		}
		for name, values := range resp.Header {
			if holdsAny(strings.Join(values, " "), []string{fakeAccess, fakeGHToken, tc.secret}) {
				t.Errorf("%s: header %s holds the credential", tc.path, name)
			}
		}
	}
}

func TestRedactedEventStreamIsNotBuffered(t *testing.T) {
	third := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Each event ends in a byte that starts no secret, and the next
		// one waits for the guest to have read it.
		_, _ = io.WriteString(w, "data: Bearer f\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-third:
		case <-time.After(10 * time.Second):
		}
		_, _ = io.WriteString(w, "data: "+r.Header.Get("Authorization")+"\n\n")
	})
	r := newRig(t, up, claudeRoutes(t, up))
	req, _ := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", strings.NewReader("{}"))
	req.Header = bearer(r.mint("brig-a", "anthropic"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		a, _ := br.ReadString('\n')
		b, _ := br.ReadString('\n')
		first <- a + b
	}()
	select {
	case e := <-first:
		if e != "data: Bearer f\n\n" {
			t.Fatalf("first event %q", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was held back")
	}
	close(third)
	rest, _ := io.ReadAll(br)
	if string(rest) != "data: Bearer [redacted]\n\n" {
		t.Fatalf("second event %q", rest)
	}
}

func TestOversizedBodyIsNotReplayed(t *testing.T) {
	up := newUpstream(t, refuseStale)
	r := newRig(t, up, claudeRoutes(t, up))
	body := strings.Repeat("x", maxReplay+1)
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), body)
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "renew=body")
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
	if _, renews := r.creds.counts(); renews != 0 {
		t.Fatalf("ForceRenew ran %d times", renews)
	}
}

func TestReplayBudgetBoundsBuffering(t *testing.T) {
	up := newUpstream(t, refuseStale)
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) {
		c.ReplayBudget = 16
		c.BodyTimeout = 200 * time.Millisecond
	})
	// A body the budget cannot hold is refused, since it cannot be scanned.
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")),
		strings.Repeat("x", 100))
	wantStatus(t, resp, http.StatusServiceUnavailable)
	wantAudit(t, r.lastAudit(), "check=budget")
	// A body that fits is replayed, and its bytes come back to the budget.
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusOK)
	r.h.replay.mu.Lock()
	left := r.h.replay.left
	r.h.replay.mu.Unlock()
	if left != 16 {
		t.Fatalf("the budget holds %d bytes after the requests, want 16", left)
	}
}

func TestForcedRenewalsAreSpaced(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	r := newRig(t, up, claudeRoutes(t, up))
	send := func() {
		t.Helper()
		resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
		wantStatus(t, resp, http.StatusUnauthorized)
	}
	send()
	send()
	wantAudit(t, r.lastAudit(), "renew=interval")
	if _, renews := r.creds.counts(); renews != 1 {
		t.Fatalf("ForceRenew ran %d times within the interval, want 1", renews)
	}
	if n := up.count(); n != 3 {
		t.Fatalf("the upstream saw %d requests, want 3", n)
	}
	r.clock.Advance(renewInterval)
	send()
	if _, renews := r.creds.counts(); renews != 2 {
		t.Fatalf("ForceRenew ran %d times after the interval, want 2", renews)
	}
}

func TestReAddedSandboxDoesNotReviveOldTokens(t *testing.T) {
	up := newUpstream(t, nil)
	routes := claudeRoutes(t, up)
	r := newRig(t, up, routes)
	old := r.mint("brig-a", "anthropic")
	r.h.RemoveSandbox("brig-a")
	if err := r.h.AddSandbox(Sandbox{ID: "brig-a", Profile: "claude-code", Routes: routes}); err != nil {
		t.Fatal(err)
	}
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(old), "{}")
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "status=401", "check=signature")
	resp, _ = r.do("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")), "{}")
	wantStatus(t, resp, http.StatusOK)
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

func TestAuditHoldsNoSecret(t *testing.T) {
	const bodyMarker = "body-marker-0c6f2b"
	up := newUpstream(t, refuseStale)
	routes := append(claudeRoutes(t, up),
		customRoute(t, "inventory", up.URL, &v1alpha1.RequestAllow{Methods: []string{"GET"}}))
	r := newRig(t, up, routes)
	tokA := r.mint("brig-a", "anthropic")
	tokB := r.mint("brig-b", "anthropic")
	tokInv := r.mint("brig-a", "inventory")
	gh := http.Request{Header: http.Header{}}
	gh.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
	withJunk := bearer(tokA)
	withJunk.Set("X-Api-Key", "junk-guest-api-key")
	withJunk.Set("Anthropic-Beta", "unlisted-beta-2099-01-01")

	r.do("POST", "/brig-a/r/anthropic/v1/messages", withJunk, `{"prompt":"`+bodyMarker+`"}`)
	r.do("POST", "/brig-b/r/anthropic/v1/messages", bearer(tokA), bodyMarker)
	r.do("POST", "/brig-a/r/inventory/v2/items", bearer(tokInv), bodyMarker)
	r.do("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs", gh.Header, bodyMarker)
	r.creds.mu.Lock()
	r.creds.err = errors.New("read failed for " + fakeGHToken)
	r.creds.mu.Unlock()
	r.do("POST", "/brig-b/r/anthropic/v1/messages", bearer(tokB), bodyMarker)

	log := r.audit.String()
	if n := strings.Count(log, "\n"); n != 5 {
		t.Fatalf("%d audit lines, want 5:\n%s", n, log)
	}
	for _, secret := range []string{
		fakeAccess, fakeFresh, fakeAPIKey, fakeGHToken, "fake-inventory-token", "fake-refresh",
		tokA, tokB, tokInv, bodyMarker, "junk-guest-api-key", "unlisted-beta",
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeGHToken)),
	} {
		if strings.Contains(log, secret) {
			t.Errorf("the audit log holds %q:\n%s", secret, log)
		}
	}
	// A token can be split by the logger's quoting, so look for each half.
	for _, tok := range []string{tokA, tokB, tokInv} {
		body, mac, _ := strings.Cut(tok, ".")
		if strings.Contains(log, body) || strings.Contains(log, mac) {
			t.Errorf("the audit log holds part of a session token:\n%s", log)
		}
	}
}

func TestConcurrentRequestsAndRegistration(t *testing.T) {
	up := newUpstream(t, nil)
	routes := claudeRoutes(t, up)
	r := newRig(t, up, routes)
	toks := map[string]string{"brig-a": r.mint("brig-a", "anthropic"), "brig-b": r.mint("brig-b", "anthropic")}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		sb := "brig-a"
		if i%2 == 1 {
			sb = "brig-b"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				req, _ := http.NewRequest("POST", r.srv.URL+"/"+sb+"/r/anthropic/v1/messages", strings.NewReader("{}"))
				req.Header = bearer(toks[sb])
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("%s: status %d", sb, resp.StatusCode)
				}
			}
		}()
	}
	// brig-c comes and goes while its own token is in use. A request finds it
	// registered with the key of its token, registered with another key, or
	// gone.
	var tokC atomic.Value
	tokC.Store("")
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for j := 0; j < 50; j++ {
			if err := r.h.AddSandbox(Sandbox{ID: "brig-c", Routes: routes}); err != nil {
				errs <- err
				return
			}
			tok, err := r.h.Mint("brig-c", "", "anthropic", time.Hour)
			if err != nil {
				errs <- err
				return
			}
			tokC.Store(tok)
			time.Sleep(time.Millisecond)
			r.h.RemoveSandbox("brig-c")
		}
	}()
	var statuses sync.Map
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest("POST", r.srv.URL+"/brig-c/r/anthropic/v1/messages", strings.NewReader("{}"))
				req.Header = bearer(tokC.Load().(string))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				statuses.Store(resp.StatusCode, true)
				switch resp.StatusCode {
				case http.StatusOK, http.StatusUnauthorized, http.StatusNotFound:
				default:
					errs <- fmt.Errorf("brig-c: status %d", resp.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
	if _, ok := statuses.Load(http.StatusOK); !ok {
		t.Error("no request to brig-c found it registered")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestAddSandboxRefusesDuplicateRoutes(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: &fakeCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	rt := customRoute(t, "inventory", "https://example.com", nil)
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{rt, rt}}); err == nil {
		t.Fatal("AddSandbox took two routes with one name")
	}
	if err := h.AddSandbox(Sandbox{ID: "brig/a", Routes: []Route{rt}}); err == nil {
		t.Fatal("AddSandbox took a sandbox id with a slash")
	}
}

func TestCredentialFieldIsFoundAtAnyDepth(t *testing.T) {
	for _, tc := range []struct {
		field, doc, want string
	}{
		{"", "plain-value", "plain-value"},
		{"token", `{"token":"a"}`, "a"},
		{"token", `{"wrap":{"token":"b"}}`, "b"},
		{"claudeAiOauth.accessToken", `{"claudeAiOauth":{"accessToken":"c"}}`, "c"},
		{"claudeAiOauth.accessToken", `{"outer":{"claudeAiOauth":{"accessToken":"d"}}}`, "d"},
		{"a.b", `{"a.b":"e"}`, "e"},
		// The path from the root wins over a key that holds the dotted
		// name further down.
		{"claudeAiOauth.accessToken",
			`{"claudeAiOauth":{"accessToken":"REAL"},"mcpOAuth":{"srv|1":{"claudeAiOauth.accessToken":"OTHER"}}}`, "REAL"},
		{"token", `{"a":{"token":"same"},"b":{"token":"same"}}`, "same"},
	} {
		got, err := credentialValue(&v1alpha1.CredentialRef{Field: tc.field}, []byte(tc.doc))
		if err != nil || got != tc.want {
			t.Errorf("field %q of %s = %q, %v; want %q", tc.field, tc.doc, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ field, doc string }{
		{"token", `{"other":"a"}`},
		{"token", `{"token":1}`},
		{"token", `not json`},
		{"", "two\nlines"},
		{"", ""},
		{"claudeAiOauth.accessToken", `{"claudeAiOauth":"flat"}`},
		// Two different values at depth: map order would pick one.
		{"accessToken", `{"claudeAiOauth":{"accessToken":"REAL"},"mcpOAuth":{"srv|1":{"accessToken":"OTHER"}}}`},
	} {
		if got, err := credentialValue(&v1alpha1.CredentialRef{Field: tc.field}, []byte(tc.doc)); err == nil {
			t.Errorf("field %q of %q = %q, want an error", tc.field, tc.doc, got)
		}
	}
}

func TestCredentialDropsOneTrailingNewline(t *testing.T) {
	for _, tc := range []struct{ field, doc, want string }{
		{"", "key\n", "key"},
		{"", "key\r\n", "key"},
		{"token", `{"token":"a\n"}`, "a"},
	} {
		got, err := credentialValue(&v1alpha1.CredentialRef{Field: tc.field}, []byte(tc.doc))
		if err != nil || got != tc.want {
			t.Errorf("field %q of %q = %q, %v; want %q", tc.field, tc.doc, got, err, tc.want)
		}
	}
	for _, doc := range []string{"key\n\n", "key\r", "key\n\r\n", "a\nb\n", "\n", "\r\n"} {
		if got, err := credentialValue(&v1alpha1.CredentialRef{}, []byte(doc)); err == nil {
			t.Errorf("credential %q = %q, want an error", doc, got)
		}
	}

	// A key saved from a file, as brig secret create -f stores it.
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{
		presetRoute(t, v1alpha1.PresetAnthropicAPIKey, "anthropic", "anthropic-api-key", up.URL, nil),
	})
	r.creds.mu.Lock()
	r.creds.docs["anthropic-api-key"] = []byte(fakeAPIKey + "\n")
	r.creds.mu.Unlock()
	resp, _ := r.do("POST", "/brig-a/r/anthropic/v1/messages", http.Header{"X-Api-Key": {r.mint("brig-a", "anthropic")}}, "{}")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).header.Get("X-Api-Key"); got != fakeAPIKey {
		t.Errorf("upstream x-api-key = %q, want %q", got, fakeAPIKey)
	}
}

func TestBrokerCannotReachHostSourceOrRefresher(t *testing.T) {
	// Each GOOS brig ships for: CI runs on Linux alone, and a file built
	// only for macOS can add an import.
	var deps []string
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command("go", "list", "-deps", "github.com/brig-sh/brig/internal/broker")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps: %v", goos, err)
		}
		deps = append(deps, strings.Split(string(out), "\n")...)
	}
	for _, forbidden := range []string{
		// A request path must not read a host credential source.
		"github.com/brig-sh/brig/internal/hostsrc",
		// The broker reaches the refresher through secret.Broker and
		// Renewer, which the hosting process wires to a renew.Cache.
		"github.com/brig-sh/brig/internal/renew",
	} {
		if slices.Contains(deps, forbidden) {
			t.Errorf("internal/broker depends on %s", forbidden)
		}
	}
}

// publicRoute is a custom route that allows /v2/public/ and nothing else.
func publicRoute(t *testing.T, up *upstream) []Route {
	return []Route{customRoute(t, "inventory", up.URL,
		&v1alpha1.RequestAllow{Paths: []string{"/v2/public/*", "/v2/public/*/*"}})}
}

// %25%32%65 decodes once to %2e, which no escape the broker looks for
// matches. An upstream that decodes the path twice reads .. there, and
// leaves the allowed tree. A decoded % is refused on every route.
func TestAnEscapedPercentIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, publicRoute(t, up))
	for _, p := range []string{
		"/v2/public/%25%32%65%25%32%65/admin",
		"/v2/public/%25%32%45%25%32%45/admin",
		"/v2/public/%25%32%66admin",
		"/v2/public/100%25",
	} {
		t.Run(p, func(t *testing.T) {
			resp, _ := r.do("GET", "/brig-a/r/inventory"+p, bearer(r.mint("brig-a", "inventory")), "")
			wantStatus(t, resp, http.StatusBadRequest)
			wantAudit(t, r.lastAudit(), "check=path")
		})
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	resp, _ := r.do("GET", "/brig-a/r/inventory/v2/public/a/b", bearer(r.mint("brig-a", "inventory")), "")
	wantStatus(t, resp, http.StatusOK)
}

// An upstream that normalizes Unicode, to NFKC for one, reads a fullwidth
// solidus as /, so ．．／admin under /v2/public/ is ../admin there. The code
// points that fold to /, \ or %, the slash and backslash look-alikes, and a
// segment of characters that fold to dots are refused on every route.
func TestUnicodeLookalikesAreRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, publicRoute(t, up))
	esc := func(s string) string {
		var b strings.Builder
		for _, c := range []byte(s) {
			fmt.Fprintf(&b, "%%%02X", c)
		}
		return b.String()
	}
	var paths []string
	for _, c := range []rune{
		'／', '＼', '﹨', '％', '﹪', '℀', '℁', '℅', '℆',
		'∕', '⁄', '⧸', '⧵', '⧹', '∖',
	} {
		paths = append(paths, "/v2/public/"+esc("．．"+string(c))+"admin")
	}
	for _, c := range []rune{'․', '﹒', '．', '。', '｡', '‥', '︰', '…', '︙'} {
		paths = append(paths, "/v2/public/"+esc(string(c))+"/admin", "/v2/public/."+esc(string(c))+"/admin")
	}
	// A semicolon that folds to ; starts a parameter that such an upstream
	// strips, so ..；x reads as .. there. U+037E looks like ; and folds
	// under NFC too.
	for _, c := range []rune{'；', '﹔', '︔', '\u037e'} {
		paths = append(paths, "/v2/public/.."+esc(string(c))+"x/admin", "/v2/public/"+esc("．．"+string(c))+"/admin")
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			resp, _ := r.do("GET", "/brig-a/r/inventory"+p, bearer(r.mint("brig-a", "inventory")), "")
			wantStatus(t, resp, http.StatusBadRequest)
			wantAudit(t, r.lastAudit(), "check=path")
		})
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	// Other characters outside ASCII still pass on a route without repos.
	resp, _ := r.do("GET", "/brig-a/r/inventory/v2/public/caf%C3%A9", bearer(r.mint("brig-a", "inventory")), "")
	wantStatus(t, resp, http.StatusOK)
}
