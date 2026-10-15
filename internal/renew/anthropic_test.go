package renew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// No test in this file prints a token. A failure names the field that is
// wrong and never its value, so a test log holds no credential even when the
// values are fake.

// The tokens of the stored document. The fake endpoint issues
// "<prefix>-<n>" pairs after it.
const (
	storedAccess  = "sk-ant-oat01-stored-access"
	storedRefresh = "sk-ant-ort01-stored-refresh"
)

// claudeCredentials renders a .credentials.json the way Claude Code writes
// one, plus a top-level field brig knows nothing about.
func claudeCredentials(t *testing.T, access, refresh string, expires time.Time) []byte {
	t.Helper()
	doc := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":           access,
			"refreshToken":          refresh,
			"expiresAt":             expires.UnixMilli(),
			"refreshTokenExpiresAt": int64(1797000000000),
			"scopes":                []string{"user:inference", "user:profile"},
			"subscriptionType":      "max",
			"rateLimitTier":         "default_claude_max_20x",
		},
		"mcpOAuth": map[string]any{"server": map[string]any{"note": "a <b> & c"}},
	}
	// Unescaped, the way Claude Code writes the file.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// tokenEndpoint is a fake provider token endpoint. It rotates the refresh
// token single-use, like the real one: a refresh token it has seen before is
// refused and counted as a reuse.
type tokenEndpoint struct {
	mu     sync.Mutex
	calls  int
	issued int
	valid  string
	sent   map[string]int
	bad    []string
	// respond, when set, writes the answer in place of a pair.
	respond func(w http.ResponseWriter)
	// rotate makes respond run after the token was rotated, like a
	// provider whose answer is lost or mangled on the way back.
	rotate bool
	// started, when set, is closed by the first request to arrive.
	started chan struct{}
	// release, when set, holds every request until it is closed.
	release chan struct{}
	// expiresIn is the expires_in of an issued pair, 8 hours when zero.
	expiresIn int
}

func newTokenEndpoint(t *testing.T) (*tokenEndpoint, *httptest.Server) {
	e := &tokenEndpoint{valid: storedRefresh, sent: map[string]int{}}
	srv := httptest.NewTLSServer(e)
	t.Cleanup(srv.Close)
	return e, srv
}

var userAgentForm = regexp.MustCompile(`^claude-cli/[0-9][0-9A-Za-z.\-]* \(external, cli\)$`)

