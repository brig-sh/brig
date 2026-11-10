package broker

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
)

// fakeGCPToken is a fake value only.
const fakeGCPToken = "fake-gcp-access-token"

// gcpService is a service of a gcp route that allows methods on paths.
func gcpService(name string, methods, paths []string) v1alpha1.Service {
	s := v1alpha1.Service{Name: name}
	if methods != nil || paths != nil {
		s.Request = &v1alpha1.RequestPolicy{Allow: &v1alpha1.RequestAllow{Methods: methods, Paths: paths}}
	}
	return s
}

// gcpRoute expands a gcp route named gcp with services, the way a policy
// names it.
func gcpRoute(t *testing.T, services ...v1alpha1.Service) Route {
	t.Helper()
	r, err := policy.ExpandRoute(v1alpha1.Route{
		Name:       "gcp",
		Preset:     v1alpha1.PresetGCP,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gcp-token"}},
		Services:   services,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Route{Policy: "test-gcp", Route: r}
}

// withServiceUpstream sends every service of a gcp route to upstream, in
// place of its googleapis.com host.
func withServiceUpstream(upstream string) rigOption {
	return func(c *Config) { c.serviceUpstream = func(string) string { return upstream } }
}

// gcpRig is a rig with the gcp route rt, every service of it pointed at up.
func gcpRig(t *testing.T, up *upstream, rt Route) *rig {
	t.Helper()
	r := newRig(t, up, []Route{rt}, withServiceUpstream(up.URL))
	r.creds.set("gcp-token", []byte(fakeGCPToken))
	return r
}

// storageAndCRM is the route of most of these tests: storage may list
// buckets, and cloudresourcemanager may be read.
func storageAndCRM(t *testing.T) Route {
	return gcpRoute(t,
		gcpService("storage", []string{"GET"}, []string{"/storage/v1/b"}),
		gcpService("cloudresourcemanager", []string{"GET"}, nil))
}

// wantRefusedBeforeResolve fails unless the upstream saw upstreamSeen
// requests and no credential was read.
func wantRefusedBeforeResolve(t *testing.T, r *rig, upstreamSeen int) {
	t.Helper()
	if n := r.up.count(); n != upstreamSeen {
		t.Fatalf("the upstream saw %d requests, want %d", n, upstreamSeen)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Fatalf("Resolve ran %d times before a refusal", res)
	}
}

func TestServicesRouteForwardsToTheService(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?project=p", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if body != "ok" {
		t.Fatalf("body %q", body)
	}
	got := up.last(t)
	if got.method != "GET" || got.path != "/storage/v1/b" || got.rawQuery != "project=p" {
		t.Errorf("upstream got %s %s?%s, want GET /storage/v1/b?project=p", got.method, got.path, got.rawQuery)
	}
	if a := got.header.Get("Authorization"); a != "Bearer "+fakeGCPToken {
		t.Errorf("upstream Authorization = %q, want the real bearer", a)
	}
	for name, vs := range got.header {
		for _, v := range vs {
			if strings.Contains(v, tok) {
				t.Errorf("the session token reached the upstream in %s", name)
			}
		}
	}
	// The audit path is the route's whole rest, service included, so the
	// line says which service the request went to.
	wantAudit(t, r.lastAudit(), "status=200", "path=/storage/storage/v1/b")
}

func TestServicesRouteRefusesAnUnlistedService(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	// compute is a service brig wires, and this policy does not list it.
	resp, body := r.do("GET", "/brig-a/r/gcp/compute/compute/v1/x", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusForbidden)
	want := "brig broker refused this request\n" +
		"policy:  test-gcp\n" +
		"route:   gcp\n" +
		"rule:    spec.routes[gcp].services\n" +
		`reason:  service "compute" is not in the list [cloudresourcemanager, storage]` + "\n" +
		"sandbox: brig-a\n"
	if body != want {
		t.Errorf("body\n%s\nwant\n%s", body, want)
	}
	wantAudit(t, r.lastAudit(), "status=403", "check=request.services", "rule=spec.routes[gcp].services")
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteChecksTheServiceAllow(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	for _, tc := range []struct {
		method, path, rule, reason string
	}{
		{"POST", "/brig-a/r/gcp/storage/storage/v1/b",
			"spec.routes[gcp].services[storage].request.allow.methods",
			`method "POST" is not in the allow list [GET]`},
		// The service's paths are matched against the path after the
		// service, the one its host serves.
		{"GET", "/brig-a/r/gcp/storage/storage/v1/b/bucket/o",
			"spec.routes[gcp].services[storage].request.allow.paths",
			`path "/storage/v1/b/bucket/o" is not in the allow list [/storage/v1/b]`},
	} {
		resp, body := r.do(tc.method, tc.path, bearer(tok), "")
		wantStatus(t, resp, http.StatusForbidden)
		want := "brig broker refused this request\n" +
			"policy:  test-gcp\n" +
			"route:   gcp\n" +
			"rule:    " + tc.rule + "\n" +
			"reason:  " + tc.reason + "\n" +
			"sandbox: brig-a\n"
		if body != want {
			t.Errorf("%s %s: body\n%s\nwant\n%s", tc.method, tc.path, body, want)
		}
		wantAudit(t, r.lastAudit(), "status=403", "rule="+tc.rule, "check=request.allow.")
	}
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteRefusesTheBareRoute(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	for _, path := range []string{"/brig-a/r/gcp", "/brig-a/r/gcp/"} {
		resp, body := r.do("GET", path, bearer(tok), "")
		wantStatus(t, resp, http.StatusForbidden)
		if !strings.Contains(body, "rule:    spec.routes[gcp].services\n") {
			t.Errorf("%s: body\n%s", path, body)
		}
		wantAudit(t, r.lastAudit(), "status=403", "check=request.services")
	}
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteRefusesAnEncodedSlashInTheService(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	for _, path := range []string{
		// The upstream would read storage/x as two segments, and the
		// broker as one service name.
		"/brig-a/r/gcp/storage%2Fx/storage/v1/b",
		// The service as written is not the service as decoded. The
		// broker reads both from one segment, so they must agree.
		"/brig-a/r/gcp/stor%61ge/storage/v1/b",
	} {
		resp, _ := r.do("GET", path, bearer(tok), "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, resp.StatusCode)
		}
		wantAudit(t, r.lastAudit(), "status=400", "check=path")
	}
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteRefusesBatch(t *testing.T) {
	up := newUpstream(t, nil)
	// storage allows POST on every path, so only the batch check refuses.
	r := gcpRig(t, up, gcpRoute(t, gcpService("storage", []string{"GET", "POST"}, nil)))
	tok := r.mint("brig-a", "gcp")
	for _, path := range []string{
		"/brig-a/r/gcp/storage/batch/storage/v1",
		"/brig-a/r/gcp/storage/batch",
		"/brig-a/r/gcp/storage/Batch/storage/v1",
		"/brig-a/r/gcp/storage/batch;x/storage/v1",
	} {
		resp, body := r.do("POST", path, bearer(tok), "--batch\r\n")
		wantStatus(t, resp, http.StatusForbidden)
		want := "brig broker refused this request\n" +
			"policy:  test-gcp\n" +
			"route:   gcp\n" +
			"rule:    spec.routes[gcp].services[storage]\n" +
			"reason:  a batch request is not taken; send each request on its own\n" +
			"sandbox: brig-a\n"
		if body != want {
			t.Errorf("%s: body\n%s\nwant\n%s", path, body, want)
		}
		wantAudit(t, r.lastAudit(), "status=403", "check=request.batch",
			"rule=spec.routes[gcp].services[storage]")
	}
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteRefusesMethodOverride(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	// Google serves a GET with X-HTTP-Method-Override: DELETE as a DELETE,
	// which request.allow.methods would have checked as a GET.
	for _, name := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		h := bearer(tok)
		h.Set(name, "DELETE")
		resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", h, "")
		wantStatus(t, resp, http.StatusForbidden)
		want := "brig broker refused this request\n" +
			"policy:  test-gcp\n" +
			"route:   gcp\n" +
			"rule:    spec.routes[gcp]\n" +
			"reason:  header " + name + " names another method; send the request with the method it is for\n" +
			"sandbox: brig-a\n"
		if body != want {
			t.Errorf("%s: body\n%s\nwant\n%s", name, body, want)
		}
		wantAudit(t, r.lastAudit(), "status=403", "check=request.method-override", "rule=spec.routes[gcp]")
	}
	// Google APIs also read the method from these query parameters. The
	// parameter is refused wherever it stands and whatever it says, in any
	// case, also percent-encoded, after a semicolon, or given twice with a
	// harmless value first.
	for _, query := range []string{
		"$httpMethod=DELETE",
		"httpMethod=DELETE",
		"HTTPMETHOD=DELETE",
		"$HttpMethod=DELETE",
		"project=p&httpMethod=GET&httpMethod=DELETE",
		"$httpMethod=GET&$httpMethod=DELETE",
		"%24httpMethod=DELETE",
		"project=p;httpMethod=DELETE",
		"httpMethod",
	} {
		resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?"+query, bearer(tok), "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("?%s: status %d, want 403", query, resp.StatusCode)
		}
		if !strings.Contains(body, "rule:    spec.routes[gcp]\n") ||
			!strings.Contains(body, " names another method; send the request with the method it is for\n") {
			t.Errorf("?%s: body\n%s", query, body)
		}
		wantAudit(t, r.lastAudit(), "status=403", "check=request.method-override", "rule=spec.routes[gcp]")
	}
	// The reason names the parameter as Google spells it.
	_, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?HTTPMETHOD=DELETE", bearer(tok), "")
	if want := "reason:  query parameter httpMethod names another method; " +
		"send the request with the method it is for\n"; !strings.Contains(body, want) {
		t.Errorf("body\n%s\nwant a line\n%s", body, want)
	}
	wantRefusedBeforeResolve(t, r, 0)
	// The names are matched whole, so a parameter that only starts like
	// one is no override and goes through.
	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?httpmethodology=1&prefix=httpMethod", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
}

func TestServicesRouteVerbPathIsExact(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, gcpRoute(t,
		gcpService("cloudresourcemanager", []string{"POST"}, []string{"/v1/projects/*:getIamPolicy"})))
	tok := r.mint("brig-a", "gcp")
	resp, _ := r.do("POST", "/brig-a/r/gcp/cloudresourcemanager/v1/projects/p:getIamPolicy", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t); got.path != "/v1/projects/p:getIamPolicy" {
		t.Errorf("upstream got path %s", got.path)
	}
	resp, _ = r.do("POST", "/brig-a/r/gcp/cloudresourcemanager/v1/projects/p:setIamPolicy", bearer(tok), "{}")
	wantStatus(t, resp, http.StatusForbidden)
	wantAudit(t, r.lastAudit(), "status=403", "check=request.allow.paths",
		"rule=spec.routes[gcp].services[cloudresourcemanager].request.allow.paths")
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

// A * in a path glob stops at a slash and not at a colon, so /v1/projects/*
// allows every custom verb on a project, setIamPolicy among them. The test
// pins that matching: a change to it is a change to what every gcp policy
// allows, and must be made on purpose, with the docs that warn about it.
func TestServicesRouteWildcardMatchesAVerb(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, gcpRoute(t,
		gcpService("cloudresourcemanager", []string{"GET", "POST"}, []string{"/v1/projects/*"})))
	resp, _ := r.do("POST", "/brig-a/r/gcp/cloudresourcemanager/v1/projects/p:setIamPolicy",
		bearer(r.mint("brig-a", "gcp")), "{}")
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t); got.path != "/v1/projects/p:setIamPolicy" {
		t.Errorf("upstream got path %s", got.path)
	}
}

func TestServicesRouteLoggingPostRead(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, gcpRoute(t, gcpService("logging", []string{"POST"}, []string{"/v2/entries:list"})))
	tok := r.mint("brig-a", "gcp")
	const query = `{"resourceNames":["projects/p"],"pageSize":5}`
	resp, _ := r.do("POST", "/brig-a/r/gcp/logging/v2/entries:list", bearer(tok), query)
	wantStatus(t, resp, http.StatusOK)
	got := up.last(t)
	if got.path != "/v2/entries:list" || got.body != query {
		t.Errorf("upstream got %s with body %q", got.path, got.body)
	}
	for _, tc := range []struct{ method, path, check string }{
		{"POST", "/brig-a/r/gcp/logging/v2/entries:write", "check=request.allow.paths"},
		{"GET", "/brig-a/r/gcp/logging/v2/entries:list", "check=request.allow.methods"},
	} {
		resp, _ := r.do(tc.method, tc.path, bearer(tok), query)
		wantStatus(t, resp, http.StatusForbidden)
		wantAudit(t, r.lastAudit(), "status=403", tc.check)
	}
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", n)
	}
}

