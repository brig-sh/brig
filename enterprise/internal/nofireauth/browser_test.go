// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
)

func ptr[T any](v T) *T { return &v }

// browser plays the user's browser. Run calls the opener on another
// goroutine, so what the browser saw is kept under a lock.
type browser struct {
	t    *testing.T
	wg   sync.WaitGroup
	done chan struct{} // closed when the opener's visit has finished

	mu     sync.Mutex
	paths  []string   // what the opener was given
	opened []string   // the URL each of those pages redirects to
	landed []*url.URL // the URL each visit ended on, after redirects
	pages  []string
}

func newBrowser(t *testing.T) *browser { return &browser{t: t, done: make(chan struct{})} }

// wait blocks until the opener's visit has finished. Run does not wait for
// the opener, so a test waits before it reads what the browser saw.
func (b *browser) wait() {
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		b.t.Fatal("the browser visit did not finish")
	}
}

// visit GETs raw, following redirects, as a browser would.
func (b *browser) visit(raw string) (int, *url.URL) {
	resp, err := http.Get(raw)
	if err != nil {
		b.t.Errorf("visit: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	b.mu.Lock()
	b.landed = append(b.landed, resp.Request.URL)
	b.pages = append(b.pages, string(body))
	b.mu.Unlock()
	return resp.StatusCode, resp.Request.URL
}

// opener returns an Opener that reads the page it is given, records the
// URL the page redirects to, runs before (if set), and then approves by
// visiting the URL.
func (b *browser) opener(before func(authURL string)) nofireauth.Opener {
	return func(path string) error {
		defer close(b.done)
		authURL := follow(b.t, path)
		b.mu.Lock()
		b.paths = append(b.paths, path)
		b.opened = append(b.opened, authURL)
		b.mu.Unlock()
		if before != nil {
			before(authURL)
		}
		b.visit(authURL)
		return nil
	}
}

func (b *browser) authURL() *url.URL {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.opened) != 1 {
		b.t.Fatalf("opened %d URLs, want 1", len(b.opened))
	}
	u, err := url.Parse(b.opened[0])
	if err != nil {
		b.t.Fatal(err)
	}
	return u
}

// codes returns the authorization codes the browser carried to Brig.
func (b *browser) codes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, u := range b.landed {
		if c := u.Query().Get("code"); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// follow returns the URL the page at path redirects to, as a browser that
// opened it would.
func follow(t *testing.T, path string) string {
	t.Helper()
	u, err := fakeas.FollowPage(path)
	if err != nil {
		t.Error(err)
	}
	return u
}

func redirectOf(t *testing.T, authURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Error(err)
		return nil
	}
	r, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil {
		t.Error(err)
		return nil
	}
	return r
}

func browserFlow(t *testing.T, base string, open nofireauth.Opener) (nofireauth.BrowserFlow, *bytes.Buffer) {
	t.Helper()
	c := client(t, base)
	m, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return nofireauth.BrowserFlow{Client: c, Meta: m, Open: open, Out: out, Timeout: 30 * time.Second}, out
}

func TestBrowserFlowSignsIn(t *testing.T) {
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(nil))
	g, err := f.Run(context.Background())
	b.wait()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Issued(), g.AccessToken) || g.AccessToken == "" {
		t.Errorf("token %q is not one the server issued (%q)", g.AccessToken, s.Issued())
	}
	if g.ExpiresIn != 7*24*time.Hour {
		t.Errorf("ExpiresIn = %v, want the server default of 7 days", g.ExpiresIn)
	}
	q := b.authURL().Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "brig" {
		t.Errorf("authorization query = %v", q)
	}
	if r := redirectOf(t, b.authURL().String()); r == nil || r.Hostname() != "127.0.0.1" || r.Path != "/callback" {
		t.Errorf("redirect_uri = %v, want http://127.0.0.1:<port>/callback", r)
	}
	if b.pages[0] == "" || !strings.Contains(b.pages[0], "Signed in") {
		t.Errorf("browser page = %q, want the success page", b.pages[0])
	}
}