func (e *tokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.calls++
	if e.started != nil {
		close(e.started)
		e.started = nil
	}
	release := e.release
	e.mu.Unlock()
	if release != nil {
		<-release
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/oauth/token" {
		e.bad = append(e.bad, "method or path")
	}
	if r.Header.Get("Content-Type") != "application/json" {
		e.bad = append(e.bad, "Content-Type")
	}
	if !userAgentForm.MatchString(r.Header.Get("User-Agent")) {
		e.bad = append(e.bad, "User-Agent")
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		e.bad = append(e.bad, "body, not a JSON object of strings")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req) != 3 || req["grant_type"] != "refresh_token" ||
		req["client_id"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		e.bad = append(e.bad, "body fields")
	}
	rt := req["refresh_token"]
	e.sent[rt]++
	if e.respond != nil && !e.rotate {
		e.respond(w)
		return
	}
	if rt != e.valid {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
		return
	}
	e.issued++
	e.valid = fmt.Sprintf("sk-ant-ort01-issued-%d", e.issued)
	if e.respond != nil {
		e.respond(w)
		return
	}
	expiresIn := e.expiresIn
	if expiresIn == 0 {
		expiresIn = 28800
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"token_type":"Bearer","access_token":"sk-ant-oat01-issued-%d",`+
		`"refresh_token":%q,"expires_in":%d,"scope":"user:inference user:profile"}`,
		e.issued, e.valid, expiresIn)
}

func (e *tokenEndpoint) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *tokenEndpoint) timesSent(refresh string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sent[refresh]
}

// check fails the test for any malformed request and for any refresh token
// sent twice.
func (e *tokenEndpoint) check(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, b := range e.bad {
		t.Errorf("the token endpoint got a request with a wrong %s", b)
	}
	for _, n := range e.sent {
		if n > 1 {
			t.Errorf("a refresh token was sent %d times", n)
		}
	}
}

// oauthFields decodes claudeAiOauth and the rest of a document for
// comparison.
func oauthFields(t *testing.T, doc []byte) (map[string]any, map[string]any) {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatal("the document is not JSON")
	}
	oauth, ok := top["claudeAiOauth"].(map[string]any)
	if !ok {
		t.Fatal("the document has no claudeAiOauth object")
	}
	return top, oauth
}

func testOAuth(srv *httptest.Server, clock *fakeClock) AnthropicOAuth {
	return AnthropicOAuth{tokenURL: srv.URL + "/v1/oauth/token", client: srv.Client(), now: clock.Now}
}

func TestAnthropicRefreshKeepsEveryOtherField(t *testing.T) {
	e, srv := newTokenEndpoint(t)
	clock := newFakeClock()
	before := claudeCredentials(t, storedAccess, storedRefresh, clock.Now().Add(time.Minute))
	after, err := testOAuth(srv, clock).Renew(context.Background(), before)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	e.check(t)

	oldTop, oldOAuth := oauthFields(t, before)
	newTop, newOAuth := oauthFields(t, after)
	if newOAuth["accessToken"] != "sk-ant-oat01-issued-1" {
		t.Error("accessToken is not the issued one")
	}
	if newOAuth["refreshToken"] != "sk-ant-ort01-issued-1" {
		t.Error("refreshToken is not the issued one")
	}
	if want := float64(clock.Now().Add(8 * time.Hour).UnixMilli()); newOAuth["expiresAt"] != want {
		t.Errorf("expiresAt = %v, want %v", newOAuth["expiresAt"], want)
	}
	for _, k := range []string{"accessToken", "refreshToken", "expiresAt"} {
		delete(oldOAuth, k)
		delete(newOAuth, k)
	}
	if fmt.Sprint(oldOAuth) != fmt.Sprint(newOAuth) {
		t.Errorf("claudeAiOauth fields changed: %v became %v", oldOAuth, newOAuth)
	}
	delete(oldTop, "claudeAiOauth")
	delete(newTop, "claudeAiOauth")
	if fmt.Sprint(oldTop) != fmt.Sprint(newTop) {
		t.Errorf("top-level fields changed: %v became %v", oldTop, newTop)
	}
	if !bytes.Contains(after, []byte(`"a <b> & c"`)) {
		t.Error("an unrelated field was re-escaped")
	}
}

func TestAnthropicExpiry(t *testing.T) {
	clock := newFakeClock()
	at := clock.Now().Add(5 * time.Minute)
	got, err := AnthropicOAuth{}.Expiry(claudeCredentials(t, storedAccess, storedRefresh, at))
	if err != nil || !got.Equal(at) {
		t.Errorf("Expiry = %v, %v; want %v", got, err, at)
	}
	for _, none := range []string{`{"claudeAiOauth":{"accessToken":"x"}}`, `{"claudeAiOauth":{"expiresAt":0}}`} {
		got, err = AnthropicOAuth{}.Expiry([]byte(none))
		if err != nil || !got.IsZero() {
			t.Errorf("Expiry of %s = %v, %v; want the zero time", none, got, err)
		}
	}
	for _, bad := range []string{`[]`, `{}`, `{"claudeAiOauth":"x"}`, `{"claudeAiOauth":{"expiresAt":"soon"}}`} {
		if _, err := (AnthropicOAuth{}).Expiry([]byte(bad)); err == nil {
			t.Errorf("Expiry accepted %s", bad)
		}
	}
}

// Every answer that is not a 200 with both tokens is refused. The error
// holds nothing from the body, and marks the refresh token as spent: the
// request reached the provider. Several of the bodies carry a token.
func TestAnthropicRefusesAnUnexpectedAnswer(t *testing.T) {
	const leak = "sk-ant-oat01-LEAKED"
	pair := `"access_token":"` + leak + `","refresh_token":"sk-ant-ort01-LEAKED","expires_in":28800`
	cases := map[string]struct {
		status int
		body   string
	}{
		"400 invalid_grant":  {400, `{"error":"invalid_grant",` + pair + `}`},
		"500":                {500, leak},
		"200 not JSON":       {200, "sk-ant-oat01-LEAKED is not JSON"},
		"200 array":          {200, `["` + leak + `"]`},
		"200 null":           {200, `null`},
		"no refresh token":   {200, `{"access_token":"` + leak + `","expires_in":28800}`},
		"no access token":    {200, `{"refresh_token":"` + leak + `","expires_in":28800}`},
		"empty access token": {200, `{"access_token":"","refresh_token":"r","expires_in":28800}`},
		"token with a space": {200, `{"access_token":"a b","refresh_token":"r","expires_in":28800}`},
		"token as a number":  {200, `{"access_token":7,"refresh_token":"` + leak + `","expires_in":28800}`},
		"renamed fields":     {200, `{"accessToken":"` + leak + `","refreshToken":"r","expiresIn":28800}`},
		// A complete pair, so only the size limit refuses it.
		"oversized": {200, `{` + pair + `,"padding":"` + strings.Repeat("a", maxTokenResponse) + `"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e, srv := newTokenEndpoint(t)
			e.respond = func(w http.ResponseWriter) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}
			clock := newFakeClock()
			doc := claudeCredentials(t, storedAccess, storedRefresh, clock.Now())
			got, err := testOAuth(srv, clock).Renew(context.Background(), doc)
			if err == nil || got != nil {
				t.Fatal("the answer was accepted")
			}
			if !errors.Is(err, secret.ErrSpent) {
				t.Error("a refusal after the request was sent is not marked spent")
			}
			for _, s := range []string{"LEAKED", storedAccess, storedRefresh, "28800", "invalid_grant"} {
				if strings.Contains(err.Error(), s) {
					t.Error("the error carries part of the body or the document")
				}
			}
		})
	}
}

