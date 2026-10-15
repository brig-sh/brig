package broker

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
)

// encodedToken is one way an agent can write a token into a request.
type encodedToken struct {
	name string
	// label is the encoding= the audit line names.
	label  string
	encode func(tok string) string
}

func escapeEach(tok, prefix, digits string) string {
	var b strings.Builder
	for i := 0; i < len(tok); i++ {
		b.WriteString(prefix)
		b.WriteByte(digits[tok[i]>>4])
		b.WriteByte(digits[tok[i]&15])
	}
	return b.String()
}

// splitEvery puts sep after every n bytes of s.
func splitEvery(s string, n int, sep string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s[i:min(i+n, len(s))])
	}
	return b.String()
}

var tokenEncodings = []encodedToken{
	{"raw", "raw", func(t string) string { return t }},
	{"json-string", "raw", func(t string) string { b, _ := json.Marshal(t); return string(b) }},
	{"percent-upper", "percent", func(t string) string { return escapeEach(t, "%", "0123456789ABCDEF") }},
	{"percent-lower", "percent", func(t string) string { return escapeEach(t, "%", "0123456789abcdef") }},
	{"json-u-upper", "json", func(t string) string { return escapeEach(t, `\u00`, "0123456789ABCDEF") }},
	{"json-u-lower", "json", func(t string) string { return escapeEach(t, `\u00`, "0123456789abcdef") }},
	{"hex-lower", "hex", func(t string) string { return hex.EncodeToString([]byte(t)) }},
	{"hex-upper", "hex", func(t string) string { return strings.ToUpper(hex.EncodeToString([]byte(t))) }},
	{"base64-std", "base64", func(t string) string { return base64.StdEncoding.EncodeToString([]byte(t)) }},
	{"base64-std-raw", "base64", func(t string) string { return base64.RawStdEncoding.EncodeToString([]byte(t)) }},
	{"base64-url", "base64", func(t string) string { return base64.URLEncoding.EncodeToString([]byte(t)) }},
	{"base64-url-raw", "base64", func(t string) string { return base64.RawURLEncoding.EncodeToString([]byte(t)) }},
	// The token starts one byte into a base64 group.
	{"base64-after-bearer", "base64", func(t string) string {
		return base64.StdEncoding.EncodeToString([]byte("Bearer " + t))
	}},
	// The token starts two bytes into a base64 group.
	{"base64-url-offset-2", "base64", func(t string) string {
		return base64.URLEncoding.EncodeToString([]byte("ab" + t + "c"))
	}},
	{"base64-mime-wrapped", "base64", func(t string) string {
		return splitEvery(base64.StdEncoding.EncodeToString([]byte(t)), 76, "\r\n")
	}},
	{"raw-split-by-lines", "raw", func(t string) string { return splitEvery(t, 50, "\n") }},
	{"hex-split-by-spaces", "hex", func(t string) string { return splitEvery(hex.EncodeToString([]byte(t)), 2, " ") }},
	// A JSON string carries a line break as the two characters \n, and may
	// write / as \/. The base64 of a token holds no /, the base64 around it
	// can.
	{"base64-wrapped-in-a-json-string", "base64", func(t string) string {
		b, _ := json.Marshal(splitEvery(base64.StdEncoding.EncodeToString([]byte("API_KEY="+t+"\n")), 76, "\n"))
		return string(b)
	}},
	{"base64-with-json-escaped-slashes", "base64", func(t string) string {
		return strings.ReplaceAll(base64.StdEncoding.EncodeToString([]byte("\xff\xff\xff"+t+"\xff")), "/", `\/`)
	}},
}

// allSpellings returns every spelling the scan looks for, written out.
func allSpellings(tok string) []string {
	m := newMatcher([]liveToken{{token: tok}})
	var out []string
	for _, a := range m.anchors {
		b := make([]byte, a.length(tok))
		for i := range b {
			b[i] = a.at(tok, i)
		}
		out = append(out, string(b))
	}
	return out
}

// send is rig.do for a body of any reader. A reader that is not a bytes or
// strings reader goes chunked, with no length.
func (r *rig) send(method, path string, h http.Header, body io.Reader) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	r.lines++
	r.waitAudit(r.lines)
	return resp, string(got)
}

// wantPlaceholder checks a refusal: 403 with the body that names the route
// and the position, the audit line, and nothing read or dialled.
func wantPlaceholder(t *testing.T, r *rig, resp *http.Response, body, route, where, encoding string) {
	t.Helper()
	wantStatus(t, resp, http.StatusForbidden)
	want := fmt.Sprintf("brig: refused: the session token was sent outside its carrier (route %s, in %s)\n", route, where)
	if body != want {
		t.Errorf("body %q, want %q", body, want)
	}
	w := "where=" + where
	if strings.Contains(where, " ") {
		w = fmt.Sprintf("where=%q", where)
	}
	wantAudit(t, r.lastAudit(), "status=403", "check=placeholder", w, "encoding="+encoding, "path=[redacted]")
	if n := r.up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
	if res, _ := r.creds.counts(); res != 0 {
		t.Errorf("Resolve ran %d times", res)
	}
}

