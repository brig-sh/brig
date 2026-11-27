package mcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
)

const testSecret = "s3cret-value"

// fakeCredentials is a secret.Broker over a map.
type fakeCredentials map[string]string

func (f fakeCredentials) Resolve(_ context.Context, name string, _ secret.Workload) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return []byte(v), nil
}

// seen is one request the upstream received.
type seen struct {
	method string
	header http.Header
	body   string
}

// upstream is a TLS MCP server that records every request it receives.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.Method, r.Header.Clone(), string(body)})
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) requests() []seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seen(nil), u.reqs...)
}

// jsonResult answers with a JSON-RPC result for id 1.
func jsonResult(result string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, result)
	}
}

// audit collects records; done receives each one, for a handler that runs on
// a server goroutine.
type audit struct {
	mu   sync.Mutex
	recs []Record
	done chan Record
}

func (a *audit) add(r Record) {
	a.mu.Lock()
	a.recs = append(a.recs, r)
	a.mu.Unlock()
	if a.done != nil {
		a.done <- r
	}
}

func (a *audit) records() []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Record(nil), a.recs...)
}

func setup(u *upstream, creds fakeCredentials) (*Proxy, Target, *audit) {
	a := &audit{}
	p := &Proxy{Credentials: creds, Transport: u.srv.Client().Transport, Audit: a.add}
	t := Target{
		Sandbox: secret.Workload{Sandbox: "sb1", Profile: "claude-code"},
		Server: v1alpha1.MCPServer{
			Name:       "nf",
			Transport:  v1alpha1.MCPTransportHTTP,
			URL:        u.srv.URL + "/mcp",
			Credential: &v1alpha1.CredentialRef{SecretRef: v1alpha1.SecretRef{Name: "nf-token"}},
			Inject: &v1alpha1.Inject{Headers: &v1alpha1.HeaderInject{
				Set: []v1alpha1.Header{{Name: "Authorization", Value: "Bearer {value}"}},
			}},
			Tools: &v1alpha1.MCPTools{Allow: []string{"echo"}},
		},
	}
	return p, t, a
}

func defaultCreds() fakeCredentials { return fakeCredentials{"nf-token": testSecret} }

func post(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://brig.internal/sb1/mcp/nf", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}

const callEcho = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":"b"}}}`

func TestServeRefusedCallNeverReachesUpstream(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, a := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x"}}`), tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"id":7`) || !strings.Contains(w.Body.String(), `-32601`) {
		t.Errorf("body = %s, want a JSON-RPC error for id 7", w.Body)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	recs := a.records()
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want 1", recs)
	}
	r := recs[0]
	if r.Verdict != RefuseTool || r.Tool != "x" || r.Method != "tools/call" || r.Server != "nf" || r.Sandbox != "sb1" || r.Status != http.StatusOK {
		t.Errorf("record = %+v", r)
	}
}

func TestServeRefusedNotificationIs202WithNoBody(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, a := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(`{"jsonrpc":"2.0","method":"resources/list"}`), tgt)

	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("got %d %q, want 202 with no body", w.Code, w.Body)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if recs := a.records(); len(recs) != 1 || recs[0].Verdict != RefuseMethod {
		t.Errorf("records = %+v, want one refused:method", recs)
	}
}

func TestServeInjectsCredential(t *testing.T) {
	u := newUpstream(t, jsonResult(`{"content":[]}`))
	p, tgt, a := setup(u, defaultCreds())
	r := post(callEcho)
	r.Header.Set("Authorization", "Bearer guesttoken")
	r.Header.Set("X-Other", "guesttoken")
	r.Header.Set("Mcp-Session-Id", "sess-1")
	w := httptest.NewRecorder()
	p.Serve(w, r, tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	reqs := u.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if v := got.header.Get("Authorization"); v != "Bearer "+testSecret {
		t.Errorf("upstream Authorization = %q, want the injected credential", v)
	}
	for name, vs := range got.header {
		for _, v := range vs {
			if strings.Contains(v, "guesttoken") {
				t.Errorf("upstream header %s = %q carries the guest's token", name, v)
			}
		}
	}
	if v := got.header.Get("Mcp-Session-Id"); v != "sess-1" {
		t.Errorf("upstream Mcp-Session-Id = %q, want sess-1", v)
	}
	if got.method != http.MethodPost || !strings.Contains(got.body, `"name":"echo"`) {
		t.Errorf("upstream got %s %s", got.method, got.body)
	}
	if recs := a.records(); len(recs) != 1 || recs[0].Verdict != Forward || recs[0].Tool != "echo" {
		t.Errorf("records = %+v", recs)
	}
}

func TestServeRefusesCredentialWithLineBreak(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, a := setup(u, fakeCredentials{"nf-token": "abc\r\nX-Injected: 1"})
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if strings.Contains(w.Body.String(), "abc") {
		t.Errorf("body %q holds the credential", w.Body)
	}
	if recs := a.records(); len(recs) != 1 || recs[0].Verdict != VerdictError {
		t.Errorf("records = %+v, want one error", recs)
	}
}

func TestServeRefusesCredentialField(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, _ := setup(u, fakeCredentials{"nf-token": `{"a":"x","b":"y"}`})
	tgt.Server.Credential.Field = "a"
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0: the whole document would go upstream", n)
	}
}

