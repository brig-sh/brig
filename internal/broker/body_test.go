package broker

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// leakedKey is what a toolResultDeny pattern is for: a cloud key an agent read
// from a file and is about to hand the model.
const leakedKey = "AKIAABCDEFGHIJKLMNOP"

// bodyRig is a broker with one anthropic route on preset, with rules as its
// request.body, and the header that carries the route's session token.
type bodyRig struct {
	*rig
	preset v1alpha1.Preset
}

func newBodyRig(t *testing.T, preset v1alpha1.Preset, rules *v1alpha1.RequestBody) *bodyRig {
	t.Helper()
	secretName := "claude-credentials"
	if preset == v1alpha1.PresetAnthropicAPIKey {
		secretName = "anthropic-api-key"
	}
	up := newUpstream(t, nil)
	r := newRig(t, up, []Route{
		presetRouteWith(t, preset, "anthropic", secretName, up.URL, &v1alpha1.RequestPolicy{Body: rules}),
	})
	return &bodyRig{rig: r, preset: preset}
}

// header carries the session token the way the preset's client does: a
// bearer token for claude-code, x-api-key for anthropic-api-key.
func (b *bodyRig) header() http.Header {
	tok := b.mint("brig-a", "anthropic")
	h := http.Header{"Content-Type": {"application/json"}}
	if b.preset == v1alpha1.PresetAnthropicAPIKey {
		h.Set("X-Api-Key", tok)
	} else {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

func (b *bodyRig) messages(body string) (*http.Response, string) {
	b.t.Helper()
	return b.do("POST", "/brig-a/r/anthropic/v1/messages", b.header(), body)
}

// wantNothingRead checks that a refusal neither resolved the credential nor
// reached the upstream.
func (b *bodyRig) wantNothingRead(t *testing.T) {
	t.Helper()
	if n := b.up.count(); n != 0 {
		t.Errorf("the upstream saw %d requests", n)
	}
	if res, _ := b.creds.counts(); res != 0 {
		t.Errorf("Resolve ran %d times before a refusal", res)
	}
}

// wantAnthropicError checks a refusal in the error shape of the Anthropic API.
func wantAnthropicError(t *testing.T, resp *http.Response, body string, status int, errType string) {
	t.Helper()
	wantStatus(t, resp, status)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", err, body)
	}
	if e.Type != "error" || e.Error.Type != errType || e.Error.Message == "" {
		t.Errorf("refusal %s, want type error and error.type %s with a message", body, errType)
	}
}

// userTurn is a request with one user message whose content is content.
func userTurn(model, content string) string {
	return `{"model":"` + model + `","max_tokens":1024,"messages":[{"role":"user","content":` + content + `}]}`
}

func TestRequestBodyRefusalsHappenBeforeResolve(t *testing.T) {
	akia := []string{`AKIA[0-9A-Z]{16}`}
	for _, tc := range []struct {
		name   string
		rules  v1alpha1.RequestBody
		body   string
		field  string
		reason string
	}{
		{"model", v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5*"}},
			userTurn("claude-opus-4-1", `"hi"`), "models", `reason="model=claude-opus-4-1"`},
		{"tool denied", v1alpha1.RequestBody{Tools: &v1alpha1.ToolRules{Deny: []string{"Bash"}}},
			`{"model":"claude-sonnet-4-5","tools":[{"name":"Read","input_schema":{}},{"name":"Bash","input_schema":{}}],"messages":[]}`,
			"tools", `reason="tool=Bash"`},
		{"tool outside the allow list", v1alpha1.RequestBody{Tools: &v1alpha1.ToolRules{Allow: []string{"Read"}}},
			`{"model":"claude-sonnet-4-5","tools":[{"name":"Read"},{"name":"Bash"}],"messages":[]}`,
			"tools", `reason="tool=Bash"`},
		{"max_tokens", v1alpha1.RequestBody{MaxTokens: 4096},
			`{"model":"claude-sonnet-4-5","max_tokens":8192,"messages":[]}`, "maxTokens", `reason="max_tokens=8192"`},
		{"tool_result string", v1alpha1.RequestBody{ToolResultDeny: akia},
			userTurn("claude-sonnet-4-5", `[{"type":"tool_result","tool_use_id":"toolu_01","content":"key=`+leakedKey+`"}]`),
			"toolResultDeny", "reason=tool_result"},
		{"tool_result text block", v1alpha1.RequestBody{ToolResultDeny: akia},
			userTurn("claude-sonnet-4-5", `[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"`+leakedKey+`"}]}]`),
			"toolResultDeny", "reason=tool_result"},
		{"user text block", v1alpha1.RequestBody{ToolResultDeny: akia},
			userTurn("claude-sonnet-4-5", `[{"type":"text","text":"use `+leakedKey+`"}]`),
			"toolResultDeny", "reason=tool_result"},
		{"user string content", v1alpha1.RequestBody{ToolResultDeny: akia},
			userTurn("claude-sonnet-4-5", `"use `+leakedKey+`"`), "toolResultDeny", "reason=tool_result"},
	} {
		for _, preset := range []v1alpha1.Preset{v1alpha1.PresetClaudeCode, v1alpha1.PresetAnthropicAPIKey} {
			t.Run(tc.name+"/"+string(preset), func(t *testing.T) {
				rules := tc.rules
				r := newBodyRig(t, preset, &rules)
				resp, body := r.messages(tc.body)
				wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
				line := r.lastAudit()
				wantAudit(t, line, "status=403", "check=body", "policy=test-anthropic",
					"rule=spec.routes[anthropic].request.body."+tc.field, tc.reason)
				r.wantNothingRead(t)
				if strings.Contains(body, leakedKey) || strings.Contains(r.audit.String(), leakedKey) {
					t.Errorf("the matched text left the broker:\nbody %s\naudit %s", body, r.audit.String())
				}
			})
		}
	}
}

func TestRequestBodyRefusesAnUnreadableBody(t *testing.T) {
	rules := &v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5*"}}
	for _, tc := range []struct{ name, body string }{
		{"not JSON", "model=claude-sonnet-4-5"},
		{"empty", ""},
		{"an array", `[{"model":"claude-sonnet-4-5"}]`},
		{"bytes after the object", `{"model":"claude-sonnet-4-5"} {}`},
		{"a key twice", `{"model":"claude-sonnet-4-5","model":"claude-opus-4-1"}`},
		{"a field of another type", `{"model":"claude-sonnet-4-5","max_tokens":"8192"}`},
		{"invalid UTF-8", "{\"model\":\"claude-sonnet-4-5\xff\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBodyRig(t, v1alpha1.PresetClaudeCode, rules)
			resp, body := r.messages(tc.body)
			wantAnthropicError(t, resp, body, http.StatusBadRequest, "invalid_request_error")
			wantAudit(t, r.lastAudit(), "status=400", "check=body", "rule=spec.routes[anthropic].request.body ", "reason=json")
			r.wantNothingRead(t)
		})
	}
	t.Run("over the cap", func(t *testing.T) {
		r := newBodyRig(t, v1alpha1.PresetClaudeCode, rules)
		big := bytes.Repeat([]byte(" "), maxReplay+1)
		resp, body := r.send("POST", "/brig-a/r/anthropic/v1/messages", r.header(), bytes.NewReader(big))
		wantAnthropicError(t, resp, body, http.StatusBadRequest, "invalid_request_error")
		wantAudit(t, r.lastAudit(), "status=400", "check=body", "rule=spec.routes[anthropic].request.body ", "reason=size")
		r.wantNothingRead(t)
	})
}