// A session token anywhere but its carrier, in any encoding the scan knows,
// is refused before request.allow and Resolve, and the upstream is never
// dialled.
func TestPlaceholderInEveryPositionAndEncodingIsRefused(t *testing.T) {
	positions := []struct {
		where string
		send  func(r *rig, tok, enc string) (*http.Response, string)
	}{
		{"path", func(r *rig, tok, enc string) (*http.Response, string) {
			return r.send("POST", "/brig-a/r/anthropic/v1/"+url.PathEscape(enc), bearer(tok), strings.NewReader("{}"))
		}},
		{"query", func(r *rig, tok, enc string) (*http.Response, string) {
			return r.send("POST", "/brig-a/r/anthropic/v1/messages?q="+url.QueryEscape(enc), bearer(tok),
				strings.NewReader("{}"))
		}},
		{"header X-Leak", func(r *rig, tok, enc string) (*http.Response, string) {
			h := bearer(tok)
			// A header value holds no line break, so a split token is split
			// by spaces there.
			h.Set("X-Leak", strings.NewReplacer("\r\n", " ", "\n", " ").Replace(enc))
			return r.send("POST", "/brig-a/r/anthropic/v1/messages", h, strings.NewReader("{}"))
		}},
		{"body", func(r *rig, tok, enc string) (*http.Response, string) {
			return r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok),
				strings.NewReader(`{"messages":[{"role":"user","content":"env says `+enc+` ok"}]}`))
		}},
	}
	for _, pos := range positions {
		for _, enc := range tokenEncodings {
			t.Run(pos.where+"/"+enc.name, func(t *testing.T) {
				up := newUpstream(t, nil)
				r := newRig(t, up, claudeRoutes(t, up))
				tok := r.mint("brig-a", "anthropic")
				resp, body := pos.send(r, tok, enc.encode(tok))
				wantPlaceholder(t, r, resp, body, "anthropic", pos.where, enc.label)
			})
		}
	}
}

// A header name can hold a token too. The refusal does not repeat the name.
func TestPlaceholderInAHeaderNameIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	h := bearer(tok)
	// Go puts a header name in canonical case, and hex in lower case keeps
	// its case through that.
	h.Set("X-Leak-"+hex.EncodeToString([]byte(tok)), "1")
	resp, body := r.send("POST", "/brig-a/r/anthropic/v1/messages", h, strings.NewReader("{}"))
	wantPlaceholder(t, r, resp, body, "anthropic", "header name", "hex")
}

// The token in its carrier, and nowhere else, passes on each preset's
// carrier: Bearer, x-api-key and the git basic password. So does a body
// that holds half of the token, which is not the token. A tool may send the
// same token in two carriers of its route.
func TestTokenInItsCarrierAlonePasses(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	claimsPart, macPart, _ := strings.Cut(tok, ".")
	resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages?beta=true", bearer(tok),
		strings.NewReader(`{"a":"`+claimsPart+`","b":"`+macPart+`"}`))
	wantStatus(t, resp, http.StatusOK)
	if line := r.lastAudit(); strings.Contains(line, "placeholder") || strings.Contains(line, "where=") {
		t.Errorf("a clean request has a placeholder field: %s", line)
	}

	gh := http.Request{Header: http.Header{}}
	gh.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
	resp, _ = r.send("POST", "/brig-a/r/github/brig-sh/brig.git/git-upload-pack", gh.Header,
		strings.NewReader("0032want 0123456789012345678901234567890123456789\n"))
	wantStatus(t, resp, http.StatusOK)

	upKey := newUpstream(t, nil)
	rk := newRig(t, upKey, []Route{
		presetRoute(t, v1alpha1.PresetAnthropicAPIKey, "anthropic", "anthropic-api-key", upKey.URL, nil),
	})
	key := rk.mint("brig-a", "anthropic")
	resp, _ = rk.send("POST", "/brig-a/r/anthropic/v1/messages", http.Header{"X-Api-Key": {key}},
		strings.NewReader("{}"))
	wantStatus(t, resp, http.StatusOK)
	both := bearer(key)
	both.Set("X-Api-Key", key)
	resp, _ = rk.send("POST", "/brig-a/r/anthropic/v1/messages", both, strings.NewReader("{}"))
	wantStatus(t, resp, http.StatusOK)
}

// A carrier that holds another token besides its own, such as a basic-auth
// username, is refused.
func TestCarrierHoldingAnotherTokenIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	anthropic := r.mint("brig-a", "anthropic")
	gh := http.Request{Header: http.Header{}}
	gh.SetBasicAuth(anthropic, r.mint("brig-a", "github"))
	resp, body := r.send("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs", gh.Header, nil)
	wantPlaceholder(t, r, resp, body, "anthropic", "header Authorization", "raw")
}

// A token of one route found in a request to another is a placeholder hit,
// and the refusal names the route of the token. Sent as the carrier of the
// other route, it is still a 401 from the route check.
func TestPlaceholderOfAnotherRouteIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	anthropic := r.mint("brig-a", "anthropic")
	github := r.mint("brig-a", "github")
	resp, body := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(anthropic),
		strings.NewReader(`{"content":"GH=`+github+`"}`))
	wantPlaceholder(t, r, resp, body, "github", "body", "raw")

	gh := http.Request{Header: http.Header{}}
	gh.SetBasicAuth("x-access-token", github)
	gh.Header.Set("X-Other", base64.StdEncoding.EncodeToString([]byte(anthropic)))
	resp, body = r.send("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs", gh.Header, nil)
	wantPlaceholder(t, r, resp, body, "anthropic", "header X-Other", "base64")

	resp, _ = r.send("GET", "/brig-a/r/github/brig-sh/brig.git/info/refs", bearer(anthropic), nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "check=token.route")
}

// A body may hold a token of another sandbox. The scan looks for this
// sandbox's own tokens only: a leak of a handle is a leak by the sandbox
// that holds it, and the broker cannot tell another sandbox's token in a
// prompt from data the agent was handed. Refusing it would let one sandbox
// stop another's conversation by planting its own token in a file the other
// reads, and would make the scan grow with every sandbox on the host.
func TestTokenOfAnotherSandboxInTheBodyPasses(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	other := r.mint("brig-b", "anthropic")
	resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")),
		strings.NewReader(`{"content":"`+other+`"}`))
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t).body; !strings.Contains(got, other) {
		t.Errorf("the upstream got body %q", got)
	}
}

// A token that spans two chunks of the scan, in a chunked body of no known
// length, is found.
func TestPlaceholderSpanningScanChunksIsFound(t *testing.T) {
	for _, at := range []int{scanChunk - 7, 2*scanChunk - 100, 3*scanChunk - 1} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			up := newUpstream(t, nil)
			r := newRig(t, up, claudeRoutes(t, up))
			tok := r.mint("brig-a", "anthropic")
			body := strings.Repeat("x", at) + tok + strings.Repeat("y", 4*scanChunk)
			// A reader that hides its length makes the client send chunked.
			resp, got := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok),
				io.MultiReader(strings.NewReader(body)))
			wantPlaceholder(t, r, resp, got, "anthropic", "body", "raw")
		})
	}
}