func TestServeRedactsCredentialFromResponse(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "id-"+testSecret)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"you sent %s"}]}}`, testSecret)
	})
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), testSecret) {
		t.Errorf("body %s holds the credential", w.Body)
	}
	if !strings.Contains(w.Body.String(), "you sent ") {
		t.Errorf("body %s lost the rest of the result", w.Body)
	}
	for name, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, testSecret) {
				t.Errorf("header %s = %q holds the credential", name, v)
			}
		}
	}
	if w.Header().Get("Mcp-Session-Id") != "" {
		t.Errorf("Mcp-Session-Id holding the credential was not dropped")
	}
}

func TestServeGETIs405(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, httptest.NewRequest(http.MethodGet, "https://brig.internal/sb1/mcp/nf", nil), tgt)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

func TestServeDELETEForwarded(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", r.Header.Get("Mcp-Session-Id"))
		w.WriteHeader(http.StatusOK)
	})
	p, tgt, _ := setup(u, defaultCreds())
	r := httptest.NewRequest(http.MethodDelete, "https://brig.internal/sb1/mcp/nf", nil)
	r.Header.Set("Mcp-Session-Id", "sess-9")
	w := httptest.NewRecorder()
	p.Serve(w, r, tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	reqs := u.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodDelete || reqs[0].header.Get("Mcp-Session-Id") != "sess-9" {
		t.Fatalf("upstream requests = %+v", reqs)
	}
	if reqs[0].header.Get("Authorization") != "Bearer "+testSecret {
		t.Errorf("DELETE went upstream without the credential")
	}
	if got := w.Header().Get("Mcp-Session-Id"); got != "sess-9" {
		t.Errorf("guest Mcp-Session-Id = %q, want sess-9", got)
	}
}

func TestServeAsksForIdentityAndRefusesEncoded(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte("\x1f\x8b not really"))
	})
	p, tgt, a := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	reqs := u.requests()
	if len(reqs) != 1 || reqs[0].header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("upstream requests = %+v, want Accept-Encoding: identity", reqs)
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "not really") {
		t.Errorf("guest received the encoded body")
	}
	if recs := a.records(); len(recs) != 1 || recs[0].Verdict != RefuseEncoding {
		t.Errorf("records = %+v, want one refused:encoding", recs)
	}
}

// A WWW-Authenticate from the upstream would send the agent into an OAuth
// flow against the upstream, for a credential the broker holds.
func TestForward401DropsWWWAuthenticate(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://evil.example/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if v := w.Header().Get("WWW-Authenticate"); v != "" {
		t.Errorf("WWW-Authenticate = %q reached the guest", v)
	}
}

func TestServeDropsSetCookie(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "upstream"})
		jsonResult(`{}`)(w, r)
	})
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if v := w.Header().Values("Set-Cookie"); len(v) != 0 {
		t.Errorf("Set-Cookie = %q reached the guest", v)
	}
}

// The guest sees the content type the filter judged, not the upstream's: a
// charset or a second value relayed raw could make a client decode the body
// differently.
func TestServeNormalisesContentType(t *testing.T) {
	for upstreamCT, want := range map[string]string{
		"application/json; charset=UTF-8; foo=bar": "application/json",
		"Text/Event-Stream; charset=utf-8":         "text/event-stream",
	} {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", upstreamCT)
			w.Header().Add("Content-Type", "text/html; charset=utf-7")
			body := `{"jsonrpc":"2.0","id":1,"result":{}}`
			if strings.HasPrefix(strings.ToLower(upstreamCT), "text/event-stream") {
				body = "data: " + body + "\n\n"
			}
			io.WriteString(w, body)
		})
		p, tgt, _ := setup(u, defaultCreds())
		w := httptest.NewRecorder()
		p.Serve(w, post(callEcho), tgt)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", upstreamCT, w.Code)
		}
		if got := w.Header().Values("Content-Type"); len(got) != 1 || got[0] != want {
			t.Errorf("%s: guest Content-Type = %q, want %q", upstreamCT, got, want)
		}
	}
}

func TestServeBodyTooLarge(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, _ := setup(u, defaultCreds())
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("a", MaxRequest) + `"}}`
	w := httptest.NewRecorder()
	p.Serve(w, post(big), tgt)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