func TestServicesRouteRetryKeepsTheServiceHost(t *testing.T) {
	up := newUpstream(t, refuseStale)
	rt := storageAndCRM(t)
	// A credential with a lifecycle, so an upstream 401 renews it and
	// retries.
	// The anthropic-oauth lifecycle is borrowed only to make an upstream 401
	// force a renewal; a gcp route carries no lifecycle of its own.
	rt.Route.Credential = &v1alpha1.CredentialRef{
		SecretRef: v1alpha1.SecretRef{Name: "claude-credentials"},
		Field:     "claudeAiOauth.accessToken",
		Lifecycle: v1alpha1.LifecycleAnthropicOAuth,
	}
	r := gcpRig(t, up, rt)
	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?project=p", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusOK)
	wantAudit(t, r.lastAudit(), "status=200", "renew=retried")
	up.mu.Lock()
	reqs := append([]seen(nil), up.reqs...)
	up.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", len(reqs))
	}
	for i, got := range reqs {
		if got.path != "/storage/v1/b" || got.rawQuery != "project=p" {
			t.Errorf("request %d reached %s?%s, want /storage/v1/b?project=p", i, got.path, got.rawQuery)
		}
	}
	if a := reqs[1].header.Get("Authorization"); a != "Bearer "+fakeFresh {
		t.Errorf("the retry carried Authorization %q", a)
	}
}

