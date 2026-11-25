// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
)

// prefixed serves the fake deployment under a path prefix, as a deployment
// behind a shared host does. A request outside the prefix is another
// application on the host: it is refused, counted, and its bearer header kept.
type prefixed struct {
	front   *httptest.Server
	mu      sync.Mutex
	inside  int
	outside []string // method and path of every request outside the prefix
	bearers []string // Authorization headers those requests carried
}

func newPrefixed(t *testing.T, h *recHost, prefix string) *prefixed {
	t.Helper()
	p := &prefixed{}
	target, _ := url.Parse(h.srv.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	p.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			p.outside = append(p.outside, r.Method+" "+r.URL.Path)
			p.bearers = append(p.bearers, r.Header.Get("Authorization"))
			http.NotFound(w, r)
			return
		}
		p.inside++
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(p.front.Close)
	return p
}

// For a deployment whose URL has a path, every request of a recorded run and of
// a flush stays under that path. A request outside it would hand the token to
// another application on the host.
func TestRequestsOfARunAndAFlushStayUnderTheDeploymentsPath(t *testing.T) {
	h := newRecHost(t)
	p := newPrefixed(t, h, "/nofire/eu")
	dep := p.front.URL + "/nofire/eu"
	t.Setenv(nofireauth.EnvURL, dep)
	h.signInTo(t, dep, recToken)
	pipes(t)

	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	// The uploads fail for the whole drain, so the spool is left for a flush.
	for i := 0; i < 500; i++ {
		h.srv.FailNextPuts(503)
	}
	if _, err := relay([]string{"sh", "-c", "echo through-the-prefix"}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	h.srv.ClearFailures()
	if len(h.srv.Keys()) != 1 {
		t.Fatalf("the open did not reach the deployment: keys %v", h.srv.Keys())
	}

	var out bytes.Buffer
	if err := recordingsCmd(&out, []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 sent and closed") {
		t.Errorf("flush: %q", out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.outside) != 0 {
		t.Errorf("requests left the deployment's path: %v carrying %v", p.outside, p.bearers)
	}
	if p.inside == 0 {
		t.Error("no request was made under the prefix")
	}
}