// The proxy judges the request body as UTF-8 JSON. A guest Content-Type
// naming another charset or media type is refused before judging: an
// upstream that decodes by the declared charset could read another tool
// name out of the same bytes.
func TestServeRefusesRequestContentType(t *testing.T) {
	// Under UTF-7, "+ACIALA-" is `",` and "+ACI-" is `"`: the member x
	// closes early and a second name, evil, follows.
	utf7 := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","x":"+ACIALA-name+ACI-:+ACI-evil+ACI-"}}`
	for _, ct := range [][]string{
		{"application/json; charset=utf-7"},
		{"application/json; charset=UTF-16"},
		{"text/plain"},
		{"text/plain; charset=utf-8"},
		nil,
		{"application/json", "application/json; charset=utf-7"},
	} {
		u := newUpstream(t, jsonResult(`{}`))
		p, tgt, a := setup(u, defaultCreds())
		r := post(utf7)
		r.Header.Del("Content-Type")
		for _, v := range ct {
			r.Header.Add("Content-Type", v)
		}
		w := httptest.NewRecorder()
		p.Serve(w, r, tgt)

		if n := len(u.requests()); n != 0 {
			t.Errorf("%q: upstream received %d requests, want 0", ct, n)
		}
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `-32700`) || !strings.Contains(w.Body.String(), `"id":null`) {
			t.Errorf("%q: got %d %s, want a JSON-RPC parse error", ct, w.Code, w.Body)
		}
		if recs := a.records(); len(recs) != 1 || recs[0].Verdict != RefuseParse {
			t.Errorf("%q: records = %+v, want one refused:parse", ct, recs)
		}
	}
}

// The upstream gets the Content-Type the proxy judged the body under, never
// the guest's value.
func TestServeSetsUpstreamContentType(t *testing.T) {
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON; charset=UTF-8"} {
		u := newUpstream(t, jsonResult(`{}`))
		p, tgt, _ := setup(u, defaultCreds())
		r := post(callEcho)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		p.Serve(w, r, tgt)

		if w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d %s", ct, w.Code, w.Body)
		}
		reqs := u.requests()
		if len(reqs) != 1 {
			t.Fatalf("%q: upstream received %d requests, want 1", ct, len(reqs))
		}
		if got := reqs[0].header.Values("Content-Type"); len(got) != 1 || got[0] != "application/json" {
			t.Errorf("%q: upstream Content-Type = %q, want application/json", ct, got)
		}
	}
}

// A response the filter refuses for its charset reaches the guest as a 502
// with none of the upstream's body.
func TestServeRefusesResponseCharset(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-7")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"marker":"+ACI-"}}`)
	})
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if b := w.Body.String(); strings.Contains(b, "marker") || strings.Contains(b, "result") {
		t.Errorf("guest body = %q carries the upstream body", b)
	}
}

func TestServeAuditCarriesNoArguments(t *testing.T) {
	u := newUpstream(t, jsonResult(`{"content":[{"type":"text","text":"secret-arg"}]}`))
	p, tgt, a := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"secret-arg"}}}`), tgt)

	recs := a.records()
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want 1", recs)
	}
	for _, s := range []string{"secret-arg", testSecret} {
		if line := fmt.Sprintf("%+v", recs[0]); strings.Contains(line, s) {
			t.Errorf("record %s holds %q", line, s)
		}
	}
}

func TestServeAuditClipsGuestStrings(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, a := setup(u, defaultCreds())
	long := strings.Repeat("t", 500)
	w := httptest.NewRecorder()
	p.Serve(w, post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+long+`"}}`), tgt)

	recs := a.records()
	if len(recs) != 1 || len(recs[0].Tool) != 64 {
		t.Errorf("records = %+v, want one with a 64-byte tool", recs)
	}
}

