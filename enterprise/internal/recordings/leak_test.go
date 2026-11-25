// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"errors"
	"io/fs"
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

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
	"github.com/brig-sh/brig/internal/cli"
)

// The login token goes to the deployment and nowhere else. These tests run a
// recorded session with the sentinel as the token and look for it in every
// place it must not be. A test that finds nothing proves little unless the
// run did record, so each one asserts that it did.

// A recorded run does not put the token in what the child gets, in what is
// printed, in the files the spool leaves, or in what the deployment is sent
// as a recording.
func TestTheTokenIsNotInTheChildTheOutputOrTheSpool(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	seen := pipes(t)

	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	// The network fails for the whole drain, so the spool and the meta file
	// stay on disk for this test to read.
	for i := 0; i < 500; i++ {
		h.srv.FailNextPuts(503)
	}
	argv := []string{"sh", "-c", `echo "args: $0 $*"; env; ps -o command= -p $$`, "zero", "a1", "a2"}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=xterm"}
	wantArgv, wantEnv := slices.Clone(argv), slices.Clone(env)
	code, err := relay(argv, env)
	if err != nil || code != 0 {
		t.Fatalf("relay: %d %v", code, err)
	}

	// (i) the child's argv and environment, as handed over and as the child
	// saw them. The child printed its own environment and command line.
	if !slices.Equal(argv, wantArgv) || !slices.Equal(env, wantEnv) {
		t.Errorf("the relay changed its argv or environment:\n argv %q\n env  %q", argv, env)
	}
	for _, s := range append(slices.Clone(argv), env...) {
		if strings.Contains(s, recToken) {
			t.Errorf("the child was given the token in %q", s)
		}
	}
	for _, e := range os.Environ() {
		if strings.Contains(e, recToken) {
			t.Errorf("the token is in brig's own environment: %q", e)
		}
	}

	// (ii) every file the run left under the state directory.
	files := 0
	err = filepath.WalkDir(h.state, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), recToken) {
			t.Errorf("%s holds the token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Errorf("%d files under the state directory, want the spool and the meta file", files)
	}

	// (iii) everything brig printed: the terminal, stderr and the notices.
	stdout, stderr := seen()
	if !strings.Contains(stdout, "args: zero a1 a2") {
		t.Errorf("the child did not run as asked: %q", stdout)
	}
	for name, got := range map[string]string{"stdout": stdout, "stderr": stderr, "notice": h.notice.String()} {
		if strings.Contains(got, recToken) {
			t.Errorf("%s holds the token: %q", name, got)
		}
	}

	// What the deployment holds as the recording is the terminal output, and
	// the token was only ever the bearer header, which the fake checked.
	h.srv.ClearFailures()
	if rec := h.srv.Recording(h.srv.Keys()[0]); rec != nil && strings.Contains(string(rec.Raw), recToken) {
		t.Error("the recording holds the token")
	}
	if h.srv.BadAuth() != 0 {
		t.Errorf("%d requests without the bearer token", h.srv.BadAuth())
	}
}

// redirecting stands in for a deployment that redirects. It forwards what it
// is sent to the real fake, except that a request whose path ends in one of
// the suffixes is answered with a 307 to elsewhere.
type redirecting struct {
	front     *httptest.Server
	elsewhere *httptest.Server
	mu        sync.Mutex
	hits      []string // hosts and paths the elsewhere server was sent
	carried   []string // authorization headers the elsewhere server was sent
	mine      []string // hosts the front was sent, for the requests that carried the token
	redirect  func(r *http.Request) bool
}

func newRedirecting(t *testing.T, f *recordingtest.Server, redirect func(r *http.Request) bool) *redirecting {
	t.Helper()
	r := &redirecting{redirect: redirect}
	r.elsewhere = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, req.Host+req.URL.Path)
		r.carried = append(r.carried, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.elsewhere.Close)
	target, _ := url.Parse(f.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	r.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.Header.Get("Authorization"), recToken) {
			r.mu.Lock()
			r.mine = append(r.mine, req.Host)
			r.mu.Unlock()
		}
		if r.redirect(req) {
			http.Redirect(w, req, r.elsewhere.URL+req.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(r.front.Close)
	return r
}

func (r *redirecting) elsewhereHits() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hits), slices.Clone(r.carried)
}

