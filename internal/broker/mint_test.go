package broker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// gcloudPointer and gcloudSelfPointer are pointers at a host gcloud login,
// as a secret holds them, in each mode. They name a configuration, a service
// account and an account, which the upstream must not see.
const (
	gcloudPointer = `{"kind":"gcloud","configuration":"brig-test","mode":"impersonate",` +
		`"serviceAccount":"agent@p.iam.gserviceaccount.com"}`
	gcloudSelfPointer = `{"kind":"gcloud","configuration":"brig-test","mode":"self",` +
		`"account":"dev@example.com"}`
)

// fakeMinter answers every Mint with token and err, and records the docs it
// was given.
type fakeMinter struct {
	mu    sync.Mutex
	token string
	err   error
	docs  [][]byte
}

func (f *fakeMinter) Mint(_ context.Context, _, _ string, doc []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, append([]byte(nil), doc...))
	return f.token, f.err
}

func (f *fakeMinter) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.docs)
}

// isGcloud stands in for gcpcred.IsPointer: any JSON object of kind gcloud,
// valid or not.
func isGcloud(doc []byte) bool { return strings.Contains(string(doc), `"kind":"gcloud"`) }

// withMinter gives the broker m, with isGcloud as its MintDoc.
func withMinter(m Minter) rigOption {
	return func(c *Config) { c.Minter, c.MintDoc = m, isGcloud }
}

// mintRig is a gcp rig whose secret is doc, minted by m.
func mintRig(t *testing.T, up *upstream, m *fakeMinter, doc string) *rig {
	t.Helper()
	r := newRig(t, up, []Route{storageAndCRM(t)}, withServiceUpstream(up.URL), withMinter(m))
	r.creds.set("gcp-token", []byte(doc))
	return r
}

// wantNoHeaderHolds fails if any header the upstream saw holds s.
func wantNoHeaderHolds(t *testing.T, h http.Header, s string) {
	t.Helper()
	for name, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, s) {
				t.Errorf("upstream header %s holds %q: %q", name, s, v)
			}
		}
	}
}

func TestAGCPRouteInjectsTheMintedToken(t *testing.T) {
	for _, doc := range []string{gcloudPointer, gcloudSelfPointer} {
		up := newUpstream(t, nil)
		m := &fakeMinter{token: "minted-1"}
		r := mintRig(t, up, m, doc)
		resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(r.mint("brig-a", "gcp")), "")
		wantStatus(t, resp, http.StatusOK)
		got := up.last(t)
		if a := got.header.Get("Authorization"); a != "Bearer minted-1" {
			t.Errorf("upstream Authorization = %q, want the minted token", a)
		}
		for _, s := range []string{"gcloud", "brig-test", "dev@example.com", "agent@p.iam"} {
			wantNoHeaderHolds(t, got.header, s)
		}
		if m.calls() != 1 || string(m.docs[0]) != doc {
			t.Errorf("Mint got %q, want the pointer once", m.docs)
		}
	}
}

func TestAPointerOnARouteWithoutServicesIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	m := &fakeMinter{token: "minted-1"}
	routes := []Route{presetRoute(t, v1alpha1.PresetGitHubToken, "github", "gh-token", up.URL, nil)}
	r := newRig(t, up, routes, withMinter(m))
	r.creds.set("gh-token", []byte(gcloudPointer))
	resp, body := r.do("GET", "/brig-a/r/github/user", bearer(r.mint("brig-a", "github")), "")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=credential.gcponly")
	want := "brig broker: the credential of route github is a gcloud pointer, which only a gcp route takes\n"
	if body != want {
		t.Errorf("body %q, want %q", body, want)
	}
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
	if n := m.calls(); n != 0 {
		t.Errorf("Mint ran %d times for a route without services", n)
	}
}

func TestAFailedMintSendsNothing(t *testing.T) {
	up := newUpstream(t, nil)
	m := &fakeMinter{err: errors.New("x /host/path")}
	r := mintRig(t, up, m, gcloudPointer)
	tok := r.mint("brig-a", "gcp")
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(tok), "")
	wantStatus(t, resp, http.StatusBadGateway)
	wantAudit(t, r.lastAudit(), "check=credential")
	if strings.Contains(body, "/host/path") {
		t.Errorf("the body quotes the Minter's error: %q", body)
	}
	if !strings.Contains(body, "could not be minted; see the broker log") {
		t.Errorf("body %q, want the fixed text", body)
	}

	m.mu.Lock()
	m.err = &VendError{Message: "run gcloud auth login"}
	m.mu.Unlock()
	resp, body = r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(tok), "")
	wantStatus(t, resp, http.StatusBadGateway)
	if !strings.Contains(body, "run gcloud auth login") {
		t.Errorf("body %q, want the VendError's message", body)
	}
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