func TestBrowserFlowSendsTTL(t *testing.T) {
	for _, tc := range []struct {
		ttl  time.Duration
		want []int
	}{
		{30 * 24 * time.Hour, []int{2592000}},
		{0, nil},
	} {
		s := start(t, fakeas.Options{})
		b := newBrowser(t)
		f, _ := browserFlow(t, s.URL, b.opener(nil))
		f.TTL = tc.ttl
		_, err := f.Run(context.Background())
		b.wait()
		if err != nil {
			t.Fatal(err)
		}
		if got := s.RequestedTTL(); !slices.Equal(got, tc.want) {
			t.Errorf("TTL %v: server saw ttl %v, want %v", tc.ttl, got, tc.want)
		}
		if tc.ttl == 0 && b.authURL().Query().Has("ttl") {
			t.Errorf("TTL 0 sent a ttl parameter: %s", b.authURL())
		}
	}
}

func TestWrongStateIsIgnored(t *testing.T) {
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(func(authURL string) {
		r := redirectOf(t, authURL)
		r.RawQuery = url.Values{"state": {"wrong"}, "code": {"x"}, "iss": {s.URL}}.Encode()
		if status, _ := b.visit(r.String()); status != http.StatusBadRequest {
			t.Errorf("wrong state: status %d, want 400", status)
		}
	}))
	g, err := f.Run(context.Background())
	b.wait()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Issued(), g.AccessToken) {
		t.Errorf("token %q was not issued", g.AccessToken)
	}
}

func TestDenyEndsTheWait(t *testing.T) {
	s := start(t, fakeas.Options{Approval: fakeas.Deny})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(nil))
	_, err := f.Run(context.Background())
	b.wait()
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want access_denied", err)
	}
	if n := len(s.Issued()); n != 0 {
		t.Errorf("%d tokens issued", n)
	}
}

func testIssRefused(t *testing.T, iss string) {
	s := start(t, fakeas.Options{CallbackIss: iss})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(nil))
	_, err := f.Run(context.Background())
	b.wait()
	if err == nil || !strings.Contains(err.Error(), "RFC 9207") {
		t.Fatalf("err = %v, want a refusal citing RFC 9207", err)
	}
	if n := len(s.Issued()); n != 0 {
		t.Errorf("%d tokens issued", n)
	}
	if slices.Contains(s.Requests(), "POST /oauth/token") {
		t.Error("the code was sent to the token endpoint")
	}
	if !strings.Contains(b.pages[0], "failed") {
		t.Errorf("browser page = %q, want the failure page", b.pages[0])
	}
}

func TestMissingIssRefused(t *testing.T) { testIssRefused(t, "-") }

func TestWrongIssRefused(t *testing.T) { testIssRefused(t, "https://evil.example") }

func TestOnlyCallbackPathServed(t *testing.T) {
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(func(authURL string) {
		r := redirectOf(t, authURL)
		r.Path = "/other"
		if status, _ := b.visit(r.String()); status != http.StatusNotFound {
			t.Errorf("GET /other: status %d, want 404", status)
		}
	}))
	_, err := f.Run(context.Background())
	b.wait()
	if err != nil {
		t.Fatal(err)
	}
}

// nonLoopbackAddr returns an address of this host that is not loopback and
// not link-local (which needs a zone to dial), or "".
func nonLoopbackAddr() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && !n.IP.IsUnspecified() {
			return n.IP.String()
		}
	}
	return ""
}

func TestListenerIsLoopbackOnly(t *testing.T) {
	addr := nonLoopbackAddr()
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(func(authURL string) {
		r := redirectOf(t, authURL)
		if r.Hostname() != "127.0.0.1" {
			t.Errorf("redirect host = %q, want 127.0.0.1", r.Hostname())
		}
		if addr == "" {
			return
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, r.Port()), 2*time.Second)
		if err == nil {
			c.Close()
			t.Errorf("the callback port answers on %s", addr)
		}
	}))
	_, err := f.Run(context.Background())
	b.wait()
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		t.Skip("this host has no non-loopback address to dial")
	}
}