func TestAddSandboxBuildsAnUpstreamPerService(t *testing.T) {
	h, err := New(Config{HostKey: testHostKey, Credentials: newFakeCreds()})
	if err != nil {
		t.Fatal(err)
	}
	rt := gcpRoute(t, gcpService("storage", nil, nil), gcpService("logging", nil, nil))
	if rt.Route.Upstream != "" {
		t.Fatalf("the gcp route has upstream %q", rt.Route.Upstream)
	}
	if err := h.AddSandbox(Sandbox{ID: "brig-a", Routes: []Route{rt}}); err != nil {
		t.Fatal(err)
	}
	got := h.sandbox("brig-a").routes["gcp"].services
	for name, want := range map[string]string{
		"storage": "https://storage.googleapis.com",
		"logging": "https://logging.googleapis.com",
	} {
		if u := got[name]; u == nil || u.String() != want {
			t.Errorf("service %s goes to %v, want %s", name, u, want)
		}
	}
	if len(got) != 2 {
		t.Errorf("the route has %d services, want 2", len(got))
	}

	// A route without services still needs an upstream.
	inv := customRoute(t, "inventory", "https://inventory.example.com", nil)
	inv.Route.Upstream = ""
	if err := h.AddSandbox(Sandbox{ID: "brig-b", Routes: []Route{inv}}); err == nil {
		t.Error("a route with no upstream and no services was registered")
	}

	// A route with services that skipped validation is still refused for an
	// upstream it would never use, a service listed twice, or a mode whose
	// divert would read an upstream it does not have.
	for what, edit := range map[string]func(*v1alpha1.Route){
		"an upstream":      func(r *v1alpha1.Route) { r.Upstream = "https://example.com" },
		"a repeat service": func(r *v1alpha1.Route) { r.Services = append(r.Services, r.Services[0]) },
		"another mode":     func(r *v1alpha1.Route) { r.Mode = "proxy" },
	} {
		rt := gcpRoute(t, gcpService("storage", nil, nil))
		edit(&rt.Route)
		if err := h.AddSandbox(Sandbox{ID: "brig-d", Routes: []Route{rt}, Modes: []v1alpha1.RouteMode{"proxy"}}); err == nil {
			t.Errorf("a route with services and %s was registered", what)
		}
	}

	// The service name becomes part of a host name. One brig does not wire
	// would let a route that skipped validation name any host.
	bad := gcpRoute(t, gcpService("storage", nil, nil))
	bad.Route.Services[0].Name = "evil.example.com/x"
	if err := h.AddSandbox(Sandbox{ID: "brig-c", Routes: []Route{bad}}); err == nil {
		t.Error("a service brig does not wire was registered")
	}
}