// The tokens are what the provider rotated. An expiry brig cannot use does
// not void them: the pair is kept with expiresAt 0 and renewed on a 401.
func TestAnthropicKeepsAPairWithAnUnusableExpiry(t *testing.T) {
	for name, expiresIn := range map[string]string{
		"absent":         ``,
		"a string":       `,"expires_in":"28800"`,
		"zero":           `,"expires_in":0`,
		"under a minute": `,"expires_in":59`,
		"over 30 days":   `,"expires_in":2592001`,
		"overflow":       `,"expires_in":9300000000`,
		"a fraction":     `,"expires_in":28800.5`,
	} {
		t.Run(name, func(t *testing.T) {
			e, srv := newTokenEndpoint(t)
			e.respond = func(w http.ResponseWriter) {
				fmt.Fprint(w, `{"access_token":"sk-ant-oat01-new","refresh_token":"sk-ant-ort01-new"`+expiresIn+`}`)
			}
			clock := newFakeClock()
			doc := claudeCredentials(t, storedAccess, storedRefresh, clock.Now())
			got, err := testOAuth(srv, clock).Renew(context.Background(), doc)
			if err != nil {
				t.Fatalf("Renew: %v", err)
			}
			_, oauth := oauthFields(t, got)
			if oauth["accessToken"] != "sk-ant-oat01-new" || oauth["refreshToken"] != "sk-ant-ort01-new" {
				t.Error("the pair was not kept")
			}
			if oauth["expiresAt"] != float64(0) {
				t.Errorf("expiresAt = %v, want 0", oauth["expiresAt"])
			}
			if at, err := (AnthropicOAuth{}).Expiry(got); err != nil || !at.IsZero() {
				t.Errorf("Expiry = %v, %v; want the zero time", at, err)
			}
		})
	}
}

// A request that never reached the provider spent nothing, so it can be
// tried again.
func TestAnthropicUnsentRequestIsNotSpent(t *testing.T) {
	_, srv := newTokenEndpoint(t)
	o := AnthropicOAuth{tokenURL: srv.URL + "/v1/oauth/token", client: srv.Client()}
	srv.Close()
	doc := claudeCredentials(t, storedAccess, storedRefresh, time.Now())
	_, err := o.Renew(context.Background(), doc)
	if err == nil || errors.Is(err, secret.ErrSpent) {
		t.Errorf("Renew against a closed endpoint = %v, want an error that is not spent", err)
	}
}

