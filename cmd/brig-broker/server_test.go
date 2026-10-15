package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/renew"
	"github.com/brig-sh/brig/internal/secret"
)

// Fake values only. Each test that looks for a leak looks for these.
const (
	fakeAccess = "fake-access-token-for-the-broker-tests"
	fakeGitHub = "fake-github-token-for-the-broker-tests"
)

// fakeDoc is a claude-credentials document that expires in an hour, so no
// refresh is due.
func fakeDoc(access string, expires time.Time) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"fake-refresh","expiresAt":%d}}`,
		access, expires.UnixMilli()))
}

// memStore is an in-memory secret.Store. The broker tests never open the
// login keychain.
type memStore struct {
	mu     sync.Mutex
	values map[string][]byte
	reads  int
}

func newMemStore() *memStore {
	return &memStore{values: map[string][]byte{
		"claude-credentials": fakeDoc(fakeAccess, time.Now().Add(time.Hour)),
		"gh-token":           []byte(fakeGitHub),
	}}
}

func (m *memStore) Kind() string { return "memory" }
func (m *memStore) Create(name string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[name]; ok {
		return secret.ErrExists
	}
	m.values[name] = bytes.Clone(v)
	return nil
}
func (m *memStore) Read(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	v, ok := m.values[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return bytes.Clone(v), nil
}
func (m *memStore) Update(name string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[name]; !ok {
		return secret.ErrNotFound
	}
	m.values[name] = bytes.Clone(v)
	return nil
}
func (m *memStore) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, name)
	return nil
}
func (m *memStore) List() ([]secret.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []secret.Secret
	for name := range m.values {
		out = append(out, secret.Secret{Name: name})
	}
	return out, nil
}

// upstream is a TLS server that records what each request carried.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, r.Header.Clone())
		u.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) requests() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.seen)
}

// transport sends every upstream request to u, whatever origin the route
// names, and trusts u's certificate. It stands in for the system roots and
// for DNS, and nothing else: the broker still dials TLS and verifies.
func (u *upstream) transport() http.RoundTripper {
	addr := u.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(u.Certificate())
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}
}

// shortDir is a fresh 0700 directory with a path short enough for a unix
// socket under it.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// noRenewer fails the test if any refresh is attempted.
func noRenewer(t *testing.T) func(v1alpha1.Lifecycle) (secret.Renewer, error) {
	return func(v1alpha1.Lifecycle) (secret.Renewer, error) {
		return renewerFunc(func(context.Context, []byte) ([]byte, error) {
			t.Error("a refresh ran in a test whose credential is not due")
			return nil, errors.New("no refresh in this test")
		}), nil
	}
}

// renewerFunc is a Renewer of the claude-credentials shape: it reads the
// expiry the way the anthropic-oauth lifecycle does, and renews by calling
// itself instead of the provider.
type renewerFunc func(context.Context, []byte) ([]byte, error)

func (f renewerFunc) Renew(ctx context.Context, doc []byte) ([]byte, error) { return f(ctx, doc) }

func (renewerFunc) Expiry(doc []byte) (time.Time, error) { return renew.AnthropicOAuth{}.Expiry(doc) }

// running is a broker serving in this process.
type running struct {
	srv    *server
	client injector.Client
	dir    string
	store  *memStore
	done   chan error
	cancel context.CancelFunc
}

// startBroker serves a broker on dir with a memory store and upstream u.
func startBroker(t *testing.T, dir string, u *upstream, renewers func(v1alpha1.Lifecycle) (secret.Renewer, error)) *running {
	t.Helper()
	return startBrokerWith(t, serverConfig{dir: dir, renewerFor: renewers, logTo: io.Discard}, u)
}

// startBrokerWith serves cfg, with a memory store unless cfg opens its own,
// and upstream u.
func startBrokerWith(t *testing.T, cfg serverConfig, u *upstream) *running {
	t.Helper()
	store := newMemStore()
	if cfg.openStore == nil {
		cfg.openStore = func() (secret.Store, error) { return store, nil }
	}
	dir, renewers := cfg.dir, cfg.renewerFor
	if u != nil {
		cfg.transport = u.transport()
	}
	if renewers == nil {
		cfg.renewerFor = noRenewer(t)
	}
	srv, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{srv: srv, client: injector.Client{Socket: filepath.Join(dir, injector.SocketName)},
		dir: dir, store: store, done: make(chan error, 1), cancel: cancel}
	go func() { r.done <- srv.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

// claudeRoute is the claude-code route as policy.Routes hands it to brig.
func claudeRoute() injector.Route {
	r := v1alpha1.Route{Name: "anthropic", Preset: v1alpha1.PresetClaudeCode,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "claude-credentials"}}}
	return injector.Route{Policy: "claude-only", Route: r}
}

func githubRoute() injector.Route {
	r := v1alpha1.Route{Name: "github", Preset: v1alpha1.PresetGitHubToken,
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "gh-token"}}}
	return injector.Route{Policy: "github", Route: r}
}

// get sends one request to a route of the running broker.
func (r *running) send(t *testing.T, sandbox, route, token, path string) int {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/%s/r/%s%s", r.srv.port, sandbox, route, path)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (r *running) mint(t *testing.T, sandbox, route string) string {
	t.Helper()
	token, err := r.client.Mint(injector.MintRequest{Sandbox: sandbox, Route: route})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The control protocol end to end: a sandbox is registered, a token minted
// for it reaches the upstream with the credential in its place, and removing
// the sandbox refuses the token from the next request on.
func TestControlProtocolRegistersMintsAndRevokes(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "brig-a", "anthropic")
	if got := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); got != http.StatusOK {
		t.Fatalf("a request with the sandbox's token got %d", got)
	}
	seen := u.requests()
	if len(seen) != 1 || seen[0].Get("Authorization") != "Bearer "+fakeAccess {
		t.Fatalf("upstream saw %v, want the stored access token", seen)
	}
	for name, values := range seen[0] {
		for _, v := range values {
			if strings.Contains(v, token) {
				t.Errorf("the session token reached upstream in %s", name)
			}
		}
	}
	st, err := r.client.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != os.Getpid() || st.Port != r.srv.port || !slices.Equal(st.Sandboxes, []string{"brig-a"}) {
		t.Errorf("status = %+v", st)
	}
	removed, err := r.client.RemoveSandbox("brig-a")
	if err != nil || !removed.Removed || removed.Remaining != 0 {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	if got := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); got != http.StatusNotFound {
		t.Errorf("the token of a removed sandbox got %d, want 404", got)
	}
	if n := len(u.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want the one before the removal", n)
	}
}

// Registering the same routes again, as every exec does, keeps the tokens an
// agent already holds. A sandbox registered again with other routes is a new
// registration, and the tokens of the old one fail.
func TestReRegisteringKeepsTokensAndChangingRoutesRevokesThem(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	sb := injector.Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []injector.Route{claudeRoute()}}
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	token := r.mint(t, "brig-a", "anthropic")
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	if got := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); got != http.StatusOK {
		t.Errorf("a token minted before the same registration again got %d", got)
	}
	sb.Routes = append(sb.Routes, githubRoute())
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	if got := r.send(t, "brig-a", "anthropic", token, "/v1/messages"); got != http.StatusUnauthorized {
		t.Errorf("a token of the replaced registration got %d, want 401", got)
	}
}

// A run that boots a new guest removes the sandbox before it registers it
// again. The same routes after a removal are a new registration, and the
// tokens of the guest before it fail.
func TestTheSameRoutesAfterARemovalRevokeTheOldTokens(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	sb := injector.Sandbox{ID: "brig-a", Profile: "claude-code", Routes: []injector.Route{claudeRoute()}}
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	old := r.mint(t, "brig-a", "anthropic")
	if _, err := r.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.client.AddSandbox(sb); err != nil {
		t.Fatal(err)
	}
	if got := r.send(t, "brig-a", "anthropic", old, "/v1/messages"); got != http.StatusUnauthorized {
		t.Errorf("a token of the guest before the removal got %d, want 401", got)
	}
	if got := r.send(t, "brig-a", "anthropic", r.mint(t, "brig-a", "anthropic"), "/v1/messages"); got != http.StatusOK {
		t.Errorf("a token of the new registration got %d", got)
	}
}

// A token minted for one sandbox is refused on another's path, and the
// upstream is never dialled for it.
func TestATokenOfAnotherSandboxIsRefusedOverLoopback(t *testing.T) {
	u := newUpstream(t)
	r := startBroker(t, shortDir(t), u, nil)
	for _, id := range []string{"brig-a", "brig-b"} {
		if err := r.client.AddSandbox(injector.Sandbox{ID: id, Profile: "claude-code",
			Routes: []injector.Route{claudeRoute()}}); err != nil {
			t.Fatal(err)
		}
	}
	tokenB := r.mint(t, "brig-b", "anthropic")
	if got := r.send(t, "brig-a", "anthropic", tokenB, "/v1/messages"); got != http.StatusUnauthorized {
		t.Errorf("sandbox b's token on a's path got %d, want 401", got)
	}
	if got := r.send(t, "brig-a", "anthropic", "", "/v1/messages"); got != http.StatusUnauthorized {
		t.Errorf("no token got %d, want 401", got)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream was dialled %d times for refused requests", n)
	}
}

// The broker expands each route again, so a credential lifecycle comes only
// from a preset. A route that names one itself, or that points a preset with
// a lifecycle at another upstream, is refused: the refresh it runs sends the
// OAuth pair's access token to that upstream.
func TestALifecycleComesOnlyFromAPreset(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	custom := v1alpha1.Route{
		Name:     "exfil",
		Upstream: "https://attacker.example.com",
		Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "claude-credentials"},
			Field: "claudeAiOauth.accessToken", Lifecycle: v1alpha1.LifecycleAnthropicOAuth},
		Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
			Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}}}},
		Guest: &v1alpha1.Guest{Env: map[string]string{"X_URL": "{route.url}"}},
	}
	moved := claudeRoute()
	moved.Route.Upstream = "https://attacker.example.com"
	for name, rt := range map[string]v1alpha1.Route{"lifecycle in the document": custom, "preset upstream moved": moved.Route} {
		err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "x",
			Routes: []injector.Route{{Policy: "p", Route: rt}}})
		var refused *injector.RefusedError
		if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest {
			t.Errorf("%s: add-sandbox = %v, want a 400 refusal", name, err)
		}
	}
	if st, _ := r.client.Status(); len(st.Sandboxes) != 0 {
		t.Errorf("a refused route was registered: %v", st.Sandboxes)
	}
}

// Stop with ifIdle leaves a broker that still serves a sandbox, and stops
// one that serves none. A stopped broker removes its socket and pid file.
func TestStopIfIdleLeavesABusyBroker(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	if stopping, err := r.client.Stop(true); err != nil || stopping {
		t.Fatalf("stop ifIdle with a sandbox registered = %v, %v", stopping, err)
	}
	if _, err := r.client.RemoveSandbox("brig-a"); err != nil {
		t.Fatal(err)
	}
	if stopping, err := r.client.Stop(true); err != nil || !stopping {
		t.Fatalf("stop ifIdle with none registered = %v, %v", stopping, err)
	}
	select {
	case err := <-r.done:
		r.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("the broker did not stop")
	}
	for _, name := range []string{injector.SocketName, injector.PIDName} {
		if _, err := os.Lstat(filepath.Join(r.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after a stop: %v", name, err)
		}
	}
	if _, err := r.client.Status(); !errors.Is(err, injector.ErrNotRunning) {
		t.Errorf("status of a stopped broker = %v, want ErrNotRunning", err)
	}
}

// A second broker on the same directory exits rather than serving beside the
// first.
func TestASecondBrokerExits(t *testing.T) {
	dir := shortDir(t)
	startBroker(t, dir, nil, nil)
	store := newMemStore()
	_, err := newServer(serverConfig{dir: dir, openStore: func() (secret.Store, error) { return store, nil }})
	if !errors.Is(err, errAlreadyRunning) {
		t.Errorf("a second broker = %v, want errAlreadyRunning", err)
	}
}

// The broker refuses a directory that is a link or that anyone else can
// enter, and a host key that anyone else can read.
func TestTheBrokerRefusesADirectoryOrKeyOthersCanReach(t *testing.T) {
	open := func() (secret.Store, error) { return newMemStore(), nil }
	dir := shortDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(serverConfig{dir: dir, openStore: open}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a 0755 directory = %v, want a refusal", err)
	}
	real := shortDir(t)
	link := filepath.Join(shortDir(t), "broker")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(serverConfig{dir: link, openStore: open}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("a linked directory = %v, want a refusal", err)
	}
	keyed := shortDir(t)
	if err := os.WriteFile(filepath.Join(keyed, injector.HostKeyName), bytes.Repeat([]byte{1}, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(serverConfig{dir: keyed, openStore: open}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a 0644 host key = %v, want a refusal", err)
	}
}

// The host key is drawn once, kept 0600, and read again by the next broker,
// so a restart does not change the key the sandbox keys derive from.
func TestTheHostKeyIsPrivateAndOutlivesARestart(t *testing.T) {
	dir := shortDir(t)
	r := startBroker(t, dir, nil, nil)
	path := filepath.Join(dir, injector.HostKeyName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != hostKeySize {
		t.Errorf("host key mode %v size %d", info.Mode().Perm(), info.Size())
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{injector.SocketName, injector.AuditName, injector.CAKeyName, injector.PIDName} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
	}
	r.cancel()
	<-r.done
	r.done <- nil
	again, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Error("the host key changed across a restart")
	}
}

// The brig CA signs brig.internal and nothing else. A guest that trusts it
// does not trust a certificate it signs for an upstream's name or for an
// address, so the broker could not stand in for api.anthropic.com even if it
// tried.
func TestTheBrigCASignsOnlyBrigInternal(t *testing.T) {
	dir := shortDir(t)
	a, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	leaf, err := x509.ParseCertificate(a.leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "brig.internal", Roots: pool}); err != nil {
		t.Errorf("the brig.internal certificate does not verify: %v", err)
	}
	for _, name := range []string{"api.anthropic.com", "github.com", "evil.brig.internal.example.com"} {
		forged, err := a.sign(name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(forged.Certificate[0])
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err == nil {
			t.Errorf("a certificate the brig CA signed for %s verifies", name)
		}
	}
	// A TLS client that trusts the CA completes a handshake with the
	// broker's configuration for brig.internal.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", a.serverConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "brig.internal"})
	if err != nil {
		t.Fatalf("a client that trusts the brig CA could not reach brig.internal: %v", err)
	}
	_ = conn.Close()
	// The CA key is 0600 and the next start reads the same CA.
	if info, err := os.Stat(filepath.Join(dir, injector.CAKeyName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("CA key: %v %v", info, err)
	}
	b, err := loadOrCreateCA(dir)
	if err != nil || !b.cert.Equal(a.cert) {
		t.Errorf("the CA changed across a restart: %v", err)
	}
}

// Two sandboxes that share a credential share its refresh: a document due
// for renewal is renewed once, whichever sandbox's request finds it first,
// and the store holds the renewed document after.
func TestTwoSandboxesShareOneRefresh(t *testing.T) {
	u := newUpstream(t)
	var renewals atomic.Int32
	renewers := func(v1alpha1.Lifecycle) (secret.Renewer, error) {
		return renewerFunc(func(context.Context, []byte) ([]byte, error) {
			renewals.Add(1)
			time.Sleep(50 * time.Millisecond)
			return fakeDoc("fake-access-token-renewed", time.Now().Add(time.Hour)), nil
		}), nil
	}
	r := startBroker(t, shortDir(t), u, renewers)
	r.store.values["claude-credentials"] = fakeDoc(fakeAccess, time.Now().Add(time.Minute))
	tokens := map[string]string{}
	for _, id := range []string{"brig-a", "brig-b"} {
		if err := r.client.AddSandbox(injector.Sandbox{ID: id, Profile: "claude-code",
			Routes: []injector.Route{claudeRoute()}}); err != nil {
			t.Fatal(err)
		}
		tokens[id] = r.mint(t, id, "anthropic")
	}
	var wg sync.WaitGroup
	for id, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := r.send(t, id, "anthropic", token, "/v1/messages"); got != http.StatusOK {
				t.Errorf("%s got %d", id, got)
			}
		}()
	}
	wg.Wait()
	if n := renewals.Load(); n != 1 {
		t.Errorf("%d refreshes for two sandboxes sharing one credential, want 1", n)
	}
	for _, h := range u.requests() {
		if h.Get("Authorization") != "Bearer fake-access-token-renewed" {
			t.Errorf("upstream saw %q, want the renewed token", h.Get("Authorization"))
		}
	}
	stored, _ := r.store.Read("claude-credentials")
	if !bytes.Contains(stored, []byte("fake-access-token-renewed")) {
		t.Error("the renewed document was not written back to the store")
	}
}

// The audit log has a line per request, and no line holds the credential or
// a session token.
func TestTheAuditLogHoldsNoValueOrToken(t *testing.T) {
	u := newUpstream(t)
	dir := shortDir(t)
	r := startBroker(t, dir, u, nil)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute(), githubRoute()}}); err != nil {
		t.Fatal(err)
	}
	tokens := []string{r.mint(t, "brig-a", "anthropic"), r.mint(t, "brig-a", "github")}
	r.send(t, "brig-a", "anthropic", tokens[0], "/v1/messages")
	r.send(t, "brig-a", "anthropic", tokens[1], "/v1/messages")
	blob, err := os.ReadFile(filepath.Join(dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blob, []byte("sandbox=brig-a")) || bytes.Count(blob, []byte("\n")) != 2 {
		t.Errorf("the audit log does not have a line per request:\n%s", blob)
	}
	for _, s := range append(tokens, fakeAccess, fakeGitHub, "fake-refresh") {
		if bytes.Contains(blob, []byte(s)) {
			t.Errorf("the audit log holds %q", s)
		}
	}
}

// A malformed or unknown control request is refused, and so is a field the
// protocol does not have.
func TestControlRequestsAreStrict(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.client.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, body := range []string{`{"id":"brig-a","profile":"x","routes":[],"extra":1}`, `not json`} {
		resp, err := client.Post("http://broker/sandboxes", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s got %d, want 400", body, resp.StatusCode)
		}
	}
	var refused *injector.RefusedError
	if _, err := r.client.Mint(injector.MintRequest{Sandbox: "brig-none", Route: "anthropic"}); !errors.As(err, &refused) ||
		refused.Status != http.StatusNotFound {
		t.Errorf("a mint for a sandbox nobody registered = %v, want a 404 refusal", err)
	}
	// A sandbox id is a plain name on every request that carries one.
	for _, id := range []string{"..", "a b", "a\nb", strings.Repeat("a", 256)} {
		err := r.client.AddSandbox(injector.Sandbox{ID: id, Profile: "x", Routes: []injector.Route{claudeRoute()}})
		if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest {
			t.Errorf("add-sandbox %q = %v, want 400", id, err)
		}
		if _, err := r.client.Mint(injector.MintRequest{Sandbox: id, Route: "anthropic"}); !errors.As(err, &refused) ||
			refused.Status != http.StatusBadRequest {
			t.Errorf("mint for %q = %v, want 400", id, err)
		}
	}
	resp, err := client.Do(mustRequest(t, http.MethodDelete, "http://broker/sandboxes/a%20b"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("remove-sandbox of %q got %d, want 400", "a b", resp.StatusCode)
	}
	var st injector.Status
	resp, err = client.Get("http://broker/status")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if st.PID != os.Getpid() || st.Port != r.srv.port || len(st.Sandboxes) != 0 {
		t.Errorf("status after refused requests = %+v, want this broker with no sandbox", st)
	}
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Anything on the host can send the broker a request with no token, so what
// such a request writes is bounded: its audit line carries no path, and each
// log is rotated at its cap with one previous file kept. A request line or
// headers over 16 KiB are refused before the handler. A request with a valid
// token still logs its full line.
func TestAFloodOfUnverifiedRequestsIsBounded(t *testing.T) {
	const cap = 32 << 10
	u := newUpstream(t)
	dir := shortDir(t)
	r := startBrokerWith(t, serverConfig{dir: dir, logTo: io.Discard, logMax: cap}, u)
	long := "/" + strings.Repeat("x", 8<<10)
	client := &http.Client{}
	for i := 0; i < 2000; i++ {
		resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/brig-a/r/anthropic%s", r.srv.port, long),
			"application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	big, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", r.srv.port), nil)
	if err != nil {
		t.Fatal(err)
	}
	big.Header.Set("X-Pad", strings.Repeat("p", 24<<10))
	resp, err := client.Do(big)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a 24 KiB header got %d, want 431", resp.StatusCode)
	}
	var total int64
	for _, name := range []string{injector.AuditName, injector.AuditName + ".1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() > cap {
			t.Errorf("%s is %d bytes, over the cap of %d", name, info.Size(), cap)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
		total += info.Size()
	}
	blob, err := os.ReadFile(filepath.Join(dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("xxxx")) || bytes.Contains(blob, []byte("path=")) {
		t.Error("an unverified request wrote its path into the audit log")
	}
	t.Logf("2000 unverified requests with 8 KiB paths: %d bytes of audit log across two files, cap %d each", total, cap)

	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute()}}); err != nil {
		t.Fatal(err)
	}
	if got := r.send(t, "brig-a", "anthropic", r.mint(t, "brig-a", "anthropic"), "/v1/messages"); got != http.StatusOK {
		t.Fatalf("an authenticated request got %d", got)
	}
	blob, err = os.ReadFile(filepath.Join(dir, injector.AuditName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(blob)), "\n")
	last := lines[len(lines)-1]
	for _, want := range []string{"workload=", "sandbox=brig-a", "route=anthropic", "path=/v1/messages", "status=200"} {
		if !strings.Contains(last, want) {
			t.Errorf("the authenticated request's audit line lacks %q: %s", want, last)
		}
	}
}

// The broker binds the port recorded in its directory, and records the one
// it picks at first start whole. Another process on the recorded port stops
// the start: the broker never moves.
func TestTheBrokerKeepsItsRecordedPort(t *testing.T) {
	dir := shortDir(t)
	r := startBroker(t, dir, nil, nil)
	blob, err := os.ReadFile(filepath.Join(dir, injector.PortName))
	if err != nil || strings.TrimSpace(string(blob)) != fmt.Sprint(r.srv.port) {
		t.Fatalf("port file %q (%v), bound %d", blob, err, r.srv.port)
	}
	if _, err := os.Stat(filepath.Join(dir, injector.PortName+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary port file is left behind: %v", err)
	}
	r.cancel()
	<-r.done
	r.done <- nil
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", r.srv.port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	_, err = newServer(serverConfig{dir: dir, openStore: func() (secret.Store, error) { return newMemStore(), nil },
		logTo: io.Discard})
	var held *portHeldError
	if !errors.As(err, &held) || held.port != r.srv.port || !strings.Contains(err.Error(), injector.PortHeldMessage) {
		t.Fatalf("a start with the recorded port held = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, injector.SocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a broker that refused to start left a control socket")
	}
}

// A start that stopped between the two files of the CA leaves one without
// the other. The next start makes both again instead of failing every start.
func TestAHalfWrittenCAIsMadeAgain(t *testing.T) {
	for _, gone := range []string{injector.CACertName, injector.CAKeyName} {
		dir := shortDir(t)
		first, err := loadOrCreateCA(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, gone)); err != nil {
			t.Fatal(err)
		}
		again, err := loadOrCreateCA(dir)
		if err != nil {
			t.Fatalf("with %s gone: %v", gone, err)
		}
		if again.cert.Equal(first.cert) {
			t.Errorf("with %s gone, the old CA was kept", gone)
		}
		if _, err := readCA(filepath.Join(dir, injector.CACertName), filepath.Join(dir, injector.CAKeyName)); err != nil {
			t.Errorf("with %s gone, the new pair does not read back: %v", gone, err)
		}
	}
}

// A registration that fails leaves no refresh cache behind for its secret.
func TestAFailedRegistrationLeavesNoCache(t *testing.T) {
	r := startBroker(t, shortDir(t), nil, nil)
	twice := claudeRoute()
	err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute(), twice}})
	if err == nil {
		t.Fatal("a sandbox with the same route twice was registered")
	}
	r.srv.creds.mu.Lock()
	n := len(r.srv.creds.caches)
	r.srv.creds.mu.Unlock()
	if n != 0 {
		t.Errorf("%d refresh caches left after a failed registration", n)
	}
}

// A host whose secret store does not open, a Linux host with no Secret
// Service, still runs the broker. Sandboxes register and tokens are minted.
// A route answers 502 with the reason, and reaches no upstream, until the
// store opens. Then the same token works.
func TestABrokerWithNoSecretStoreStarts(t *testing.T) {
	u := newUpstream(t)
	store := newMemStore()
	var available atomic.Bool
	dir := shortDir(t)
	r := startBrokerWith(t, serverConfig{dir: dir, logTo: io.Discard, openStore: func() (secret.Store, error) {
		if !available.Load() {
			return nil, fmt.Errorf("%w: a D-Bus session bus is running but no Secret Service answers on it",
				secret.ErrUnsupported)
		}
		return store, nil
	}}, u)
	if err := r.client.AddSandbox(injector.Sandbox{ID: "brig-a", Profile: "claude-code",
		Routes: []injector.Route{claudeRoute(), githubRoute()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Status(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"anthropic", "github"} {
		token := r.mint(t, "brig-a", route)
		url := fmt.Sprintf("http://127.0.0.1:%d/brig-a/r/%s/v1/messages", r.srv.port, route)
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "secret store") {
			t.Errorf("route %s with no store: %d %q, want 502 naming the secret store", route, resp.StatusCode, body)
		}
		audit, err := os.ReadFile(filepath.Join(dir, injector.AuditName))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(audit)), "\n")
		if last := lines[len(lines)-1]; !strings.Contains(last, "route="+route) || !strings.Contains(last, "check=credential") {
			t.Errorf("route %s with no store: audit line %q, want check=credential", route, last)
		}
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("the upstream saw %d requests with no credential to send", n)
	}
	available.Store(true)
	if got := r.send(t, "brig-a", "anthropic", r.mint(t, "brig-a", "anthropic"), "/v1/messages"); got != http.StatusOK {
		t.Errorf("once the store opens, the route answers %d", got)
	}
}

// A CA key that is not the key of the CA certificate beside it is refused
// with both files named and the way out, instead of failing every start
// with an x509 error.
func TestAMismatchedCAIsRefusedWithTheRemedy(t *testing.T) {
	a, b := shortDir(t), shortDir(t)
	for _, dir := range []string{a, b} {
		if _, err := loadOrCreateCA(dir); err != nil {
			t.Fatal(err)
		}
	}
	other, err := os.ReadFile(filepath.Join(b, injector.CACertName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, injector.CACertName), other, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loadOrCreateCA(a)
	if err == nil {
		t.Fatal("a CA certificate with another CA's key was loaded")
	}
	for _, want := range []string{filepath.Join(a, injector.CACertName), filepath.Join(a, injector.CAKeyName), "Remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// A log that cannot rotate keeps taking lines in the file it has, says so
// once, and rotates at a later write once it can.
func TestALogThatCannotRotateKeepsWriting(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, injector.AuditName)
	r, err := openRotating(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var warned bytes.Buffer
	r.warn = &warned
	// A directory where the previous log goes makes the rename fail.
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first\n", "second\n", "third\n"} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("writing %q with the rotation failing: %v", line, err)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "first\nsecond\nthird\n" {
		t.Errorf("the log holds %q", got)
	}
	if n := strings.Count(warned.String(), "\n"); n != 1 || !strings.Contains(warned.String(), path) {
		t.Errorf("the failed rotation was reported %d times: %q", n, warned.String())
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("fourth\n")); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path + ".1")
	cur, _ := os.ReadFile(path)
	if string(old) != "first\nsecond\nthird\n" || string(cur) != "fourth\n" {
		t.Errorf("after the rotation: previous %q, current %q", old, cur)
	}
}

// stderrChildEnv makes this test binary the child of
// TestStderrFollowsTheRotatedLog.
const stderrChildEnv = "BRIG_BROKER_TEST_STDERR_LOG"

// What the broker writes to its stderr, a panic included, lands in the
// broker log it writes now, not in the one it rotated away.
func TestStderrFollowsTheRotatedLog(t *testing.T) {
	if path := os.Getenv(stderrChildEnv); path != "" {
		r, err := openRotating(path, 8)
		if err != nil {
			t.Fatal(err)
		}
		r.reopened = followStderr
		if _, err := r.Write([]byte("before the rotation\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Write([]byte("rotated\n")); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(os.Stderr, "written to stderr after the rotation")
		os.Exit(0)
	}
	path := filepath.Join(shortDir(t), injector.LogName)
	// brig hands the broker the log as its stdout and stderr.
	log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStderrFollowsTheRotatedLog$")
	cmd.Env = append(os.Environ(), stderrChildEnv+"="+path)
	cmd.Stdout, cmd.Stderr = log, log
	err = cmd.Run()
	_ = log.Close()
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if !strings.Contains(string(cur), "written to stderr after the rotation") {
		t.Errorf("stderr after the rotation is not in the current log %q; the previous one holds %q", cur, old)
	}
}