func TestCancelLeavesNothing(t *testing.T) {
	s := start(t, fakeas.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var port string
	var page string
	open := func(path string) error {
		mu.Lock()
		page = path
		port = redirectOf(t, follow(t, path)).Port()
		mu.Unlock()
		cancel()
		return nil
	}
	f, _ := browserFlow(t, s.URL, open)
	began := time.Now()
	_, err := f.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("Run took %v to return after the cancel", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Error("the callback port is still open after Run returned")
	}
	if _, err := os.Stat(filepath.Dir(page)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the page's directory %s is still there after Run returned: %v", filepath.Dir(page), err)
	}
	if n := len(s.Issued()); n != 0 {
		t.Errorf("%d tokens issued", n)
	}
}

func TestOpenerFailureKeepsWaiting(t *testing.T) {
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	var authURL string
	open := func(path string) error {
		u := follow(t, path)
		authURL = u
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			time.Sleep(50 * time.Millisecond)
			b.visit(u)
		}()
		return errors.New("no browser on this host")
	}
	f, out := browserFlow(t, s.URL, open)
	g, err := f.Run(context.Background())
	b.wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Issued(), g.AccessToken) {
		t.Errorf("token %q was not issued", g.AccessToken)
	}
	for _, want := range []string{authURL, "If this host has no browser, run brig login --device instead"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
}

func TestUnusableLifetimeRevoked(t *testing.T) {
	s := start(t, fakeas.Options{ExpiresIn: ptr(0)})
	b := newBrowser(t)
	f, _ := browserFlow(t, s.URL, b.opener(nil))
	_, err := f.Run(context.Background())
	b.wait()
	if err == nil || !strings.Contains(err.Error(), "its lifetime") {
		t.Fatalf("err = %v, want a refusal naming the lifetime", err)
	}
	issued := s.Issued()
	if len(issued) != 1 || !slices.Equal(s.Revoked(), issued) {
		t.Errorf("issued %q, revoked %q; want the issued token revoked", issued, s.Revoked())
	}
}

// recordingProxy stands in front of a fake and records every form value sent
// to it, so a test can learn the PKCE verifier Brig sent.
type recordingProxy struct {
	*httptest.Server
	mu     sync.Mutex
	target *url.URL
	forms  []url.Values
}

func newRecordingProxy(t *testing.T) *recordingProxy {
	p := &recordingProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(body))
		p.mu.Lock()
		p.forms = append(p.forms, f)
		target := p.target
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *recordingProxy) values(key string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, f := range p.forms {
		out = append(out, f[key]...)
	}
	return out
}

func TestSecretsNotInErrorsOrArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    fakeas.Options
	}{
		{"deny", fakeas.Options{Approval: fakeas.Deny}},
		{"missing iss", fakeas.Options{CallbackIss: "-"}},
		{"wrong iss", fakeas.Options{CallbackIss: "https://evil.example"}},
		{"unusable lifetime", fakeas.Options{ExpiresIn: ptr(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRecordingProxy(t)
			tc.o.Issuer, tc.o.EndpointBase = p.URL, p.URL
			s := start(t, tc.o)
			p.mu.Lock()
			p.target, _ = url.Parse(s.URL)
			p.mu.Unlock()

			b := newBrowser(t)
			f, out := browserFlow(t, p.URL, b.opener(nil))
			_, err := f.Run(context.Background())
			b.wait()
			if err == nil {
				t.Fatal("Run succeeded")
			}
			state := b.authURL().Query().Get("state")
			var secrets []string
			secrets = append(secrets, b.codes()...)
			secrets = append(secrets, p.values("code_verifier")...)
			secrets = append(secrets, s.Issued()...)
			if tc.name == "unusable lifetime" && len(secrets) != 3 {
				t.Fatalf("collected %d secrets, want a code, a verifier and a token", len(secrets))
			}
			for _, sec := range secrets {
				if sec == "" {
					continue
				}
				if strings.Contains(err.Error(), sec) {
					t.Errorf("error %q contains a secret", err)
				}
				if strings.Contains(out.String(), sec) {
					t.Errorf("output %q contains a secret", out)
				}
			}
			if state == "" || strings.Contains(err.Error(), state) {
				t.Errorf("error %q contains the state", err)
			}
		})
	}

	t.Run("SystemOpener argv", func(t *testing.T) {
		// Stubs on PATH record their name and every argument, one per
		// line, so one argument holding the whole URL, and no shell, is
		// what passes.
		dir := t.TempDir()
		argv := filepath.Join(dir, "argv")
		for _, bin := range []string{"open", "xdg-open"} {
			script := "#!/bin/sh\nprintf '%s\\n' " + bin + " \"$#\" \"$@\" > " + argv + "\n"
			if err := os.WriteFile(filepath.Join(dir, bin), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", dir)
		const u = "https://my.nofire.ai/oauth/authorize?a=1&b=$(id);c='d e'"
		for goos, bin := range map[string]string{"darwin": "open", "linux": "xdg-open"} {
			os.Remove(argv)
			if err := nofireauth.SystemOpener(goos)(u); err != nil {
				t.Fatalf("%s: %v", goos, err)
			}
			got, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			if want := bin + "\n1\n" + u + "\n"; string(got) != want {
				t.Errorf("%s: argv = %q, want %q", goos, got, want)
			}
		}
	})
}

// tokenProxy stands in front of a fake and passes every request through. The
// fake's answer to a token request goes to answer, which writes what Brig
// reads, so a test can act after the fake has issued a token and before Brig
// sees it.
type tokenProxy struct {
	*httptest.Server
	mu     sync.Mutex
	target *url.URL
}

func newTokenProxy(t *testing.T, answer func(w http.ResponseWriter, fake *httptest.ResponseRecorder)) *tokenProxy {
	p := &tokenProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		rp := httputil.NewSingleHostReverseProxy(p.target)
		p.mu.Unlock()
		if r.URL.Path != "/oauth/token" {
			rp.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		rp.ServeHTTP(rec, r)
		answer(w, rec)
	}))
	t.Cleanup(p.Close)
	return p
}