func TestAGCPRouteWithARawTokenIsUnchanged(t *testing.T) {
	up := newUpstream(t, nil)
	m := &fakeMinter{token: "minted-1"}
	r := mintRig(t, up, m, "ya29.x")
	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusOK)
	if a := up.last(t).header.Get("Authorization"); a != "Bearer ya29.x" {
		t.Errorf("upstream Authorization = %q, want the raw token", a)
	}
	if n := m.calls(); n != 0 {
		t.Errorf("Mint ran %d times for a raw token", n)
	}
}

// A pointer that does not parse is still a pointer: it goes to the Minter,
// which refuses it, and is never sent as the token itself.
func TestAMalformedPointerIsNotSentAsAToken(t *testing.T) {
	up := newUpstream(t, nil)
	m := &fakeMinter{err: errors.New(`gcloud pointer: mode "bogus" is not impersonate or self`)}
	r := mintRig(t, up, m, `{"kind":"gcloud","mode":"bogus"}`)
	resp, _ := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusBadGateway)
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
	if n := m.calls(); n != 1 {
		t.Errorf("Mint ran %d times, want 1", n)
	}
}

// A 401 to a minted token is the upstream's answer to pass on: gcloud would
// hand back the same cached token, so there is nothing to retry with.
func TestAMintedTokenPassesA401Through(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	})
	m := &fakeMinter{token: "minted-1"}
	r := mintRig(t, up, m, gcloudPointer)
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if body != "expired\n" {
		t.Errorf("body %q, want the upstream's", body)
	}
	if n := up.count(); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
	if n := m.calls(); n != 1 {
		t.Errorf("Mint ran %d times, want 1", n)
	}
	if _, renews := r.creds.counts(); renews != 0 {
		t.Errorf("ForceRenew ran %d times", renews)
	}
}

// The same holds on a route whose credential has a lifecycle, which a gcp
// route does not have today: a 401 to a minted token renews nothing, so the
// pointer is never handed to the renewer or sent as the credential of a
// retry.
func TestAMintedTokenIsNotRenewedOnARouteWithALifecycle(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	})
	m := &fakeMinter{token: "minted-1"}
	rt := storageAndCRM(t)
	rt.Route.Credential = &v1alpha1.CredentialRef{
		SecretRef: v1alpha1.SecretRef{Name: "gcp-token"},
		Lifecycle: v1alpha1.LifecycleAnthropicOAuth,
	}
	r := newRig(t, up, []Route{rt}, withServiceUpstream(up.URL), withMinter(m))
	r.creds.set("gcp-token", []byte(gcloudPointer))
	resp, body := r.do("GET", "/brig-a/r/gcp/storage/storage/v1/b", bearer(r.mint("brig-a", "gcp")), "")
	wantStatus(t, resp, http.StatusUnauthorized)
	if body != "expired\n" {
		t.Errorf("body %q, want the upstream's", body)
	}
	if n := up.count(); n != 1 {
		t.Errorf("the upstream saw %d requests, want 1", n)
	}
	if _, renews := r.creds.counts(); renews != 0 {
		t.Errorf("ForceRenew ran %d times", renews)
	}
}

func TestAMinterNeedsMintDoc(t *testing.T) {
	for _, cfg := range []Config{
		{HostKey: testHostKey, Credentials: newFakeCreds(), Minter: &fakeMinter{}},
		{HostKey: testHostKey, Credentials: newFakeCreds(), MintDoc: isGcloud},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New took Minter %v and MintDoc set %v", cfg.Minter != nil, cfg.MintDoc != nil)
		}
	}
	if _, err := New(Config{HostKey: testHostKey, Credentials: newFakeCreds(),
		Minter: &fakeMinter{}, MintDoc: isGcloud}); err != nil {
		t.Errorf("New refused both set: %v", err)
	}
}
