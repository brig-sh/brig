package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
)

// Fake values only.
const (
	fakeGCPMinted     = "ya29.fake-minted-token-for-the-broker-tests"
	fakeGCPRaw        = "ya29.fake-raw-token-for-the-broker-tests"
	gcpServiceAccount = "sandbox@proj.iam.gserviceaccount.com"
	gcpPointer        = `{"kind":"gcloud","configuration":"brig-test","mode":"impersonate",` +
		`"serviceAccount":"` + gcpServiceAccount + `"}`
)

// fakeGCloud is a gcloud binary that records each call and its environment
// in its directory, and fails as a lapsed login does while the file lapsed
// is there.
type fakeGCloud struct {
	dir string
	bin string
}

func newFakeGCloud(t *testing.T) *fakeGCloud {
	t.Helper()
	f := &fakeGCloud{dir: t.TempDir()}
	f.bin = filepath.Join(f.dir, "gcloud")
	script := fmt.Sprintf(`#!/bin/sh
d=%q
echo "$*" >> "$d/calls"
env >> "$d/env"
if [ -e "$d/lapsed" ]; then
	echo "ERROR: (gcloud.auth.print-access-token) Reauthentication failed. cannot prompt during non-interactive execution." >&2
	exit 1
fi
case "$1 $2" in
"auth print-access-token") printf '{"token":"%%s"}' %q ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`, f.dir, fakeGCPMinted)
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeGCloud) lapse(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "lapsed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeGCloud) unlapse(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, "lapsed")); err != nil {
		t.Fatal(err)
	}
}

// calls is every argv the fake was run with.
func (f *fakeGCloud) calls(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
}