// front starts a fake whose metadata names the proxy.
func (p *tokenProxy) front(t *testing.T, o fakeas.Options) *fakeas.Server {
	t.Helper()
	o.Issuer, o.EndpointBase = p.URL, p.URL
	s := start(t, o)
	p.mu.Lock()
	p.target, _ = url.Parse(s.URL)
	p.mu.Unlock()
	return s
}

// relay writes the fake's answer unchanged.
func relay(w http.ResponseWriter, fake *httptest.ResponseRecorder) {
	for k, v := range fake.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(fake.Code)
	_, _ = w.Write(fake.Body.Bytes())
}

// Ctrl-C after the deployment issued the token, while its answer is still
// on the way. A cancelled exchange would drop a token that is live on the
// server; Run returns it instead, so the caller can revoke it.
func TestExchangeOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTokenProxy(t, func(w http.ResponseWriter, fake *httptest.ResponseRecorder) {
		cancel()
		relay(w, fake)
	})
	s := p.front(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, p.URL, b.opener(nil))
	g, err := f.Run(ctx)
	b.wait()
	if ctx.Err() == nil {
		t.Fatal("the token request was not made")
	}
	issued := s.Issued()
	if err != nil || len(issued) != 1 || g.AccessToken != issued[0] {
		t.Fatalf("Run = %v, %v; issued %d: want the issued token back", g.AccessToken != "", err, len(issued))
	}
}

// expires_in must be a positive whole number of seconds. Anything else is
// refused, and a token that came with it is revoked: Brig cannot say when it
// expires.
func TestUnusableLifetimesRevoked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn any // nil leaves expires_in out
	}{
		{"missing", nil},
		{"zero", 0},
		{"negative", -1},
		{"fractional", 1.5},
		{"string", "3600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTokenProxy(t, func(w http.ResponseWriter, fake *httptest.ResponseRecorder) {
				var body map[string]any
				if err := json.Unmarshal(fake.Body.Bytes(), &body); err != nil {
					t.Errorf("the fake's token response: %v", err)
				}
				delete(body, "expires_in")
				if tc.expiresIn != nil {
					body["expires_in"] = tc.expiresIn
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			})
			s := p.front(t, fakeas.Options{})
			b := newBrowser(t)
			f, _ := browserFlow(t, p.URL, b.opener(nil))
			g, err := f.Run(context.Background())
			b.wait()
			if err == nil || g.AccessToken != "" || !strings.Contains(err.Error(), "its lifetime") {
				t.Fatalf("token returned: %v, err = %v; want a refusal naming the lifetime", g.AccessToken != "", err)
			}
			issued := s.Issued()
			if len(issued) != 1 || !slices.Equal(s.Revoked(), issued) {
				t.Errorf("issued %d, revoked %d; want the issued token revoked", len(issued), len(s.Revoked()))
			}
		})
	}
}