// The scan reads up to maxReplay bytes of a body. A token that ends on the
// last of them is found, and the scan is whole.
func TestPlaceholderEndingAtTheCapIsFound(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	body := strings.Repeat("x", maxReplay-len(tok)) + tok
	resp, got := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), strings.NewReader(body))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "raw")
	if strings.Contains(r.lastAudit(), "placeholder=partial") {
		t.Error("a body of maxReplay bytes was audited as scanned in part")
	}
}

// Above the cap the start of the body is scanned and the line says
// placeholder=partial. A token in the start is refused. A token past it is
// not found, and the body reaches the upstream whole.
func TestPlaceholderAboveTheCapScansTheStart(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	tail := strings.Repeat("y", 1<<20)
	resp, got := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok),
		strings.NewReader("{"+tok+strings.Repeat("x", maxReplay)+tail))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "raw")
	wantAudit(t, r.lastAudit(), "placeholder=partial")

	past := strings.Repeat("x", maxReplay+10) + tok
	resp, _ = r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), strings.NewReader(past))
	wantStatus(t, resp, http.StatusOK)
	wantAudit(t, r.lastAudit(), "status=200", "placeholder=partial")
	if got := up.last(t).body; got != past {
		t.Errorf("the upstream got %d bytes of a %d-byte body", len(got), len(past))
	}
}

// A body the replay budget cannot hold in time is refused with 503, and is
// never forwarded unscanned.
func TestBodyTheBudgetCannotHoldIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) {
		c.ReplayBudget = 16
		c.BodyTimeout = 200 * time.Millisecond
	})
	tok := r.mint("brig-a", "anthropic")
	resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok),
		strings.NewReader(strings.Repeat("x", 100)+tok))
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 503 for the budget has no Retry-After")
	}
	wantAudit(t, r.lastAudit(), "check=budget")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// A gzip or deflate body is scanned as it decompresses, and goes upstream as
// the guest compressed it. A body in a coding the scan cannot read is
// refused with 415.
func TestCompressedBodies(t *testing.T) {
	gz := func(s, name string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Name = name
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.String()
	}
	zl := func(s string) string {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.String()
	}
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	coded := func(coding string) http.Header {
		h := bearer(tok)
		h.Set("Content-Encoding", coding)
		return h
	}
	const path = "/brig-a/r/anthropic/v1/messages"

	resp, got := r.send("POST", path, coded("gzip"), strings.NewReader(gz(`{"c":"`+tok+`"}`, "")))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "gzip+raw")
	b64 := base64.StdEncoding.EncodeToString([]byte(tok))
	resp, got = r.send("POST", path, coded("deflate"), strings.NewReader(zl(`{"c":"`+b64+`"}`)))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "deflate+base64")
	resp, got = r.send("POST", path, coded("gzip"), strings.NewReader(gz("{}", tok)))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "gzip+raw")

	for _, c := range []struct{ coding, body string }{
		{"br", "anything"},
		{"gzip, gzip", gz(gz("{}", ""), "")},
		{"gzip", "not gzip at all"},
	} {
		resp, _ := r.send("POST", path, coded(c.coding), strings.NewReader(c.body))
		wantStatus(t, resp, http.StatusUnsupportedMediaType)
		wantAudit(t, r.lastAudit(), "check=content-encoding")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}

	// Raw deflate, without the zlib wrapper, as some clients send it.
	fl := func(s string) string {
		var b bytes.Buffer
		w, _ := flate.NewWriter(&b, flate.BestSpeed)
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.String()
	}
	resp, got = r.send("POST", path, coded("deflate"), strings.NewReader(fl(`{"c":"`+tok+`"}`)))
	wantPlaceholder(t, r, resp, got, "anthropic", "body", "deflate+raw")
	// A small body that decompresses past the cap cannot be scanned whole.
	// Measured before the fix: 8 KiB of gzip with the token past 8 MiB of
	// output got 200 and placeholder=partial.
	bomb := gz(strings.Repeat("a", maxReplay+1)+tok, "")
	resp, _ = r.send("POST", path, coded("gzip"), strings.NewReader(bomb))
	wantStatus(t, resp, http.StatusRequestEntityTooLarge)
	wantAudit(t, r.lastAudit(), "check=body")
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}

	resp, _ = r.send("POST", path, coded("deflate"), strings.NewReader(fl(`{"a":1}`)))
	wantStatus(t, resp, http.StatusOK)
	clean := gz(`{"model":"claude-sonnet-5"}`, "")
	resp, _ = r.send("POST", path, coded("gzip"), strings.NewReader(clean))
	wantStatus(t, resp, http.StatusOK)
	if got := up.last(t); got.body != clean || got.header.Get("Content-Encoding") != "gzip" {
		t.Errorf("the upstream got %q in %q, want the guest's gzip bytes", got.body, got.header.Get("Content-Encoding"))
	}
}

// A decoder stops at the end of its stream, so bytes after it would go
// upstream unscanned. A body with any is refused, and a token in the bytes
// as sent is found whatever the coding says.
func TestCompressedBodyWithBytesAfterItsStreamIsRefused(t *testing.T) {
	compress := func(coding, s string) string {
		var b bytes.Buffer
		var w io.WriteCloser
		switch coding {
		case "gzip":
			w = gzip.NewWriter(&b)
		case "zlib":
			w = zlib.NewWriter(&b)
		default:
			w, _ = flate.NewWriter(&b, flate.BestSpeed)
		}
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.String()
	}
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	const path = "/brig-a/r/anthropic/v1/messages"
	for _, c := range []struct{ coding, header string }{{"zlib", "deflate"}, {"flate", "deflate"}, {"gzip", "gzip"}} {
		h := bearer(tok)
		h.Set("Content-Encoding", c.header)
		resp, got := r.send("POST", path, h, strings.NewReader(compress(c.coding, "{}")+`{"leak":"`+tok+`"}`))
		wantPlaceholder(t, r, resp, got, "anthropic", "body", "raw")
		resp, _ = r.send("POST", path, h, strings.NewReader(compress(c.coding, "{}")+`{"more":1}`))
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("%s with bytes after the stream: %d, want 415", c.coding, resp.StatusCode)
		}
		wantAudit(t, r.lastAudit(), "check=content-encoding")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}
}