// gzipped compresses s.
func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRequestBodyRefusesACompressedBody(t *testing.T) {
	r := newBodyRig(t, v1alpha1.PresetClaudeCode, &v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5*"}})
	h := r.header()
	h.Set("Content-Encoding", "gzip")
	resp, _ := r.send("POST", "/brig-a/r/anthropic/v1/messages", h,
		bytes.NewReader(gzipped(t, userTurn("claude-sonnet-4-5", `"hi"`))))
	wantStatus(t, resp, http.StatusUnsupportedMediaType)
	wantAudit(t, r.lastAudit(), "status=415", "check=content-encoding")
	r.wantNothingRead(t)
}

func TestRequestBodyPassesAnAllowedRequestUnchanged(t *testing.T) {
	rules := &v1alpha1.RequestBody{
		Models:         []string{"claude-sonnet-4-5*", "claude-haiku-4-5"},
		Tools:          &v1alpha1.ToolRules{Allow: []string{"Read", "Grep"}, Deny: []string{"Bash"}},
		MaxTokens:      4096,
		ToolResultDeny: []string{`AKIA[0-9A-Z]{16}`},
	}
	for _, preset := range []v1alpha1.Preset{v1alpha1.PresetClaudeCode, v1alpha1.PresetAnthropicAPIKey} {
		t.Run(string(preset), func(t *testing.T) {
			r := newBodyRig(t, preset, rules)
			for _, sent := range []string{
				// Spacing and key order the broker would not write itself, to
				// show the upstream gets the guest's bytes.
				`{ "max_tokens": 4096, "model": "claude-sonnet-4-5-20250929",` +
					` "tools": [ {"name": "Read"}, {"name": "Grep"} ],` +
					` "messages": [ {"role": "user", "content": "list the files"},` +
					` {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_01", "name": "Read", "input": {}}]},` +
					` {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": "README.md"}]} ] }`,
				// No max_tokens passes a maxTokens rule.
				`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`,
				// The pattern reads tool results and user text, not what the
				// assistant said.
				`{"model":"claude-haiku-4-5","messages":[{"role":"assistant","content":"` + leakedKey + `"}]}`,
			} {
				resp, _ := r.messages(sent)
				wantStatus(t, resp, http.StatusOK)
				if got := r.up.last(t); got.body != sent || got.path != "/v1/messages" {
					t.Errorf("the upstream got %s %q, want /v1/messages %q", got.path, got.body, sent)
				}
			}
			// A call outside /v1/messages is not parsed: a GET has no
			// messages. The other calls under /v1/messages carry the same
			// fields where the rules cannot read them, so they are refused.
			notJSON := "tools=Bash&model=claude-opus-4-1"
			resp, _ := r.do("GET", "/brig-a/r/anthropic/v1/models", r.header(), notJSON)
			wantStatus(t, resp, http.StatusOK)
			if got := r.up.last(t); got.body != notJSON {
				t.Errorf("GET /v1/models: the upstream got %q", got.body)
			}
			for _, path := range []string{"/v1/messages/count_tokens", "/v1/messages/batches"} {
				resp, body := r.do("POST", "/brig-a/r/anthropic"+path, r.header(), notJSON)
				wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
			}
			// The 415 is for the rule path alone: a gzip body on a call the
			// rules do not read goes upstream as sent.
			h := r.header()
			h.Set("Content-Encoding", "gzip")
			zipped := gzipped(t, `{"model":"claude-opus-4-1"}`)
			resp, _ = r.send("GET", "/brig-a/r/anthropic/v1/models", h, bytes.NewReader(zipped))
			wantStatus(t, resp, http.StatusOK)
			if got := r.up.last(t); got.body != string(zipped) {
				t.Error("the upstream did not get the gzip body of /v1/models as sent")
			}
		})
	}
}

func TestRequestBodyDenyWinsOverAllow(t *testing.T) {
	r := newBodyRig(t, v1alpha1.PresetClaudeCode, &v1alpha1.RequestBody{
		Tools: &v1alpha1.ToolRules{Allow: []string{"Bash"}, Deny: []string{"Bash"}},
	})
	resp, body := r.messages(`{"model":"claude-sonnet-4-5","tools":[{"name":"Bash"}],"messages":[]}`)
	wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
	wantAudit(t, r.lastAudit(), "check=body", "rule=spec.routes[anthropic].request.body.tools", `reason="tool=Bash"`)
	r.wantNothingRead(t)
}

// A key the upstream does not read must not stand in for one it does:
// encoding/json alone would take MODEL for model, and Name for name.
func TestRequestBodyReadsKeysAsTheUpstreamDoes(t *testing.T) {
	r := newBodyRig(t, v1alpha1.PresetClaudeCode, &v1alpha1.RequestBody{
		Models: []string{"claude-sonnet-4-5*"},
		Tools:  &v1alpha1.ToolRules{Deny: []string{"Bash"}},
	})
	resp, body := r.messages(`{"model":"claude-opus-4-1","MODEL":"claude-sonnet-4-5","messages":[]}`)
	wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
	wantAudit(t, r.lastAudit(), `reason="model=claude-opus-4-1"`)
	resp, body = r.messages(`{"model":"claude-sonnet-4-5","tools":[{"name":"Bash","Name":"Read"}],"messages":[]}`)
	wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
	wantAudit(t, r.lastAudit(), `reason="tool=Bash"`)
	r.wantNothingRead(t)
}

// A trailing slash, a ;parameter or a query is the same call to an upstream
// that strips or ignores them, so each is checked.
func TestRequestBodyCoversEverySpellingOfTheCall(t *testing.T) {
	r := newBodyRig(t, v1alpha1.PresetClaudeCode, &v1alpha1.RequestBody{Models: []string{"claude-sonnet-4-5*"}})
	for _, path := range []string{"/v1/messages/", "/v1/messages;x", "/v1/messages?beta=true", "/V1/Messages"} {
		resp, body := r.do("POST", "/brig-a/r/anthropic"+path, r.header(), userTurn("claude-opus-4-1", `"hi"`))
		wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
	}
	r.wantNothingRead(t)
}

func TestMatchStar(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4-5", true},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929", false},
		{"claude-sonnet-4-5*", "claude-sonnet-4-5", true},
		{"claude-sonnet-4-5*", "claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4-5*", "claude-opus-4-1", false},
		{"*", "", true},
		{"*-4-5", "claude-haiku-4-5", true},
		{"claude-*-4-5*", "claude-haiku-4-5-20251001", true},
		{"claude-*-4-5*", "claude-opus-4-1", false},
		{"a*a", "a", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"us.anthropic.claude-*", "us.anthropic.claude-sonnet-4-5/v1", true},
	} {
		if got := matchStar(tc.pattern, tc.s); got != tc.want {
			t.Errorf("matchStar(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMessagesSiblingsAreRefusedOnABodyRoute(t *testing.T) {
	for _, rest := range []string{"/v1/messages/batches", "/v1/messages/count_tokens", "/v1/messages/batches/b1/results", "/v1/Messages/batches/", "/v1/messages/batches;x=1"} {
		if !messagesSibling(rest) {
			t.Errorf("%q is not a sibling", rest)
		}
	}
	for _, rest := range []string{"/v1/messages", "/v1/messages/", "/v1/messages;x=1", "/v1/models", "/v1/messagesx"} {
		if messagesSibling(rest) {
			t.Errorf("%q is a sibling", rest)
		}
	}
}