func TestServeSecretMissing(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, a := setup(u, fakeCredentials{})
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "nf-token") {
		t.Errorf("body %q does not name the secret", w.Body)
	}
	if n := len(u.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if recs := a.records(); len(recs) != 1 || recs[0].Verdict != VerdictError || recs[0].Status != http.StatusBadGateway {
		t.Errorf("records = %+v", recs)
	}
}

func TestServeFiltersToolsList(t *testing.T) {
	u := newUpstream(t, jsonResult(`{"tools":[{"name":"echo"},{"name":"rm"}]}`))
	p, tgt, _ := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), tgt)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"echo"`) || strings.Contains(w.Body.String(), `"rm"`) {
		t.Errorf("got %d %s, want only echo", w.Code, w.Body)
	}
}

// guestServer serves the proxy over a real connection, so that flushing and
// an aborted stream are seen as a client sees them.
func guestServer(t *testing.T, p *Proxy, tgt Target) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.Serve(w, r, tgt) }))
	t.Cleanup(s.Close)
	return s
}

func guestPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// An agent waits on progress events; one held until the stream ends is one
// the agent never sees in time.
func TestServeFlushesSSEEvents(t *testing.T) {
	release := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p, tgt, _ := setup(u, defaultCreds())
	s := guestServer(t, p, tgt)
	resp := guestPost(t, s.URL, callEcho)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "notifications/progress") {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the stream ended")
	}
	close(release)
}

func TestServeAbortsStreamOnFilterError(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {not json\n\n")
	})
	p, tgt, a := setup(u, defaultCreds())
	a.done = make(chan Record, 4)
	s := guestServer(t, p, tgt)
	resp := guestPost(t, s.URL, callEcho)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("stream ended cleanly with %q, want a broken stream", body)
	}
	if bytes.Contains(body, []byte("not json")) {
		t.Errorf("the unjudged event reached the guest: %q", body)
	}
	select {
	case r := <-a.done:
		if r.Verdict != VerdictError {
			t.Errorf("record = %+v, want error", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
	}
}

func TestServeRecordsDroppedServerRequest(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
	})
	p, tgt, a := setup(u, defaultCreds())
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if strings.Contains(w.Body.String(), "sampling") {
		t.Errorf("server request reached the guest: %s", w.Body)
	}
	var dropped, forwarded int
	for _, r := range a.records() {
		switch {
		case r.Verdict == RefuseServerRequest && r.Method == "sampling/createMessage":
			dropped++
		case r.Verdict == Forward:
			forwarded++
		}
	}
	if dropped != 1 || forwarded != 1 {
		t.Errorf("records = %+v, want one dropped server request and one forwarded call", a.records())
	}
}

func TestRedactSplitAcrossReads(t *testing.T) {
	src := "a" + testSecret + "b" + testSecret[:4] + "|" + testSecret
	rc := redact(io.NopCloser(oneByte{strings.NewReader(src)}), []byte(testSecret))
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := "a" + redacted + "b" + testSecret[:4] + "|" + redacted
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A value with a quote travels escaped in a JSON string.
func TestRedactJSONEscapedForm(t *testing.T) {
	value := `ab"c<d`
	src := `{"text":"ab\"c<d and ab\"c<d"}`
	got, _ := io.ReadAll(redact(io.NopCloser(strings.NewReader(src)), []byte(value)))
	if strings.Contains(string(got), `ab\"c`) {
		t.Errorf("got %s, want both escaped forms redacted", got)
	}
}