// A body above the cap that says gzip or deflate and does not decompress is
// refused, as a smaller one is. Only a stream cut at the cap is scanned in
// part.
func TestLargeBodyThatDoesNotDecompressIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	const path = "/brig-a/r/anthropic/v1/messages"
	for _, coding := range []string{"gzip", "deflate"} {
		h := bearer(tok)
		h.Set("Content-Encoding", coding)
		resp, got := r.send("POST", path, h, strings.NewReader(`{"c":"`+tok+`"}`+strings.Repeat("x", maxReplay+200)))
		wantPlaceholder(t, r, resp, got, "anthropic", "body", "raw")
		resp, _ = r.send("POST", path, h, strings.NewReader(`{"c":"`+strings.Repeat("x", maxReplay+200)))
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("%s over the cap that does not decompress: %d, want 415", coding, resp.StatusCode)
		}
		wantAudit(t, r.lastAudit(), "check=content-encoding")
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the upstream saw %d requests", n)
	}

	// A gzip stream longer than the cap is cut there, and goes upstream.
	raw := make([]byte, maxReplay+(1<<20))
	_, _ = rand.Read(raw)
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, gzip.NoCompression)
	_, _ = w.Write(raw)
	_ = w.Close()
	h := bearer(tok)
	h.Set("Content-Encoding", "gzip")
	resp, _ := r.send("POST", path, h, bytes.NewReader(b.Bytes()))
	wantStatus(t, resp, http.StatusOK)
	wantAudit(t, r.lastAudit(), "placeholder=partial")
	if got := up.last(t).body; got != b.String() {
		t.Errorf("the upstream got %d bytes of a %d-byte body", len(got), b.Len())
	}
}

// The audit line of a refusal names where and how, and never holds the
// token, a spelling of it or the body.
func TestPlaceholderAuditHoldsNoToken(t *testing.T) {
	const marker = "body-marker-5d1e"
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	gh := r.mint("brig-a", "github")
	for _, enc := range tokenEncodings {
		r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok),
			strings.NewReader(marker+enc.encode(tok)+enc.encode(gh)))
		h := bearer(tok)
		h.Set("X-"+hex.EncodeToString([]byte(gh)), marker)
		r.send("POST", "/brig-a/r/anthropic/v1/"+url.PathEscape(enc.encode(gh)), h, nil)
	}
	log := r.audit.String()
	if n := strings.Count(log, "check=placeholder"); n != 2*len(tokenEncodings) {
		t.Fatalf("%d placeholder lines, want %d:\n%s", n, 2*len(tokenEncodings), log)
	}
	if strings.Contains(log, marker) {
		t.Errorf("the audit log holds the body:\n%s", log)
	}
	// A token can be split by the logger's quoting, so look for a piece of
	// each half and of each spelling.
	for _, x := range []string{tok, gh} {
		body, mac, _ := strings.Cut(x, ".")
		for _, s := range append(allSpellings(x), body, mac) {
			if strings.Contains(log, s[len(s)-24:]) {
				t.Errorf("the audit log holds part of a token:\n%s", log)
			}
		}
	}
}

// A sandbox keeps its last maxLive tokens. An older one is not found in a
// body until it is presented again as a carrier, and an expired one is
// dropped. A removed sandbox takes its tokens with it.
func TestLiveTokensAreBoundedAndPruned(t *testing.T) {
	up := newUpstream(t, nil)
	routes := claudeRoutes(t, up)
	r := newRig(t, up, routes)
	first := r.mint("brig-a", "anthropic")
	for i := range maxLive {
		if _, err := r.h.Mint("brig-a", fmt.Sprint("s", i), "anthropic", DefaultTokenTTL); err != nil {
			t.Fatal(err)
		}
	}
	const path = "/brig-a/r/anthropic/v1/messages"
	// rig.mint mints for session refactor, as first was, so the carrier is
	// a token of a session of its own.
	carrier, err := r.h.Mint("brig-a", "carrier", "anthropic", DefaultTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := r.send("POST", path, bearer(carrier), strings.NewReader(first))
	wantStatus(t, resp, http.StatusOK)
	// Presented as a carrier, the dropped token is live again.
	resp, _ = r.send("POST", path, bearer(first), strings.NewReader("{}"))
	wantStatus(t, resp, http.StatusOK)
	resp, _ = r.send("POST", path, bearer(carrier), strings.NewReader(first))
	wantStatus(t, resp, http.StatusForbidden)

	short, err := r.h.Mint("brig-a", "short", "anthropic", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * time.Hour)
	resp, _ = r.send("POST", path, bearer(first), strings.NewReader(short))
	wantStatus(t, resp, http.StatusOK)
	sb := r.h.sandbox("brig-a")
	sb.liveMu.Lock()
	n := len(sb.live)
	kept := slices.ContainsFunc(sb.live, func(l liveToken) bool { return l.token == short })
	sb.liveMu.Unlock()
	if n > maxLive || kept {
		t.Errorf("brig-a keeps %d live tokens, the expired one among them: %v", n, kept)
	}

	r.h.RemoveSandbox("brig-a")
	if err := r.h.AddSandbox(Sandbox{ID: "brig-a", Profile: "claude-code", Routes: routes}); err != nil {
		t.Fatal(err)
	}
	sb = r.h.sandbox("brig-a")
	if len(sb.live) != 0 || sb.placeholders.Load() != 0 {
		t.Errorf("a sandbox added again has %d live tokens and %d refusals", len(sb.live), sb.placeholders.Load())
	}
}

// Each refusal counts against its sandbox, and Placeholders names only the
// sandboxes with one.
func TestPlaceholdersAreCounted(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for range 2 {
		r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), strings.NewReader(tok))
	}
	r.send("POST", "/brig-b/r/anthropic/v1/messages", bearer(r.mint("brig-b", "anthropic")), strings.NewReader("{}"))
	got := r.h.Placeholders()
	if len(got) != 1 || got["brig-a"] != 2 {
		t.Errorf("Placeholders() = %v, want brig-a: 2", got)
	}
}