// Each service has its own host. The other tests send every service to one
// upstream, so they would pass if the broker sent a request to the host of
// another service; this one fails then.
func TestServicesRouteReachesTheHostOfItsService(t *testing.T) {
	storage := newUpstream(t, nil)
	crm := newUpstream(t, nil)
	// Both upstreams serve the httptest certificate, so the transport of
	// one trusts the other.
	r := newRig(t, storage, []Route{storageAndCRM(t)}, func(c *Config) {
		c.serviceUpstream = func(name string) string {
			switch name {
			case "storage":
				return storage.URL
			case "cloudresourcemanager":
				return crm.URL
			}
			return "https://unused.invalid"
		}
	})
	r.creds.set("gcp-token", []byte(fakeGCPToken))
	tok := r.mint("brig-a", "gcp")

	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if s, c := storage.count(), crm.count(); s != 1 || c != 0 {
		t.Fatalf("after a storage request, storage saw %d and cloudresourcemanager %d, want 1 and 0", s, c)
	}
	if got := storage.last(t); got.path != "/storage/v1/b" {
		t.Errorf("storage got path %s, want /storage/v1/b", got.path)
	}

	resp, _ = r.do("GET", "/brig-a/r/gcp/cloudresourcemanager/v1/projects/p", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	if s, c := storage.count(), crm.count(); s != 1 || c != 1 {
		t.Fatalf("after a cloudresourcemanager request, storage saw %d and cloudresourcemanager %d, want 1 and 1", s, c)
	}
	if got := crm.last(t); got.path != "/v1/projects/p" {
		t.Errorf("cloudresourcemanager got path %s, want /v1/projects/p", got.path)
	}
}

// The Google token goes upstream and nowhere else. The upstream replies
// without echoing anything, so any trace of the token in what the guest
// gets back was put there by the broker.
func TestServicesRouteGuestNeverSeesTheGoogleToken(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	r := gcpRig(t, up, storageAndCRM(t))
	tok := r.mint("brig-a", "gcp")
	if strings.Contains(tok, fakeGCPToken) {
		t.Fatal("the session token holds the Google token")
	}
	sent := bearer(tok)
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b?project=p", sent, "")
	wantStatus(t, resp, http.StatusOK)

	if a := up.last(t).header.Get("Authorization"); a != "Bearer "+fakeGCPToken {
		t.Errorf("upstream Authorization = %q, want the Google token", a)
	}
	// The guest sent the session token alone.
	for name, vs := range sent {
		for _, v := range vs {
			if strings.Contains(v, fakeGCPToken) {
				t.Errorf("the guest sent the Google token in %s", name)
			}
		}
	}
	if body != `{"items":[]}` {
		t.Errorf("body %q", body)
	}
	if strings.Contains(body, fakeGCPToken) {
		t.Error("the response body holds the Google token")
	}
	for name, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, fakeGCPToken) {
				t.Errorf("response header %s holds the Google token", name)
			}
		}
	}
	if strings.Contains(r.lastAudit(), fakeGCPToken) {
		t.Error("the audit line holds the Google token")
	}
}