// The end of an event holds no part of the value, so it is not held back
// waiting for the next read.
func TestRedactDoesNotHoldEventEnd(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go io.WriteString(pw, "data: x s3\n\n")
	rc := redact(pr, []byte(testSecret))
	buf := make([]byte, 64)
	got := make(chan string, 1)
	go func() {
		n, _ := rc.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if s != "data: x s3\n\n" {
			t.Errorf("read %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("redactor held the end of the event")
	}
}

// slashSecret is a base64-shaped credential. PHP's json_encode, and several
// Java and .NET encoders, write its slashes as \/.
const slashSecret = "ab/cd+ef/gh=="

// noSlashSecret fails t if s holds slashSecret in any spelling a JSON
// encoder could give it.
func noSlashSecret(t *testing.T, where, s string) {
	t.Helper()
	for _, form := range []string{slashSecret, `ab\/cd+ef\/gh==`, `ab\/cd+ef/gh==`, `ab/cd+ef\/gh==`} {
		if strings.Contains(s, form) {
			t.Errorf("%s holds the credential as %q: %q", where, form, s)
		}
	}
}

func TestServeRedactsSlashEscapedCredential(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", `Bearer ab\/cd+ef\/gh==`)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"auth was Bearer ab\/cd+ef\/gh=="}]}}`)
	})
	p, tgt, _ := setup(u, fakeCredentials{"nf-token": slashSecret})
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	noSlashSecret(t, "body", w.Body.String())
	if !strings.Contains(w.Body.String(), "auth was Bearer "+redacted) {
		t.Errorf("body %s, want the echo redacted in place", w.Body)
	}
	for name, vs := range w.Header() {
		noSlashSecret(t, "header "+name, strings.Join(vs, ","))
	}
}

func TestServeRedactsSlashEscapedCredentialSplitInSSE(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Bearer ab\/cd+e`)
		w.(http.Flusher).Flush()
		io.WriteString(w, `f\/gh== and ab/cd+ef/gh=="}]}}`+"\n\n")
	})
	p, tgt, _ := setup(u, fakeCredentials{"nf-token": slashSecret})
	s := guestServer(t, p, tgt)
	resp := guestPost(t, s.URL, callEcho)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	noSlashSecret(t, "body", string(body))
	if strings.Count(string(body), redacted) != 2 {
		t.Errorf("body %q, want both echoes redacted", body)
	}
}

func TestRedactSlashEscapedSplitAcrossReads(t *testing.T) {
	src := `data: {"text":"Bearer ab\/cd+ef\/gh==","raw":"ab/cd+ef/gh=="}` + "\n\n"
	got, err := io.ReadAll(redact(io.NopCloser(oneByte{strings.NewReader(src)}), []byte(slashSecret)))
	if err != nil {
		t.Fatal(err)
	}
	noSlashSecret(t, "output", string(got))
	want := `data: {"text":"Bearer ` + redacted + `","raw":"` + redacted + `"}` + "\n\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The transport a Proxy uses by default goes to the upstream alone, asks for
// TLS 1.2 or later and never decompresses: the filter must read the bytes
// the upstream sent, and a proxy the host names would see the credential.
func TestDefaultTransportGoesDirectAndKeepsEncoding(t *testing.T) {
	tr := DefaultTransport()
	if tr.Proxy != nil {
		t.Error("the default transport uses a proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Errorf("the default transport accepts TLS below 1.2: %+v", tr.TLSClientConfig)
	}
	if !tr.DisableCompression {
		t.Error("the default transport decompresses responses")
	}
	if DefaultTransport() == tr {
		t.Error("DefaultTransport returned the same transport twice; a caller's change would reach the next one")
	}
}

// The guest learns only that the server is unreachable. The transport's
// error, which may name host addresses, goes to Log on the host.
func TestServeLogsUnreachableDetail(t *testing.T) {
	u := newUpstream(t, jsonResult(`{}`))
	p, tgt, _ := setup(u, defaultCreds())
	u.srv.Close()
	var logged []string
	p.Log = func(server string, err error) { logged = append(logged, server+": "+err.Error()) }
	w := httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)
	if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != `mcp server "nf" is unreachable` {
		t.Errorf("guest got %d %q, want 502 unreachable alone", w.Code, w.Body)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "nf: ") || len(logged[0]) <= len("nf: ") {
		t.Errorf("logged %q, want one line for nf with the transport's error", logged)
	}
	// A Proxy without Log still answers.
	p.Log = nil
	w = httptest.NewRecorder()
	p.Serve(w, post(callEcho), tgt)
	if w.Code != http.StatusBadGateway {
		t.Errorf("without Log: status %d, want 502", w.Code)
	}
}