// The held body is what a retry after an upstream 401 sends again, and goes
// upstream unchanged the first time.
func TestHeldBodyIsSentAndReplayed(t *testing.T) {
	up := newUpstream(t, refuseStale)
	r := newRig(t, up, claudeRoutes(t, up))
	body := strings.Repeat("z", 3*scanChunk+17)
	resp, got := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(r.mint("brig-a", "anthropic")),
		io.MultiReader(strings.NewReader(body)))
	wantStatus(t, resp, http.StatusOK)
	if got != body {
		t.Errorf("the retry carried %d bytes, want %d", len(got), len(body))
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.reqs) != 2 || up.reqs[0].body != body {
		t.Errorf("the upstream saw %d requests", len(up.reqs))
	}
}

// The scan of an 8 MiB body for the spellings of 10 live tokens. Measured on
// an M-series Mac at about 24 ms; the design bound is 100 ms.
func BenchmarkPlaceholderScan(b *testing.B) {
	var live []liveToken
	key := bytes.Repeat([]byte{9}, 32)
	for i := range 10 {
		tok, err := mint(key, claims{Sandbox: "brig-claude-code", Session: fmt.Sprint("s", i), Route: "anthropic",
			Exp: 1792000000, Typ: tokenType})
		if err != nil {
			b.Fatal(err)
		}
		live = append(live, liveToken{token: tok, route: "anthropic", exp: time.Unix(1792000000, 0)})
	}
	m := newMatcher(live)
	// Text that keeps hitting the first characters of the spellings: the
	// start of every token, e, digits, escapes.
	words := strings.Fields(`the quick brown fox eyJzYW5kYm94Ijoi lazy \u0065 %65%79 6579 e ee {"role":"user"}`)
	var body bytes.Buffer
	for i := 0; body.Len() < 8<<20; i++ {
		body.WriteString(words[i*7%len(words)])
		body.WriteByte(" \n"[i%2])
	}
	data := body.Bytes()[:8<<20]
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		if hit := m.scan().write(data); hit != nil {
			b.Fatal("a clean body matched")
		}
	}
}

// budgetHeld returns what sandbox holds of the replay budget, and what is
// left of it.
func budgetHeld(h *Handler, sandbox string) (held, left int64) {
	h.replay.mu.Lock()
	defer h.replay.mu.Unlock()
	return h.replay.held[sandbox], h.replay.left
}

// waitBudget waits until cond holds of the budget.
func waitBudget(t *testing.T, h *Handler, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the budget did not settle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stall sends a request with a Content-Length and the first n bytes of its
// body, and leaves the rest unsent until the test ends.
func stall(t *testing.T, r *rig, sandbox, tok string, length, n int) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		_, _ = fmt.Fprintf(c, "POST /%s/r/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
			"Content-Length: %d\r\n\r\n", sandbox, tok, length)
		_, _ = c.Write(bytes.Repeat([]byte("x"), n))
	}()
}

// A guest that opens bodies, declares large lengths and stalls holds one
// growth step of the budget per body, not the length it declared, so the
// bodies of another sandbox are still scanned. Measured before the fix: 29
// such bodies from brig-a left 1 byte of the budget, and brig-b's token went
// upstream in its body with 200.
func TestOneSandboxCannotDrainTheBodyBudget(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tokA := r.mint("brig-a", "anthropic")
	for range 7 {
		stall(t, r, "brig-a", tokA, maxReplay, 1)
	}
	// A length of 1 with its one byte sent is a whole body, so the least is 3.
	for k := 22; k >= 2; k-- {
		stall(t, r, "brig-a", tokA, 1<<k-1, 1)
	}
	waitBudget(t, r.h, func() bool { held, _ := budgetHeld(r.h, "brig-a"); return held >= 7*holdStep })
	held, left := budgetHeld(r.h, "brig-a")
	if held > 28*holdStep {
		t.Errorf("28 stalled bodies hold %d bytes, want at most one step of %d each", held, holdStep)
	}
	tokB := r.mint("brig-b", "anthropic")
	resp, body := r.send("POST", "/brig-b/r/anthropic/v1/messages", bearer(tokB),
		strings.NewReader(`{"messages":[{"role":"user","content":"env: `+tokB+`"}]}`))
	wantPlaceholder(t, r, resp, body, "anthropic", "body", "raw")
	t.Logf("budget left %d with brig-a holding %d", left, held)
}

// A sandbox holds at most its share of the budget, the budget over the
// registered sandboxes. Past it, its own next body waits a tenth of
// BodyTimeout and is refused with 503, while another sandbox's body is
// served.
func TestASandboxHoldsAtMostItsShareOfTheBudget(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) {
		c.ReplayBudget = 4 * maxHeld
		c.BodyTimeout = 5 * time.Second
	})
	share := r.h.share()
	if share != 2*maxHeld {
		t.Fatalf("share %d with two sandboxes, want %d", share, 2*maxHeld)
	}
	tokA := r.mint("brig-a", "anthropic")
	stall(t, r, "brig-a", tokA, maxReplay+100, maxReplay)
	stall(t, r, "brig-a", tokA, maxReplay+100, maxReplay)
	waitBudget(t, r.h, func() bool { held, _ := budgetHeld(r.h, "brig-a"); return held == share })

	resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tokA), strings.NewReader("{}"))
	wantStatus(t, resp, http.StatusServiceUnavailable)
	wantAudit(t, r.lastAudit(), "sandbox=brig-a", "check=budget")

	resp, _ = r.send("POST", "/brig-b/r/anthropic/v1/messages", bearer(r.mint("brig-b", "anthropic")),
		strings.NewReader(strings.Repeat("y", 1<<20)))
	wantStatus(t, resp, http.StatusOK)
	if n := up.count(); n != 1 {
		t.Fatalf("the upstream saw %d requests, want brig-b's", n)
	}
}

