// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package fakeas

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

const verifier = "0123456789012345678901234567890123456789012" // 43 characters

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func start(t *testing.T, o Options) *Server {
	t.Helper()
	s, err := Start("127.0.0.1:0", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func authorize(t *testing.T, s *Server, mod func(url.Values)) *http.Response {
	t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {"brig"},
		"redirect_uri":   {"http://127.0.0.1:4000/callback"},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"state": {"st"},
	}
	if mod != nil {
		mod(q)
	}
	resp, err := noRedirect.Get(s.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func exchange(t *testing.T, s *Server, code, v string) int {
	t.Helper()
	resp, err := http.PostForm(s.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {"brig"}, "code": {code},
		"redirect_uri": {"http://127.0.0.1:4000/callback"}, "code_verifier": {v},
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestFakeRefusesPlainChallenge(t *testing.T) {
	s := start(t, Options{})
	resp := authorize(t, s, func(q url.Values) { q.Set("code_challenge_method", "plain") })
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("status %d, Location %q; want an error page", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestFakeRefusesNonLoopbackRedirect(t *testing.T) {
	s := start(t, Options{})
	for _, uri := range []string{"https://evil.example/callback", "http://localhost:4000/callback", "http://127.0.0.1:4000/other", "http://127.0.0.1/callback"} {
		resp := authorize(t, s, func(q url.Values) { q.Set("redirect_uri", uri) })
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%s: status %d, Location %q; want an error page", uri, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestFakeCodeIsSingleUseAndBoundToTheVerifier(t *testing.T) {
	s := start(t, Options{})
	code := func() string {
		resp := authorize(t, s, nil)
		u, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || u.Query().Get("state") != "st" || u.Query().Get("iss") != s.URL {
			t.Fatalf("Location %q", resp.Header.Get("Location"))
		}
		return u.Query().Get("code")
	}
	c := code()
	if st := exchange(t, s, c, verifier); st != 200 {
		t.Fatalf("good exchange: %d", st)
	}
	if st := exchange(t, s, c, verifier); st != 400 {
		t.Errorf("reused code: %d, want 400", st)
	}
	if st := exchange(t, s, code(), strings.Repeat("x", 43)); st != 400 {
		t.Errorf("wrong verifier: %d, want 400", st)
	}
	if len(s.Issued()) != 1 {
		t.Errorf("issued %d tokens, want 1", len(s.Issued()))
	}
}

func TestFakeDeviceSlowDownAndScriptedPolls(t *testing.T) {
	s := start(t, Options{Polls: []string{"authorization_pending"}, Interval: time.Hour})
	resp, err := http.PostForm(s.URL+"/oauth/device_authorization", url.Values{"client_id": {"brig"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	code := between(string(b), `"device_code":"`, `"`)
	poll := func() string {
		r, err := http.PostForm(s.URL+"/oauth/token", url.Values{"grant_type": {deviceGrant}, "client_id": {"brig"}, "device_code": {code}})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if got := poll(); !strings.Contains(got, "authorization_pending") {
		t.Errorf("first poll = %s", got)
	}
	if got := poll(); !strings.Contains(got, "slow_down") {
		t.Errorf("immediate second poll = %s, want slow_down", got)
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}

func TestFakeCallbackIssIsTheDiscoveredIssuerUnderAPrefix(t *testing.T) {
	s := start(t, Options{})
	resp, err := http.Get(s.URL + metadataPath + "/tenant")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	issuer := between(string(b), `"issuer":"`, `"`)
	if issuer != s.URL+"/tenant" {
		t.Fatalf("metadata issuer = %q", issuer)
	}
	loc, _ := url.Parse(authorize(t, s, nil).Header.Get("Location"))
	if got := loc.Query().Get("iss"); got != issuer {
		t.Errorf("iss = %q, want %q", got, issuer)
	}
}

func postAuthorize(t *testing.T, s *Server) *http.Response {
	t.Helper()
	post, err := noRedirect.PostForm(s.URL+"/oauth/authorize", url.Values{
		"response_type": {"code"}, "client_id": {"brig"},
		"redirect_uri":   {"http://127.0.0.1:4000/callback"},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"state": {"st"}, "decision": {"allow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { post.Body.Close() })
	return post
}

func TestFakePageApprovalNeedsAPostedDecision(t *testing.T) {
	s := start(t, Options{Approval: Page})
	resp := authorize(t, s, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<form") {
		t.Fatalf("GET: status %d, body %s", resp.StatusCode, b)
	}
	post := postAuthorize(t, s)
	loc, _ := url.Parse(post.Header.Get("Location"))
	if post.StatusCode != 302 || loc.Query().Get("code") == "" || loc.Query().Get("state") != "st" || loc.Query().Get("iss") == "" {
		t.Errorf("POST: status %d, Location %q", post.StatusCode, post.Header.Get("Location"))
	}
}

func TestFakeDenyIgnoresAPostedDecision(t *testing.T) {
	s := start(t, Options{Approval: Deny})
	if loc := postAuthorize(t, s).Header.Get("Location"); !strings.Contains(loc, "error=access_denied") {
		t.Errorf("Location %q, want access_denied", loc)
	}
}