// (iv) A deployment that redirects the open is not followed: the host it names
// is sent nothing, and the run is not recorded.
func TestARedirectOfTheOpenIsNotFollowed(t *testing.T) {
	h := newRecHost(t)
	httpClient = nil // the client the recorder builds for itself, not the fake's
	rd := newRedirecting(t, h.srv, func(*http.Request) bool { return true })
	t.Setenv(nofireauth.EnvURL, rd.front.URL)
	h.signInTo(t, rd.front.URL, recToken)

	if !declines(t, Attach(hullRun())) {
		t.Fatal("a run was recorded through a redirect")
	}
	if hits, carried := rd.elsewhereHits(); len(hits) != 0 {
		t.Errorf("the redirect was followed: %v carrying %v", hits, carried)
	}
	if h.srv.Requests() != 0 {
		t.Errorf("the real deployment was reached %d times", h.srv.Requests())
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Errorf("the state directory was created: %v", err)
	}
}

// (iv) A deployment that redirects the uploads mid-run is not followed either:
// the token goes only to the front's own host, every request that carried it
// was to that host, the run goes on and the recording stays on disk.
func TestARedirectOfAnUploadIsNotFollowed(t *testing.T) {
	h := newRecHost(t)
	httpClient = nil
	rd := newRedirecting(t, h.srv, func(r *http.Request) bool {
		return strings.HasSuffix(r.URL.Path, "/data") || strings.HasSuffix(r.URL.Path, "/close")
	})
	t.Setenv(nofireauth.EnvURL, rd.front.URL)
	h.signInTo(t, rd.front.URL, recToken)
	seen := pipes(t)

	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	code, err := relay([]string{"sh", "-c", "echo through; exit 3"}, os.Environ())
	if err != nil || code != 3 {
		t.Fatalf("relay: %d %v", code, err)
	}
	stdout, _ := seen()
	if !strings.Contains(stdout, "through") {
		t.Errorf("the run was disturbed: %q", stdout)
	}
	if hits, carried := rd.elsewhereHits(); len(hits) != 0 {
		t.Errorf("a redirect was followed: %v carrying %v", hits, carried)
	}
	frontHost := strings.TrimPrefix(rd.front.URL, "http://")
	if len(rd.mine) == 0 {
		t.Fatal("no request carried the token, so the run was not recorded as the test assumes")
	}
	for _, host := range rd.mine {
		if host != frontHost {
			t.Errorf("a request carrying the token went to %s, not to the deployment %s", host, frontHost)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 2 {
		t.Errorf("the recording should stay on disk when its upload is refused: %v", left)
	}
}

// A token stored for deployment A is never sent to deployment B, which only
// the environment names. B is not asked, and neither is A.
func TestATokenForOneDeploymentIsNotSentToAnother(t *testing.T) {
	a := recordingtest.New(t, recToken)
	b := recordingtest.New(t, recToken)
	h := newRecHost(t)
	h.signInTo(t, a.URL(), recToken)
	t.Setenv(nofireauth.EnvURL, b.URL())

	if relay := Attach(hullRun()); relay != nil {
		t.Fatal("recorded for a deployment with no stored token")
	}
	if a.Requests() != 0 || b.Requests() != 0 || h.srv.Requests() != 0 {
		t.Errorf("requests: A %d, B %d, other %d; want none", a.Requests(), b.Requests(), h.srv.Requests())
	}
	db, _ := nofireauth.ParseDeployment(b.URL())
	if want := []string{db.String()}; !slices.Equal(h.logins.asked, want) {
		t.Errorf("the store was asked for %v, want only the environment's deployment %v", h.logins.asked, want)
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Errorf("the state directory was created: %v", err)
	}

	// With no environment at all the deployment is the default one, and the
	// token of A is not its token.
	t.Setenv(nofireauth.EnvURL, "")
	h.logins.asked = nil
	if relay := Attach(hullRun()); relay != nil {
		t.Fatal("recorded for the default deployment with no stored token")
	}
	if want := []string{nofireauth.DefaultURL}; !slices.Equal(h.logins.asked, want) {
		t.Errorf("the store was asked for %v, want the default %v", h.logins.asked, want)
	}
	if a.Requests() != 0 {
		t.Errorf("A was sent %d requests", a.Requests())
	}
}

// A spool names the deployment it was recorded for. A flush sends its token
// only there: a spool that names a deployment the person never signed in to,
// edited by hand or planted, is left alone and nothing is sent.
func TestAFlushSendsATokenOnlyToTheDeploymentItWasStoredFor(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	for i := 0; i < 500; i++ {
		h.srv.FailNextPuts(503)
	}
	_, _ = relay([]string{"sh", "-c", "echo x"}, os.Environ())
	h.srv.ClearFailures()

	other := recordingtest.New(t, recToken)
	metas, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*.json"))
	if len(metas) != 1 {
		t.Fatalf("meta files %v", metas)
	}
	b, err := os.ReadFile(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(b), h.srv.URL(), other.URL())
	if edited == string(b) {
		t.Fatal("the meta file does not name the deployment")
	}
	if err := os.WriteFile(metas[0], []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	before := h.srv.Requests()
	if err := recordingsCmd(&strings.Builder{}, []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if other.Requests() != 0 || h.srv.Requests() != before {
		t.Errorf("a flush sent requests: other %d, original %d more", other.Requests(), h.srv.Requests()-before)
	}
}

// A run cannot ask a question, so it records with a trust store from the
// environment only when the person accepted that variable, with that value, at
// brig login. Accepted and equal: the run is recorded. Set but never accepted,
// or accepted with another value: nothing is sent, the token is not used, and
// the run prints one line that names the variable and not its value. A person
// who is not signed in gets no line. Flush refuses in the same words.
func TestATrustStoreFromTheEnvironmentStopsTheRecorder(t *testing.T) {
	for _, v := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		offLine := "brig: could not check whether this run is recorded: " + v + " is set and was not accepted when you signed in. Run `brig login` to accept it\n"
		off := func(t *testing.T, h *recHost, value string) {
			t.Helper()
			t.Setenv(v, value)
			relay := Attach(hullRun())
			if relay == nil {
				t.Fatalf("%s: a signed-in person got no relay, so no line", v)
			}
			if h.notice.Len() != 0 {
				t.Errorf("%s: printed before the exec: %q", v, h.notice)
			}
			if code, err := relay([]string{"sh", "-c", "echo must-not-run"}, os.Environ()); !errors.Is(err, cli.ErrRelayDeclined) || code != 0 {
				t.Errorf("%s: relay = %d, %v; want it to decline", v, code, err)
			}
			if got := h.notice.String(); got != offLine {
				t.Errorf("%s: line %q, want %q", v, got, offLine)
			}
			if strings.Contains(h.notice.String(), value) || strings.Contains(h.notice.String(), recToken) {
				t.Errorf("%s: the line names a value or the token: %q", v, h.notice)
			}
			if h.srv.Requests() != 0 {
				t.Errorf("%s: %d requests", v, h.srv.Requests())
			}
			if tok := tokenForURL(h.srv.URL()); tok != "" {
				t.Errorf("%s: a spool's deployment got the token", v)
			}
			err := recordingsCmd(&strings.Builder{}, []string{"flush"})
			if err == nil || !strings.Contains(err.Error(), v) || !strings.Contains(err.Error(), "brig login") ||
				strings.Contains(err.Error(), recToken) || strings.Contains(err.Error(), value) {
				t.Errorf("%s: flush: %v", v, err)
			}
			if h.srv.Requests() != 0 {
				t.Errorf("%s: flush sent %d requests", v, h.srv.Requests())
			}
		}

		t.Run(v+" accepted and equal", func(t *testing.T) {
			h := newRecHost(t)
			h.signIn(t)
			h.accept(t, h.srv.URL(), map[string]string{v: "/corp/ca.pem"})
			t.Setenv(v, "/corp/ca.pem")
			pipes(t)
			relay := Attach(hullRun())
			if relay == nil {
				t.Fatal("not recorded")
			}
			if code, err := relay([]string{"sh", "-c", "echo recorded"}, os.Environ()); err != nil || code != 0 {
				t.Fatalf("relay: %d %v", code, err)
			}
			if len(h.srv.Keys()) != 1 || h.notice.String() != recordedNotice+"\n" {
				t.Errorf("keys %v, notice %q", h.srv.Keys(), h.notice)
			}
		})
		t.Run(v+" set but never accepted", func(t *testing.T) {
			h := newRecHost(t)
			h.signIn(t)
			off(t, h, "/corp/ca.pem")
		})
		t.Run(v+" accepted but the value changed", func(t *testing.T) {
			h := newRecHost(t)
			h.signIn(t)
			h.accept(t, h.srv.URL(), map[string]string{v: "/corp/old.pem"})
			off(t, h, "/corp/new.pem")
		})
		t.Run(v+" not a terminal session", func(t *testing.T) {
			h := newRecHost(t)
			h.signIn(t)
			t.Setenv(v, "/corp/ca.pem")
			isTerminal = func(*os.File) bool { return false }
			if relay := Attach(hullRun()); relay != nil {
				t.Error("a run that is not a terminal session got a relay")
			}
			if h.notice.Len() != 0 || h.srv.Requests() != 0 || len(h.logins.asked) != 0 {
				t.Errorf("notice %q, requests %d, store asked %v", h.notice, h.srv.Requests(), h.logins.asked)
			}
		})
		t.Run(v+" not signed in", func(t *testing.T) {
			h := newRecHost(t)
			t.Setenv(v, "/corp/ca.pem")
			if relay := Attach(hullRun()); relay != nil {
				t.Error("a person who is not signed in got a relay")
			}
			if h.notice.Len() != 0 || h.srv.Requests() != 0 {
				t.Errorf("notice %q, requests %d", h.notice, h.srv.Requests())
			}
		})
	}
}