// A body whose held part does not arrive within BodyTimeout is refused with
// 408, and its bytes go back to the budget.
func TestAStalledBodyTimesOut(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) { c.BodyTimeout = 300 * time.Millisecond })
	tok := r.mint("brig-a", "anthropic")
	c, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "POST /brig-a/r/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
		"Content-Length: 100\r\n\r\n0123456789", tok)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, resp, http.StatusRequestTimeout)
	r.lines++
	wantAudit(t, r.waitAudit(r.lines), "check=body")
	waitBudget(t, r.h, func() bool { _, left := budgetHeld(r.h, "brig-a"); return left == DefaultReplayBudget })
	if up.count() != 0 {
		t.Fatal("the upstream saw the stalled request")
	}
}

// Request trailers do not reach the upstream, below the cap or above it: a
// trailer would pass every header rule, since the rules edit the headers
// alone. Measured before the fix: X-Brig-Debug, X-Http-Method-Override and
// X-Forwarded-For sent as trailers of a small body reached the upstream.
func TestRequestTrailersAreNotForwarded(t *testing.T) {
	var mu sync.Mutex
	var hdr, trl http.Header
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hdr, trl = r.Header.Clone(), r.Trailer.Clone()
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	})
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for _, size := range []int{1 << 10, maxReplay + 100} {
		req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages",
			io.MultiReader(strings.NewReader(strings.Repeat("x", size))))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = bearer(tok)
		req.Trailer = http.Header{"X-Checksum": {"abc"}, "X-Brig-Debug": {"1"},
			"X-Http-Method-Override": {"DELETE"}, "X-Forwarded-For": {"1.2.3.4"}}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		r.lines++
		r.waitAudit(r.lines)
		wantStatus(t, resp, http.StatusOK)
		mu.Lock()
		for _, k := range []string{"X-Checksum", "X-Brig-Debug", "X-Http-Method-Override", "X-Forwarded-For"} {
			if hdr.Get(k) != "" || trl.Get(k) != "" {
				t.Errorf("body %d: the upstream got %s as header %q, trailer %q", size, k, hdr.Get(k), trl.Get(k))
			}
		}
		mu.Unlock()
	}
}

// A token in a trailer of a body read whole is refused, though the trailer
// would not be forwarded.
func TestPlaceholderInATrailerIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages",
		io.MultiReader(strings.NewReader("{}")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(tok)
	req.Trailer = http.Header{"X-Leak": {tok}}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	r.lines++
	r.waitAudit(r.lines)
	wantPlaceholder(t, r, resp, string(body), "anthropic", "trailer X-Leak", "raw")
}

// A token in the method is refused, and the audit line holds neither the
// method nor the path, which held the token too.
func TestPlaceholderInTheMethodIsNotAudited(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	c, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "%s /brig-a/r/anthropic/%s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
		"Content-Length: 0\r\n\r\n", tok, tok, tok)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	r.lines++
	r.waitAudit(r.lines)
	wantPlaceholder(t, r, resp, string(body), "anthropic", "method", "raw")
	line := r.lastAudit()
	wantAudit(t, line, "method=[redacted]")
	if strings.Contains(line, tok[:16]) {
		t.Errorf("the audit line holds part of the token: %s", line)
	}
}