// A body cut short is a failure after the provider answered, and it may
// have rotated the token.
func TestAnthropicBodyCutShortIsSpent(t *testing.T) {
	e, srv := newTokenEndpoint(t)
	e.rotate = true
	e.respond = func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"access_token":"sk-ant-oat01-x"`)
	}
	clock := newFakeClock()
	doc := claudeCredentials(t, storedAccess, storedRefresh, clock.Now())
	if _, err := testOAuth(srv, clock).Renew(context.Background(), doc); !errors.Is(err, secret.ErrSpent) {
		t.Errorf("Renew with a body cut short = %v, want a spent error", err)
	}
}

// An endpoint that takes the request and never answers may have rotated the
// token before it stalled. The refresh times out as spent, the error holds
// no token, and the refresh token is not sent again.
func TestAnthropicEndpointThatNeverAnswersIsSpent(t *testing.T) {
	e, srv := newTokenEndpoint(t)
	e.release = make(chan struct{})
	t.Cleanup(func() { close(e.release) })
	client := *srv.Client()
	client.Timeout = 200 * time.Millisecond
	clock := newFakeClock()
	oauth := AnthropicOAuth{tokenURL: srv.URL + "/v1/oauth/token", client: &client, now: clock.Now}

	doc := claudeCredentials(t, storedAccess, storedRefresh, clock.Now().Add(5*time.Minute))
	_, err := oauth.Renew(context.Background(), doc)
	if !errors.Is(err, secret.ErrSpent) {
		t.Fatalf("Renew against an endpoint that never answers = %v, want a spent error", err)
	}
	if msg := err.Error(); strings.Contains(msg, storedRefresh) || strings.Contains(msg, storedAccess) {
		t.Error("the error holds a token")
	}

	store := newMemStore()
	if err := store.Write(credName, doc, importedFrom, false); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(secret.StoreBroker(store), store, map[string]secret.Renewer{credName: oauth})
	cache.now = clock.Now
	got, err := cache.Resolve(context.Background(), credName, testWorkload)
	if err != nil || !bytes.Equal(got, doc) {
		t.Fatalf("Resolve = %v; want the current document, which has not expired", err)
	}
	for range 3 {
		clock.Advance(retryAfter)
		if _, err := cache.Resolve(context.Background(), credName, testWorkload); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if _, err := cache.ForceRenew(context.Background(), credName, testWorkload, doc); err == nil {
		t.Error("ForceRenew sent the refresh token again")
	}
	// One request from Renew above, one from the cache.
	if n := e.callCount(); n != 2 {
		t.Errorf("the token endpoint got %d requests, want 2", n)
	}
	if !slices.Equal(cache.Failing(), []string{credName}) {
		t.Error("the spent secret is not reported as failing")
	}
}

// "System roots and full verification" holds only over HTTPS.
func TestAnthropicRefusesAPlainHTTPEndpoint(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer plain.Close()
	o := AnthropicOAuth{tokenURL: plain.URL + "/v1/oauth/token", client: plain.Client()}
	doc := claudeCredentials(t, storedAccess, storedRefresh, time.Now())
	if _, err := o.Renew(context.Background(), doc); err == nil {
		t.Error("a plain http token endpoint was used")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the plain endpoint got %d request(s)", n)
	}
}

// A 307 or 308 would send the refresh token again, to whatever the Location
// header names.
func TestAnthropicRefusesARedirect(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	e, srv := newTokenEndpoint(t)
	e.respond = func(w http.ResponseWriter) {
		w.Header().Set("Location", other.URL+"/v1/oauth/token")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	clock := newFakeClock()
	doc := claudeCredentials(t, storedAccess, storedRefresh, clock.Now())
	if _, err := testOAuth(srv, clock).Renew(context.Background(), doc); err == nil {
		t.Error("a redirect was followed to a token")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect target got %d request(s)", n)
	}
}

// oauthFixture is the cache over a fake store holding an imported
// claude-credentials document, renewed against the fake token endpoint.
type oauthFixture struct {
	clock    *fakeClock
	store    *memStore
	endpoint *tokenEndpoint
	cache    *Cache
	initial  []byte
}

func newOAuthFixture(t *testing.T, expiresIn time.Duration) *oauthFixture {
	t.Helper()
	e, srv := newTokenEndpoint(t)
	clock := newFakeClock()
	store := newMemStore()
	initial := claudeCredentials(t, storedAccess, storedRefresh, clock.Now().Add(expiresIn))
	prov := secret.Provenance{V: secret.ProvenanceVersion, From: "keychain:Claude Code-credentials",
		ExpiresAt: clock.Now().Add(expiresIn).UnixMilli()}
	if err := store.Write(credName, initial, prov, false); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(secret.StoreBroker(store), store, map[string]secret.Renewer{credName: testOAuth(srv, clock)})
	cache.now = clock.Now
	return &oauthFixture{clock, store, e, cache, initial}
}

func (f *oauthFixture) resolve(t *testing.T) []byte {
	t.Helper()
	doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return doc
}

func accessToken(t *testing.T, doc []byte) string {
	t.Helper()
	_, oauth := oauthFields(t, doc)
	s, _ := oauth["accessToken"].(string)
	return s
}

func TestClaudeCredentialsExpiringIn5MinutesRefreshOnce(t *testing.T) {
	f := newOAuthFixture(t, 5*time.Minute)
	first := f.resolve(t)
	second := f.resolve(t)
	if n := f.endpoint.callCount(); n != 1 {
		t.Fatalf("the token endpoint got %d refreshes, want 1", n)
	}
	if accessToken(t, first) != "sk-ant-oat01-issued-1" || !bytes.Equal(first, second) {
		t.Error("Resolve did not serve the refreshed document")
	}

	// The store holds the new document, with every unrelated field and the
	// provenance kept.
	stored := f.store.value(credName)
	if !bytes.Equal(stored, first) {
		t.Fatal("the store does not hold the refreshed document")
	}
	_, oauth := oauthFields(t, stored)
	if oauth["subscriptionType"] != "max" || oauth["rateLimitTier"] != "default_claude_max_20x" ||
		oauth["refreshTokenExpiresAt"] != float64(1797000000000) || len(oauth["scopes"].([]any)) != 2 {
		t.Error("the write-back lost a claudeAiOauth field")
	}
	if !bytes.Contains(stored, []byte(`"mcpOAuth"`)) {
		t.Error("the write-back lost a top-level field")
	}
	prov := f.store.provenance(credName)
	if prov.From != "keychain:Claude Code-credentials" ||
		prov.ExpiresAt != f.clock.Now().Add(8*time.Hour).UnixMilli() {
		t.Errorf("provenance after the write-back = %+v", prov)
	}
	f.endpoint.check(t)
}

// A token shorter-lived than renewAhead is refreshed once, not on every
// request.
func TestAShortLivedTokenIsRefreshedOnce(t *testing.T) {
	f := newOAuthFixture(t, 5*time.Minute)
	f.endpoint.expiresIn = 300
	first := f.resolve(t)
	for range 4 {
		if got := f.resolve(t); !bytes.Equal(got, first) {
			t.Fatal("Resolve did not serve the refreshed document")
		}
	}
	if n := f.endpoint.callCount(); n != 1 {
		t.Errorf("five requests at one instant made %d refreshes, want 1", n)
	}
	f.endpoint.check(t)
}

func TestTwoSandboxesWithAnExpiredTokenRefreshOnce(t *testing.T) {
	f := newOAuthFixture(t, -time.Minute)
	f.endpoint.started = make(chan struct{})
	f.endpoint.release = make(chan struct{})
	started := f.endpoint.started

	sandboxes := []secret.Workload{
		{Sandbox: "brig-7f3a", Profile: "claude-code", Session: "refactor"},
		{Sandbox: "brig-2c91", Profile: "claude-code", Session: "docs"},
	}
	docs := make([][]byte, len(sandboxes))
	errs := make([]error, len(sandboxes))
	var wg sync.WaitGroup
	for i, w := range sandboxes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs[i], errs[i] = f.cache.Resolve(context.Background(), credName, w)
		}()
	}
	<-started
	close(f.endpoint.release)
	wg.Wait()

	if n := f.endpoint.callCount(); n != 1 {
		t.Errorf("the token endpoint got %d refreshes, want 1", n)
	}
	for i := range sandboxes {
		if errs[i] != nil {
			t.Fatalf("%s: %v", sandboxes[i].Sandbox, errs[i])
		}
		if accessToken(t, docs[i]) != "sk-ant-oat01-issued-1" {
			t.Errorf("%s was not served the refreshed document", sandboxes[i].Sandbox)
		}
	}
	f.endpoint.check(t)
}

func TestWriteBackFailureIsRetriedOnALaterAccess(t *testing.T) {
	f := newOAuthFixture(t, 5*time.Minute)
	f.store.failNext(1)

	first := f.resolve(t)
	if accessToken(t, first) != "sk-ant-oat01-issued-1" {
		t.Fatal("Resolve did not serve the refreshed document")
	}
	if !slices.Equal(f.cache.Pending(), []string{credName}) {
		t.Fatal("the failed write-back is not pending")
	}
	if !bytes.Equal(f.store.value(credName), f.initial) {
		t.Fatal("the store changed after a failed write")
	}

	f.clock.Advance(retryAfter)
	second := f.resolve(t)
	if !bytes.Equal(second, first) {
		t.Error("the next access did not serve the refreshed document")
	}
	if !bytes.Equal(f.store.value(credName), first) {
		t.Error("the retried write did not land")
	}
	if len(f.cache.Pending()) != 0 {
		t.Error("the write-back is still pending after a successful retry")
	}
	if n := f.endpoint.callCount(); n != 1 {
		t.Errorf("the token endpoint got %d refreshes, want 1", n)
	}
	f.endpoint.check(t)
}

func TestRefusedAnswerKeepsTheOldDocument(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":     `{"access_token":`,
		"shape changed": `{"token":{"access":"sk-ant-oat01-x","refresh":"sk-ant-ort01-x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t, 5*time.Minute)
			f.endpoint.respond = func(w http.ResponseWriter) { fmt.Fprint(w, body) }
			if got := f.resolve(t); !bytes.Equal(got, f.initial) {
				t.Error("Resolve did not serve the old document, which has not expired")
			}
			if !bytes.Equal(f.store.value(credName), f.initial) {
				t.Error("a refused answer changed the store")
			}
			if len(f.cache.Pending()) != 0 {
				t.Error("a refused answer left a write pending")
			}
			if !slices.Equal(f.cache.Failing(), []string{credName}) {
				t.Error("a refused answer is not reported as failing")
			}
		})
	}
	t.Run("non-200", func(t *testing.T) {
		f := newOAuthFixture(t, -time.Minute)
		f.endpoint.respond = func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"access_token":"sk-ant-oat01-x"}`)
		}
		doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
		if err == nil || doc != nil {
			t.Fatal("an expired document was served after a refused refresh")
		}
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("the error does not name the status: %v", err)
		}
		if !bytes.Equal(f.store.value(credName), f.initial) {
			t.Error("a refused answer changed the store")
		}
	})
}

// The provider rotated the refresh token and its answer was refused. The
// old refresh token is dead at the provider, so brig must not send it again,
// neither on a later access nor on a 401.
func TestRotatedThenRefusedSendsTheOldRefreshTokenOnce(t *testing.T) {
	f := newOAuthFixture(t, 5*time.Minute)
	f.endpoint.rotate = true
	f.endpoint.respond = func(w http.ResponseWriter) { fmt.Fprint(w, `{"token":"mangled"}`) }

	if got := f.resolve(t); !bytes.Equal(got, f.initial) {
		t.Fatal("Resolve did not serve the old document, which has not expired")
	}
	for range 4 {
		f.clock.Advance(retryAfter)
		f.resolve(t)
	}
	if _, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, f.initial); err == nil {
		t.Error("ForceRenew sent the old refresh token again")
	}
	if n := f.endpoint.timesSent(storedRefresh); n != 1 {
		t.Errorf("the old refresh token was sent %d times, want exactly once", n)
	}
	if !slices.Equal(f.cache.Failing(), []string{credName}) {
		t.Error("the spent secret is not reported as failing")
	}
	if !bytes.Equal(f.store.value(credName), f.initial) {
		t.Error("a refused answer changed the store")
	}
	f.endpoint.check(t)
}

// The provider rotates the refresh token single-use. Over a run of renewals,
// ahead of expiry and forced, no refresh token reaches the endpoint twice.
func TestOldRefreshTokenIsNeverSentTwice(t *testing.T) {
	f := newOAuthFixture(t, 5*time.Minute)
	doc := f.resolve(t)
	for range 3 {
		var err error
		if doc, err = f.cache.ForceRenew(context.Background(), credName, testWorkload, doc); err != nil {
			t.Fatalf("ForceRenew: %v", err)
		}
		f.clock.Advance(8*time.Hour - 5*time.Minute)
		doc = f.resolve(t)
	}
	if n := f.endpoint.callCount(); n != 7 {
		t.Errorf("the token endpoint got %d refreshes, want 7", n)
	}
	f.endpoint.check(t)
}

func TestForceRenewRefreshesOnce(t *testing.T) {
	f := newOAuthFixture(t, time.Hour)
	stale := f.resolve(t)
	first, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale)
	if err != nil {
		t.Fatalf("ForceRenew: %v", err)
	}
	// A second 401 for the same stale document finds it already renewed.
	second, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale)
	if err != nil {
		t.Fatalf("ForceRenew: %v", err)
	}
	if n := f.endpoint.callCount(); n != 1 {
		t.Errorf("the token endpoint got %d refreshes, want 1", n)
	}
	if accessToken(t, first) != "sk-ant-oat01-issued-1" || !bytes.Equal(first, second) {
		t.Error("ForceRenew did not return the refreshed document both times")
	}
	if !bytes.Equal(f.store.value(credName), first) {
		t.Error("the store does not hold the refreshed document")
	}
	f.endpoint.check(t)
}