// fakeTokenInfo answers every tokeninfo request for the fake's token with an
// hour left, for the service account.
func fakeTokenInfo(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("access_token") != fakeGCPMinted {
			http.Error(w, "invalid_token", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"expires_in":"3600","email":%q}`, gcpServiceAccount)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gcpBrokerRoute is a gcp route as policy.Routes hands it to brig.
func gcpBrokerRoute() injector.Route {
	r := v1alpha1.Route{Name: "gcp", Preset: v1alpha1.PresetGCP,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gcp"}},
		Services:   []v1alpha1.Service{{Name: "storage"}}}
	return injector.Route{Policy: "gcp", Route: r}
}

func gcpSandbox(id string) injector.Sandbox {
	return injector.Sandbox{ID: id, Profile: "claude-code", Routes: []injector.Route{gcpBrokerRoute()}}
}

// startGCPBroker serves cfg with the fake gcloud and tokeninfo, and upstream
// u. The store is empty: each test puts in the secret it needs.
func startGCPBroker(t *testing.T, cfg serverConfig, u *upstream) (*running, *fakeGCloud) {
	t.Helper()
	f := newFakeGCloud(t)
	if cfg.dir == "" {
		cfg.dir = shortDir(t)
	}
	if cfg.logTo == nil {
		cfg.logTo = io.Discard
	}
	cfg.gcloudBin = f.bin
	cfg.tokenInfoURL = fakeTokenInfo(t).URL
	return startBrokerWith(t, cfg, u), f
}

// A lapsed login refuses the run at its start, with the command that fixes
// it, and registers nothing.
func TestRegistrationIsRefusedWhenTheMintFails(t *testing.T) {
	r, f := startGCPBroker(t, serverConfig{}, nil)
	if err := r.store.Create("gcp", []byte(gcpPointer)); err != nil {
		t.Fatal(err)
	}
	f.lapse(t)
	_, err := r.client.Add(gcpSandbox("brig-a"))
	ref := refusal(t, err)
	if ref.Status != http.StatusFailedDependency || ref.Kind != "credential" {
		t.Errorf("refusal = %d kind %q, want 424 kind credential", ref.Status, ref.Kind)
	}
	if !strings.HasPrefix(ref.Message, "route gcp: ") ||
		!strings.Contains(ref.Message, "gcloud auth login --configuration brig-test") {
		t.Errorf("message = %q, want the route and the gcloud auth login hint", ref.Message)
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sandboxes) != 0 {
		t.Errorf("sandboxes = %v after a refused registration", st.Sandboxes)
	}
	if _, err := r.client.Mint(injector.MintRequest{Sandbox: "brig-a", Route: "gcp"}); err == nil {
		t.Error("a token was minted for a sandbox whose registration was refused")
	}
}

// A mint that fails is named in status until one works, as a fetch of a
// route that vends is, so a route whose guest gets 502s is not invisible
// between them. A sandbox that is removed takes its failures with it.
func TestStatusNamesAGCPRouteWhoseMintFailed(t *testing.T) {
	// The fake's token lives an hour from the host's clock. The broker's
	// clock is moved past it, so the cache mints again.
	var ahead atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	r, f := startGCPBroker(t, serverConfig{now: now}, nil)
	if err := r.store.Create("gcp", []byte(gcpPointer)); err != nil {
		t.Fatal(err)
	}
	vending := func() []string {
		t.Helper()
		st, err := r.client.Status()
		if err != nil {
			t.Fatal(err)
		}
		return st.Vending
	}
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if v := vending(); len(v) != 0 {
		t.Fatalf("vending = %v after a mint that worked", v)
	}

	f.lapse(t)
	ahead.Store(int64(59 * time.Minute))
	if ref := refusal(t, func() error { _, err := r.client.Add(gcpSandbox("brig-a")); return err }()); ref.Kind != "credential" {
		t.Fatalf("kind = %q, want credential", ref.Kind)
	}
	if v := vending(); !slices.Equal(v, []string{"brig-a/gcp"}) {
		t.Fatalf("vending = %v, want brig-a/gcp", v)
	}
	f.unlapse(t)
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if v := vending(); len(v) != 0 {
		t.Fatalf("vending = %v after a mint that worked", v)
	}

	f.lapse(t)
	ahead.Store(int64(61 * time.Minute))
	if _, err := r.client.Add(gcpSandbox("brig-a")); err == nil {
		t.Fatal("a registration on a lapsed login was not refused")
	}
	if v := vending(); !slices.Equal(v, []string{"brig-a/gcp"}) {
		t.Fatalf("vending = %v, want brig-a/gcp", v)
	}
	if _, err := r.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if v := vending(); len(v) != 0 {
		t.Errorf("vending = %v after the sandbox was removed", v)
	}
}

// Registration mints the token, and the request after it is served from
// the cache: one gcloud run, with the service account named. The token
// reaches the upstream and no file of the broker's directory.
func TestRegistrationMintsOnceForAPointer(t *testing.T) {
	logs := &lockedBuffer{}
	u := newUpstream(t)
	r, f := startGCPBroker(t, serverConfig{logTo: logs}, u)
	if err := r.store.Create("gcp", []byte(gcpPointer)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("registration ran gcloud %d times, want 1: %q", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "auth print-access-token --configuration brig-test "+
		"--impersonate-service-account "+gcpServiceAccount) {
		t.Errorf("gcloud ran with %q", calls[0])
	}
	if code := r.send(t, "brig-a", "gcp", r.mint(t, "brig-a", "gcp"), "/storage/storage/v1/b"); code != http.StatusOK {
		t.Fatalf("request = %d, want 200", code)
	}
	seen := u.requests()
	if len(seen) != 1 || seen[0].Get("Authorization") != "Bearer "+fakeGCPMinted {
		t.Errorf("upstream saw %v, want one request with the minted token", seen)
	}
	if n := len(f.calls(t)); n != 1 {
		t.Errorf("gcloud ran %d times, want 1: the request after registration is served from the cache", n)
	}
	if strings.Contains(logs.String(), fakeGCPMinted) {
		t.Error("the minted token is in the broker log")
	}
	err := filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(blob), fakeGCPMinted) {
			t.Errorf("the minted token is in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A secret that is a token, not a pointer, is injected as it is: gcloud is
// not the host's to run for it.
func TestARawTokenGCPRouteRegistersWithoutGCloud(t *testing.T) {
	u := newUpstream(t)
	r, f := startGCPBroker(t, serverConfig{}, u)
	if err := r.store.Create("gcp", []byte(fakeGCPRaw)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if code := r.send(t, "brig-a", "gcp", r.mint(t, "brig-a", "gcp"), "/storage/storage/v1/b"); code != http.StatusOK {
		t.Fatalf("request = %d, want 200", code)
	}
	if seen := u.requests(); len(seen) != 1 || seen[0].Get("Authorization") != "Bearer "+fakeGCPRaw {
		t.Errorf("upstream saw %v, want one request with the raw token", seen)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("gcloud ran for a raw token: %q", calls)
	}
}

// A secret the store does not hold is reported on the request, as for a gcp
// route that injects a raw token: registration does not refuse the run.
func TestAMissingSecretDoesNotRefuseRegistration(t *testing.T) {
	r, f := startGCPBroker(t, serverConfig{}, nil)
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatalf("registration with no secret: %v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("gcloud ran with no pointer to run it for: %q", calls)
	}
}

// A sandbox that is removed, or whose registration is refused after the
// mint, leaves no token behind: a later sandbox of the same id mints its
// own, and is not served one minted for a sandbox that is gone.
func TestAForgottenSandboxDropsItsTokens(t *testing.T) {
	r, f := startGCPBroker(t, serverConfig{}, nil)
	if err := r.store.Create("gcp", []byte(gcpPointer)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
			t.Fatal(err)
		}
	}
	// A token with an hour left is fresh enough for a second registration.
	if n := len(f.calls(t)); n != 1 {
		t.Fatalf("two registrations minted %d times, want 1", n)
	}
	if _, err := r.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(t)); n != 2 {
		t.Fatalf("a registration after remove minted %d times in all, want 2", n)
	}

	// The handler refuses two routes of one name, after the mint.
	twice := injector.Sandbox{ID: "brig-b", Profile: "claude-code",
		Routes: []injector.Route{gcpBrokerRoute(), gcpBrokerRoute()}}
	if ref := refusal(t, func() error { _, err := r.client.Add(twice); return err }()); ref.Status != http.StatusBadRequest {
		t.Fatalf("refusal = %d %q, want 400", ref.Status, ref.Message)
	}
	if n := len(f.calls(t)); n != 3 {
		t.Fatalf("the refused registration minted %d times in all, want 3", n)
	}
	if _, err := r.client.Add(gcpSandbox("brig-b")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(t)); n != 4 {
		t.Errorf("minted %d times in all, want 4: the token of the refused registration was kept", n)
	}
}

// The broker's own environment can name a Google account or a key file, from
// the shell that started the first brig. Either would decide which identity
// a sandbox gets over the pointer, so gcloud gets neither.
func TestTheBrokersEnvironmentDoesNotPickTheIdentity(t *testing.T) {
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "evil@x")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/evil.json")
	r, f := startGCPBroker(t, serverConfig{}, nil)
	if err := r.store.Create("gcp", []byte(gcpPointer)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Add(gcpSandbox("brig-a")); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(f.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	var sawPath bool
	for _, line := range strings.Split(string(blob), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if name == "CLOUDSDK_CORE_ACCOUNT" || name == "GOOGLE_APPLICATION_CREDENTIALS" {
			t.Errorf("the gcloud subprocess got %s", name)
		}
		sawPath = sawPath || name == "PATH"
	}
	// The env file is the fake's view; one with no PATH recorded nothing.
	if !sawPath {
		t.Errorf("the fake recorded no PATH, so this test checks nothing: %q", blob)
	}
}