// The scan carries backslashes at the end of one write into the next, so a
// JSON escape split across chunks is still read, also escaped twice.
func TestScanCarriesAnEscapeAcrossWrites(t *testing.T) {
	tok := mustMint(t, "s")
	m := newMatcher([]liveToken{{token: tok, route: "anthropic", exp: time.Unix(1792000000, 0)}})
	half := len(tok) / 2
	for _, parts := range [][2]string{
		{`"` + tok[:half] + `\`, `n` + tok[half:] + `"`},
		{`"` + tok[:half] + `\r\`, `n` + tok[half:] + `"`},
		{`"` + tok[:half] + `\`, `t` + tok[half:] + `"`},
		{`"` + tok[:half] + `\\`, `n` + tok[half:] + `"`},
	} {
		sc := m.scan()
		if sc.write([]byte(parts[0])) != nil {
			t.Fatal("half a token matched")
		}
		if sc.write([]byte(parts[1])) == nil {
			t.Errorf("an escape split across writes hid the token: %q | %q", parts[0][:10], parts[1][:10])
		}
	}
	// A JSON string inside a JSON string escapes the escape.
	if m.findString(tok[:half]+`\\n`+tok[half:]) == nil {
		t.Error("a line break escaped twice hid the token")
	}
}

func mustMint(t testing.TB, session string) string {
	t.Helper()
	tok, err := mint(bytes.Repeat([]byte{9}, 32), claims{Sandbox: "brig-claude-code", Session: session,
		Route: "anthropic", Exp: 1792000000, Typ: tokenType})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// The query is decoded escape by escape, so one bad escape does not keep the
// rest from being read. Measured before the fix: &bad=%zz after a token with
// one escaped character got 200.
func TestQueryWithABadEscapeIsStillDecoded(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	one := tok[:5] + fmt.Sprintf("%%%02X", tok[5]) + tok[6:]
	for _, q := range []string{"q=" + one + "&bad=%zz", "q=" + splitEvery(tok, 20, "+") + "&bad=%"} {
		resp, body := r.send("POST", "/brig-a/r/anthropic/v1/messages?"+q, bearer(tok), strings.NewReader("{}"))
		wantPlaceholder(t, r, resp, body, "anthropic", "query", "raw")
	}
}

// Every session token the sandbox minted within its lifetime is looked for,
// not only the latest few: a sandbox with two routes mints two per exec.
func TestTokensStayLiveUntilTheyExpire(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	first := r.mint("brig-a", "anthropic")
	for i := range 99 {
		route := []string{"anthropic", "github"}[i%2]
		if _, err := r.h.Mint("brig-a", fmt.Sprint("exec-", i), route, DefaultTokenTTL); err != nil {
			t.Fatal(err)
		}
	}
	carrier, err := r.h.Mint("brig-a", "carrier", "anthropic", 2*DefaultTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(carrier), strings.NewReader(first))
	wantPlaceholder(t, r, resp, body, "anthropic", "body", "raw")
	r.clock.Advance(DefaultTokenTTL)
	resp, _ = r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(carrier), strings.NewReader(first))
	wantStatus(t, resp, http.StatusOK)
}

// The matcher holds no spelling, only the hash of each spelling's end, so
// a sandbox at maxLive tokens costs well under a MiB.
func TestMatcherMemoryIsBounded(t *testing.T) {
	var live []liveToken
	for i := range maxLive {
		live = append(live, liveToken{token: mustMint(t, fmt.Sprint("s", i)), route: "anthropic"})
	}
	m := newMatcher(live)
	size := len(m.anchors)*int(unsafe.Sizeof(anchored{})) + len(m.filter)*8
	for _, l := range m.tokens {
		size += len(l.token) + int(unsafe.Sizeof(l))
	}
	t.Logf("%d tokens: %d anchors, %d bytes", maxLive, len(m.anchors), size)
	if size > 1<<20 {
		t.Errorf("the matcher of %d tokens takes %d bytes", maxLive, size)
	}
}

// Every base64 alignment, alphabet and padding is found, with zero to five
// bytes before the token and zero to two after.
func TestBase64AtEveryAlignmentIsFound(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	for name, e := range map[string]*base64.Encoding{"std": base64.StdEncoding, "std-raw": base64.RawStdEncoding,
		"url": base64.URLEncoding, "url-raw": base64.RawURLEncoding} {
		for pre := range 6 {
			for post := range 3 {
				s := e.EncodeToString([]byte(strings.Repeat("P", pre) + tok + strings.Repeat("Q", post)))
				resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages", bearer(tok), strings.NewReader("x"+s+"x"))
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("%s pre=%d post=%d: %d", name, pre, post, resp.StatusCode)
				}
			}
		}
	}
}

// A body above the cap goes upstream byte for byte: the held start, then the
// rest as it streams.
func TestLargeChunkedBodyIsForwardedByteExact(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	body := make([]byte, 20<<20)
	_, _ = rand.Read(body)
	gh := http.Request{Header: http.Header{}}
	gh.SetBasicAuth("x-access-token", r.mint("brig-a", "github"))
	resp, _ := r.send("POST", "/brig-a/r/github/brig-sh/brig.git/git-receive-pack", gh.Header,
		io.MultiReader(bytes.NewReader(body)))
	wantStatus(t, resp, http.StatusOK)
	wantAudit(t, r.lastAudit(), "placeholder=partial")
	if got := up.last(t).body; got != string(body) {
		t.Errorf("the upstream got %d bytes, not the %d sent", len(got), len(body))
	}
}

// The spellings the scan finds in a body, as JSON and as text, and the ones
// it does not. docs/broker.md lists the misses under Limits; a change that
// finds one updates that list and moves it here to found.
func TestSpellingsTheScanFindsAndMisses(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	jsonStr := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	each := func(pick func(i int) bool, f func(c byte) string) string {
		var b strings.Builder
		for i := 0; i < len(tok); i++ {
			if pick(i) {
				b.WriteString(f(tok[i]))
			} else {
				b.WriteByte(tok[i])
			}
		}
		return b.String()
	}
	all := func(int) bool { return true }
	mixed := func(format string) func(c byte) string {
		return func(c byte) string {
			s := fmt.Sprintf(format, c)
			if c%2 == 0 {
				s = s[:len(s)-2] + strings.ToUpper(s[len(s)-2:])
			}
			return s
		}
	}
	found := map[string]string{
		"base64-wrapped-76":     splitEvery(base64.StdEncoding.EncodeToString([]byte("API_KEY="+tok+"\n")), 76, "\n"),
		"base64-of-path-escape": base64.StdEncoding.EncodeToString([]byte(url.PathEscape(tok))),
		// Sent as JSON, a JSON string inside a JSON string.
		"base64-wrapped-in-a-json-string": jsonStr(splitEvery(base64.StdEncoding.EncodeToString([]byte("K="+tok+"\n")), 76, "\n")),
	}
	missed := map[string]string{
		"percent-one-char":   each(func(i int) bool { return i == 5 }, func(c byte) string { return fmt.Sprintf("%%%02X", c) }),
		"percent-mixed-case": each(all, mixed("%%%02x")),
		"json-u-one-char":    each(func(i int) bool { return i == 5 }, func(c byte) string { return fmt.Sprintf(`\u%04x`, c) }),
		"json-u-mixed-case":  each(all, mixed(`\u%04x`)),
		"hex-with-colons":    splitEvery(hex.EncodeToString([]byte(tok)), 2, ":"),
		"hex-backslash-x":    each(all, func(c byte) string { return fmt.Sprintf(`\x%02x`, c) }),
		"split-by-dashes":    splitEvery(tok, 20, "-"),
		"html-entities":      each(all, func(c byte) string { return fmt.Sprintf("&#x%x;", c) }),
		"quoted-printable":   splitEvery(tok, 75, "=\r\n"),
		"base64-twice":       base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString([]byte(tok)))),
		"reversed": func() string {
			b := []byte(tok)
			slices.Reverse(b)
			return string(b)
		}(),
	}
	const path = "/brig-a/r/anthropic/v1/messages"
	for _, c := range []struct {
		set  map[string]string
		want int
	}{{found, http.StatusForbidden}, {missed, http.StatusOK}} {
		for name, enc := range c.set {
			for _, body := range []string{`{"c":` + jsonStr(enc) + `}`, "pre " + enc + " post"} {
				if resp, _ := r.send("POST", path, bearer(tok), strings.NewReader(body)); resp.StatusCode != c.want {
					t.Errorf("%s in %.12s...: %d, want %d", name, body, resp.StatusCode, c.want)
				}
			}
		}
	}
}

