package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

const (
	vendAccess = "AKIAFAKEVENDEDKEY01"
	vendSecret = "fake-vended-secret-access-key"
	vendToken  = "fake-vended-session-token"
	vendDoc    = `{"mode":"fake-pointer"}`
)

// fakeVendor records what the broker asked it for.
type fakeVendor struct {
	mu             sync.Mutex
	calls          int
	sandbox, route string
	doc            []byte
	err            error
	expiration     time.Time
}

func (f *fakeVendor) Vend(_ context.Context, sandbox, route string, doc []byte) (VendedCredentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sandbox, f.route, f.doc = sandbox, route, doc
	if f.err != nil {
		return VendedCredentials{}, f.err
	}
	return VendedCredentials{AccessKeyID: vendAccess, SecretAccessKey: vendSecret, Token: vendToken,
		Expiration: f.expiration}, nil
}

func (f *fakeVendor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// vendRig is a broker with one aws route, whose secret holds a pointer
// document, and a fake vendor.
func vendRig(t *testing.T, opts ...rigOption) (*rig, *fakeVendor) {
	t.Helper()
	fv := &fakeVendor{expiration: time.Date(2026, 10, 15, 13, 0, 0, 0, time.FixedZone("x", 3600))}
	up := newUpstream(t, nil)
	routes := []Route{
		presetRoute(t, v1alpha1.PresetAWS, "aws", "aws", "", nil),
		presetRoute(t, v1alpha1.PresetGitHubToken, "github", "gh-token", up.URL, nil),
	}
	opts = append([]rigOption{func(c *Config) { c.Vendor = fv }}, opts...)
	r := newRig(t, up, routes, opts...)
	r.creds.docs["aws"] = []byte(vendDoc)
	return r, fv
}

func TestVendServesCredentials(t *testing.T) {
	r, fv := vendRig(t)
	resp, body := r.do("GET", "/brig-a/r/aws", http.Header{"Authorization": {r.mint("brig-a", "aws")}}, "")
	wantStatus(t, resp, http.StatusOK)
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	want := map[string]string{"AccessKeyId": vendAccess, "SecretAccessKey": vendSecret, "Token": vendToken,
		"Expiration": "2026-10-15T12:00:00Z"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := time.Parse(time.RFC3339, got["Expiration"]); err != nil {
		t.Errorf("Expiration: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fv.sandbox != "brig-a" || fv.route != "aws" || string(fv.doc) != vendDoc {
		t.Errorf("vendor saw sandbox %q route %q doc %q", fv.sandbox, fv.route, fv.doc)
	}
}

func TestVendAcceptsBearerAndRaw(t *testing.T) {
	r, fv := vendRig(t)
	tok := r.mint("brig-a", "aws")
	for _, h := range []http.Header{bearer(tok), {"Authorization": {tok}}} {
		resp, _ := r.do("GET", "/brig-a/r/aws", h, "")
		wantStatus(t, resp, http.StatusOK)
	}
	if fv.count() != 2 {
		t.Errorf("vendor called %d times, want 2", fv.count())
	}
	if n := r.h.sandbox("brig-a").placeholders.Load(); n != 0 {
		t.Errorf("the token in its carrier counted %d placeholder hits", n)
	}
}

func TestVendRefusesOtherSandboxToken(t *testing.T) {
	r, fv := vendRig(t)
	resp, _ := r.do("GET", "/brig-a/r/aws", http.Header{"Authorization": {r.mint("brig-b", "aws")}}, "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if fv.count() != 0 {
		t.Error("the vendor was called for a token of another sandbox")
	}
}

func TestVendRefusesPostAndSubpaths(t *testing.T) {
	r, fv := vendRig(t)
	h := http.Header{"Authorization": {r.mint("brig-a", "aws")}}
	resp, _ := r.do("POST", "/brig-a/r/aws", h, "")
	wantStatus(t, resp, http.StatusForbidden)
	resp, _ = r.do("GET", "/brig-a/r/aws/x", h, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp, _ = r.do("GET", "/brig-a/r/aws/", h, "")
	wantStatus(t, resp, http.StatusOK)
	if fv.count() != 1 {
		t.Errorf("vendor called %d times, want 1 (the trailing slash only)", fv.count())
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("a vend request reached the upstream transport")
	return nil, errors.New("must not forward")
}

func TestVendNeverForwards(t *testing.T) {
	r, _ := vendRig(t, func(c *Config) { c.Transport = failTransport{t} })
	resp, _ := r.do("GET", "/brig-a/r/aws", http.Header{"Authorization": {r.mint("brig-a", "aws")}}, "")
	wantStatus(t, resp, http.StatusOK)
}

func TestVendErrorText(t *testing.T) {
	r, fv := vendRig(t)
	h := http.Header{"Authorization": {r.mint("brig-a", "aws")}}

	fv.err = &VendError{Message: "run aws sso login --profile p"}
	resp, body := r.do("GET", "/brig-a/r/aws", h, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "brig broker: route aws: run aws sso login --profile p") {
		t.Errorf("body %q lacks the message", body)
	}
	wantAudit(t, r.lastAudit(), "check=vend")

	fv.err = errors.New("exec /secret/path: boom")
	resp, body = r.do("GET", "/brig-a/r/aws", h, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "see the broker log") || strings.Contains(body, "boom") {
		t.Errorf("body %q", body)
	}
}

func TestVendAuditHasNoSecret(t *testing.T) {
	r, _ := vendRig(t)
	resp, _ := r.do("GET", "/brig-a/r/aws", http.Header{"Authorization": {r.mint("brig-a", "aws")}}, "")
	wantStatus(t, resp, http.StatusOK)
	line := r.lastAudit()
	wantAudit(t, line, "vended="+vendAccess)
	for _, s := range []string{vendSecret, vendToken} {
		if strings.Contains(line, s) {
			t.Errorf("the audit line holds %q: %s", s, line)
		}
	}
}

func TestVendWithoutVendorIsBadGateway(t *testing.T) {
	r, _ := vendRig(t, func(c *Config) { c.Vendor = nil })
	resp, body := r.do("GET", "/brig-a/r/aws", http.Header{"Authorization": {r.mint("brig-a", "aws")}}, "")
	wantStatus(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "route aws vends credentials and this broker has no vendor") {
		t.Errorf("body %q", body)
	}
}

// A raw Authorization value is a carrier of the aws route alone: the AWS SDK
// sends the token that way, and no other route's tool does.
func TestRawAuthorizationOnlyOnVendRoutes(t *testing.T) {
	r, _ := vendRig(t)
	resp, _ := r.do("GET", "/brig-a/r/github/x", http.Header{"Authorization": {r.mint("brig-a", "github")}}, "")
	wantStatus(t, resp, http.StatusUnauthorized)
}

func TestAddSandboxVendNeedsNoUpstream(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: newFakeCreds()})
	if err != nil {
		t.Fatal(err)
	}
	err = h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{presetRoute(t, v1alpha1.PresetAWS, "aws", "aws", "", nil)}})
	if err != nil {
		t.Fatalf("an aws route with no upstream was refused: %v", err)
	}
}