// An authorization endpoint on another origin would show the user a page
// Brig cannot vouch for. It is refused before the browser is opened.
func TestRunRefusesOffOriginAuthorizationEndpoint(t *testing.T) {
	s := start(t, fakeas.Options{})
	opened := 0
	f, out := browserFlow(t, s.URL, func(string) error { opened++; return nil })
	f.Meta.AuthorizationEndpoint = "https://evil.example/oauth/authorize"
	_, err := f.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sends the sign-in page to another address") {
		t.Fatalf("err = %v, want the origin refusal", err)
	}
	if opened != 0 || out.Len() != 0 {
		t.Errorf("opened %d times, printed %q; want neither", opened, out)
	}
	if n := len(s.Issued()); n != 0 {
		t.Errorf("%d tokens issued", n)
	}
}

// A second response with the right state, arriving while the first one's
// code is being exchanged, is refused: only one code is ever redeemed.
func TestSecondCallbackIsRefused(t *testing.T) {
	var mu sync.Mutex
	var authURL, second string
	p := newTokenProxy(t, func(w http.ResponseWriter, fake *httptest.ResponseRecorder) {
		mu.Lock()
		defer mu.Unlock()
		u, _ := url.Parse(authURL)
		cb := redirectOf(t, authURL)
		q := url.Values{"state": {u.Query().Get("state")}, "code": {"another"}, "iss": {"x"}}
		cb.RawQuery = q.Encode()
		// Bounded: a handler that queued this callback would hold it
		// until Run returns, and Run is waiting for this exchange.
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(cb.String())
		if err != nil {
			t.Errorf("second callback: %v", err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			second = string(body)
		}
		relay(w, fake)
	})
	s := p.front(t, fakeas.Options{})
	b := newBrowser(t)
	f, _ := browserFlow(t, p.URL, b.opener(func(u string) {
		mu.Lock()
		authURL = u
		mu.Unlock()
	}))
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.wait()
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(second, "already received") {
		t.Errorf("second callback got %q, want the already-received page", second)
	}
	var posts int
	for _, r := range s.Requests() {
		if r == "POST /oauth/token" {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d token requests, want 1", posts)
	}
}

// The opener gets the path of a page only the user can read, not the URL:
// an opener's command line is readable by other users, and the URL holds the
// state and the PKCE challenge. The page is gone once Run returns.
func TestOpenerGetsAPrivatePageNotTheURL(t *testing.T) {
	s := start(t, fakeas.Options{})
	b := newBrowser(t)
	var fileMode, dirMode os.FileMode
	var page []byte
	f, out := browserFlow(t, s.URL, b.opener(func(string) {
		b.mu.Lock()
		path := b.paths[0]
		b.mu.Unlock()
		if fi, err := os.Stat(path); err == nil {
			fileMode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Dir(path)); err == nil {
			dirMode = fi.Mode().Perm()
		}
		page, _ = os.ReadFile(path)
	}))
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.wait()
	authURL := b.authURL()
	b.mu.Lock()
	path := b.paths[0]
	b.mu.Unlock()
	state := authURL.Query().Get("state")
	challenge := authURL.Query().Get("code_challenge")
	if !filepath.IsAbs(path) || state == "" || strings.Contains(path, state) || strings.Contains(path, challenge) {
		t.Errorf("the opener got %q, want an absolute path that holds no state or challenge", path)
	}
	if fileMode != 0o600 || dirMode != 0o700 {
		t.Errorf("page mode %v, directory mode %v; want 0600 and 0700", fileMode, dirMode)
	}
	if !strings.Contains(string(page), html.EscapeString(authURL.String())) {
		t.Errorf("page %q does not hold the escaped URL %s", page, authURL)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the page's directory is still there after Run returned: %v", err)
	}
	// The URL is still printed: the user needs it when no browser opens.
	if !strings.Contains(out.String(), "Opening "+authURL.String()+" in your browser.") {
		t.Errorf("out = %q, want the URL", out)
	}
}

// A quote or an angle bracket in the URL cannot end the attribute it is in.
func TestRedirectPageEscapesTheURL(t *testing.T) {
	const u = `https://h/a?x="><script>alert(1)</script>&y='1'`
	page := string(nofireauth.RedirectHTML(u))
	if strings.Contains(page, `"><script>`) || strings.Contains(page, "&y") {
		t.Errorf("page holds the URL unescaped: %s", page)
	}
	if !strings.Contains(page, html.EscapeString(u)) {
		t.Errorf("page does not hold the escaped URL: %s", page)
	}
	path := filepath.Join(t.TempDir(), "p.html")
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := follow(t, path); got != u {
		t.Errorf("the page redirects to %q, want %q", got, u)
	}
}