// storageObjectRoute is a gcp route whose storage service reads the objects
// of bucket bkt, nested ones included, and copies one to another in it.
func storageObjectRoute(t *testing.T) Route {
	return gcpRoute(t,
		gcpService("storage", []string{"GET", "POST"}, []string{
			"/storage/v1/b/bkt/o/*",
			"/download/storage/v1/b/bkt/o/*",
			"/storage/v1/b/bkt/o/*/rewriteTo/b/bkt/o/*",
			"/storage/v1/b/bkt/o/*/copyTo/b/bkt/o/*",
			"/storage/v1/b/bkt/o/*/moveTo/o/*",
		}),
		gcpService("cloudresourcemanager", []string{"GET"}, nil))
}

func TestServicesRouteReachesAStorageObjectWithASlash(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageObjectRoute(t))
	tok := r.mint("brig-a", "gcp")
	for _, tc := range []struct{ method, path, upstream string }{
		{"GET", "/storage/storage/v1/b/bkt/o/dir%2Fsub%2Ffile", "/storage/v1/b/bkt/o/dir%2Fsub%2Ffile"},
		// A client that builds the download URL from the endpoint downloads
		// from here.
		{"GET", "/storage/download/storage/v1/b/bkt/o/dir%2Ffile?alt=media", "/download/storage/v1/b/bkt/o/dir%2Ffile"},
		// The upstream gets the escape as the guest wrote it.
		{"GET", "/storage/storage/v1/b/bkt/o/dir%2ffile", "/storage/v1/b/bkt/o/dir%2ffile"},
		{"POST", "/storage/storage/v1/b/bkt/o/a%2Fb/rewriteTo/b/bkt/o/c%2Fd", "/storage/v1/b/bkt/o/a%2Fb/rewriteTo/b/bkt/o/c%2Fd"},
		{"POST", "/storage/storage/v1/b/bkt/o/a%2Fb/copyTo/b/bkt/o/c%2Fd", "/storage/v1/b/bkt/o/a%2Fb/copyTo/b/bkt/o/c%2Fd"},
		{"POST", "/storage/storage/v1/b/bkt/o/a%2Fb/moveTo/o/c%2Fd", "/storage/v1/b/bkt/o/a%2Fb/moveTo/o/c%2Fd"},
		// A folder placeholder, a name that ends in a slash, is a source
		// as well as a target.
		{"GET", "/storage/storage/v1/b/bkt/o/dir%2F", "/storage/v1/b/bkt/o/dir%2F"},
		{"POST", "/storage/storage/v1/b/bkt/o/dir%2F/copyTo/b/bkt/o/new%2F", "/storage/v1/b/bkt/o/dir%2F/copyTo/b/bkt/o/new%2F"},
		{"POST", "/storage/storage/v1/b/bkt/o/a%2Fdir%2f/rewriteTo/b/bkt/o/c", "/storage/v1/b/bkt/o/a%2Fdir%2f/rewriteTo/b/bkt/o/c"},
		{"POST", "/storage/storage/v1/b/bkt/o/dir%2F/moveTo/o/new%2F", "/storage/v1/b/bkt/o/dir%2F/moveTo/o/new%2F"},
	} {
		resp, _ := r.do(tc.method, "/brig-a/r/gcp"+tc.path, bearer(tok), "")
		wantStatus(t, resp, http.StatusOK)
		if got := up.last(t); got.method != tc.method || got.path != tc.upstream {
			t.Errorf("%s %s: upstream got %s %s, want %s", tc.method, tc.path, got.method, got.path, tc.upstream)
		}
	}
}