// The scan of an 8 MiB body stays within its bound on text that keeps
// hitting the start of the spellings, on the ends of real spellings, and on
// spellings off by their first byte. Measured at 21 to 35 ms on an M-series
// Mac; the test allows 500 ms, so a slow runner does not fail it.
func TestPlaceholderScanStaysWithinItsBound(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector slows the scan down too much for a bound on its time")
	}
	const bound = 500 * time.Millisecond
	for _, n := range []int{10, 64} {
		var live []liveToken
		for i := range n {
			live = append(live, liveToken{token: mustMint(t, fmt.Sprint("s", i)), route: "anthropic"})
		}
		m := newMatcher(live)
		for _, kind := range []string{"text", "anchors", "near"} {
			data := scanBody(m, kind)
			start := time.Now()
			hit := m.scan().write(data)
			if d := time.Since(start); d > bound {
				t.Errorf("%d tokens, %s: the scan took %s, bound %s", n, kind, d, bound)
			}
			if hit != nil {
				t.Errorf("%d tokens, %s: a body without a whole spelling matched", n, kind)
			}
		}
	}
}

// scanBody returns 8 MiB built to slow the scan down.
func scanBody(m *matcher, kind string) []byte {
	var body bytes.Buffer
	spell := func(a anchored) []byte {
		tok := m.tokens[a.tok].token
		b := make([]byte, a.length(tok))
		for i := range b {
			b[i] = a.at(tok, i)
		}
		return b
	}
	for i := 0; body.Len() < 8<<20; i++ {
		switch kind {
		case "text":
			// The start of every token, e, digits, escapes.
			words := strings.Fields(`the quick brown fox eyJzYW5kYm94Ijoi lazy e %65%79 6579 e ee {"role":"user"}`)
			body.WriteString(words[i*7%len(words)])
			body.WriteByte(" \n"[i%2])
		case "anchors":
			// The end of each spelling: an anchor hit every 32 bytes.
			for _, a := range m.anchors {
				s := spell(a)
				body.Write(s[len(s)-anchorLen:])
			}
		case "near":
			// Each spelling but its first byte: an anchor hit and a compare
			// that fails at once.
			for _, a := range m.anchors {
				s := spell(a)
				s[0] ^= 1
				body.Write(s)
			}
		}
	}
	return body.Bytes()[:8<<20]
}

// The adversarial bodies of TestPlaceholderScanStaysWithinItsBound, with 10
// and 64 live tokens.
func BenchmarkPlaceholderScanAdversarial(b *testing.B) {
	for _, n := range []int{10, 64} {
		var live []liveToken
		for i := range n {
			live = append(live, liveToken{token: mustMint(b, fmt.Sprint("s", i)), route: "anthropic"})
		}
		m := newMatcher(live)
		for _, kind := range []string{"text", "anchors", "near"} {
			data := scanBody(m, kind)
			b.Run(fmt.Sprintf("tokens=%d/%s", n, kind), func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				for b.Loop() {
					m.scan().write(data)
				}
			})
		}
	}
}

// A matcher with no live token keeps nothing of what it scanned. It used to
// keep the last -1 bytes, which is a slice out of range.
func TestAMatcherWithNoTokenScans(t *testing.T) {
	m := newMatcher(nil)
	if h := m.scan().write([]byte(`{"messages":[]}`)); h != nil {
		t.Errorf("a matcher with no token found %+v", h)
	}
	if h := m.findString("POST", "/v1/messages"); h != nil {
		t.Errorf("a matcher with no token found %+v", h)
	}
}

// The only token of a sandbox expires while the broker holds the request's
// body: it verified on arrival, and is gone from the live tokens by the scan.
// The request is refused as an expired token once the body is held, and the
// copy of the token in the body never reaches the upstream.
func TestATokenThatExpiresWhileItsBodyIsHeld(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up))
	tok := r.mint("brig-a", "anthropic")
	// Expect: 100-continue holds the body until the handler reads it, which
	// is after the token verified. The clock passes the token's expiry as
	// the body is sent.
	body := &advancing{r: strings.NewReader(`{"model":"claude","note":"` + tok + `"}`), advance: func() {
		r.clock.Advance(DefaultTokenTTL + time.Minute)
	}}
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(body.r.Len())
	req.Header = bearer(tok)
	req.Header.Set("Expect", "100-continue")
	c := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: 10 * time.Second}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("the request got no answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r.lines++
	if !body.advanced {
		t.Fatal("the body was sent without moving the clock")
	}
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d request(s), want none: the body held the token", n)
	}
	wantStatus(t, resp, http.StatusUnauthorized)
	wantAudit(t, r.lastAudit(), "status=401", "check=expired")
}

// advancing is a body that runs advance before its first byte is read.
type advancing struct {
	r        *strings.Reader
	advance  func()
	advanced bool
}

func (a *advancing) Read(p []byte) (int, error) {
	if !a.advanced {
		a.advanced = true
		a.advance()
	}
	return a.r.Read(p)
}

// panicking is a credential store that panics, as a bug in the handler
// would.
type panicking struct{}

func (panicking) Resolve(context.Context, string, secret.Workload) ([]byte, error) {
	panic("a bug")
}

// A panic in the handler is answered 500, and the audit line says
// check=panic with that status. net/http recovered it before and dropped
// the connection, and the line said status=200 for a request that never
// went upstream.
func TestAPanicIsAuditedAsOne(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up, claudeRoutes(t, up), func(c *Config) { c.Credentials = panicking{} })
	req, err := http.NewRequest("POST", r.srv.URL+"/brig-a/r/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(r.mint("brig-a", "anthropic"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the request got no answer: %v", err)
	}
	resp.Body.Close()
	r.lines++
	wantStatus(t, resp, http.StatusInternalServerError)
	wantAudit(t, r.lastAudit(), "status=500", "check=panic")
	if n := up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
}

// Building the matcher of a sandbox at maxLive tokens, which a mint causes
// on the next request.
func BenchmarkMatcherBuild(b *testing.B) {
	var live []liveToken
	for i := range maxLive {
		live = append(live, liveToken{token: mustMint(b, fmt.Sprint("s", i)), route: "anthropic"})
	}
	for b.Loop() {
		newMatcher(live)
	}
}