func TestServicesRouteRefusesAnEncodedSlashOutsideAStorageObject(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, storageObjectRoute(t))
	tok := r.mint("brig-a", "gcp")
	for _, path := range []string{
		// A bucket name holds no slash, so an upstream may read one as a
		// separator.
		"/storage/storage/v1/b/bk%2Ft/o/x",
		"/storage/storage/v1/b%2Fbkt/o/x",
		// The acl entity is not an object name.
		"/storage/storage/v1/b/bkt/o/x/acl/user%2Fy",
		// The XML API and other services keep the refusal.
		"/storage/bkt/dir%2Ffile",
		"/cloudresourcemanager/v1/projects/x%2Fy",
		// A segment of the grammar that is escaped is not that segment.
		"/storage/storage/v1/%62/bkt/o/a%2Fb",
		"/storage/storage/v1/b/bkt/%6F/a%2Fb",
		// The decoded name is checked as a path: no escaped escape, and
		// no dot or empty piece.
		"/storage/storage/v1/b/bkt/o/a%252Fb",
		"/storage/storage/v1/b/bkt/o/a%2F..%2Fb",
		"/storage/storage/v1/b/bkt/o/a%2F.%2Fb",
		"/storage/storage/v1/b/bkt/o/a%2F%2Fb",
		"/storage/storage/v1/b/bkt/o/%2Fa",
		"/storage/storage/v1/b/bkt/o/a%2F%2e%2e%2Fb",
		// Only the last piece of a name may be empty, and only in a name.
		"/storage/storage/v1/b/bkt/o/dir%2F%2F/copyTo/b/bkt/o/c",
		"/storage/storage/v1/b/bkt/o/a%2F%2Fb%2F/copyTo/b/bkt/o/c",
		"/storage/storage/v1/b/bkt/o/%2F/copyTo/b/bkt/o/c",
		"/storage/storage/v1/b/bkt/o/dir%2F/acl//x",
		"/storage/storage/v1/b/bkt/o/dir%2F/copyTo/b//o/c",
		"/storage/storage/v1/b/bkt/o/a%2F..%2F/copyTo/b/bkt/o/c",
	} {
		resp, _ := r.do("GET", "/brig-a/r/gcp"+path, bearer(tok), "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, resp.StatusCode)
		}
		wantAudit(t, r.lastAudit(), "status=400", "check=path")
	}
	wantRefusedBeforeResolve(t, r, 0)
}

func TestServicesRouteMatchesAStorageObjectPrefix(t *testing.T) {
	up := newUpstream(t, nil)
	r := gcpRig(t, up, gcpRoute(t,
		gcpService("storage", []string{"GET"}, []string{
			"/storage/v1/b/bkt/o/reports%2F*",
			"/download/storage/v1/b/bkt/o/reports%2F*",
		})))
	tok := r.mint("brig-a", "gcp")
	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b/bkt/o/reports%2Fq1%2Fsales.csv", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	// The rule is matched with the slash as %2F, however the guest cased it.
	resp, _ = r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b/bkt/o/reports%2fq1", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	resp, _ = r.do("GET", "/brig-a/r/gcp/storage/download/storage/v1/b/bkt/o/reports%2Fq1?alt=media", bearer(tok), "")
	wantStatus(t, resp, http.StatusOK)
	resp, _ = r.do("GET", "/brig-a/r/gcp/storage/download/storage/v1/b/bkt/o/other%2Fx?alt=media", bearer(tok), "")
	wantStatus(t, resp, http.StatusForbidden)
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b/bkt/o/other%2Fx", bearer(tok), "")
	wantStatus(t, resp, http.StatusForbidden)
	if want := `reason:  path "/storage/v1/b/bkt/o/other%2Fx" is not in the allow list [/storage/v1/b/bkt/o/reports%2F*, /download/storage/v1/b/bkt/o/reports%2F*]`; !strings.Contains(body, want) {
		t.Errorf("body\n%s\nwant a line\n%s", body, want)
	}
	// A slash outside an object name is a separator, which * does not cross.
	resp, _ = r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b/bkt/o/reports%2Fx/acl", bearer(tok), "")
	wantStatus(t, resp, http.StatusForbidden)
	if n := up.count(); n != 3 {
		t.Fatalf("the upstream saw %d requests, want 3", n)
	}
}
