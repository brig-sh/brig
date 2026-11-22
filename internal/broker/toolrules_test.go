package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// newToolUseRig is a broker with one claude-code route whose request.body
// holds rules as its toolUse, in front of an upstream served by h.
func newToolUseRig(t *testing.T, rules *v1alpha1.ToolUseRules, h http.HandlerFunc) *bodyRig {
	t.Helper()
	up := newUpstream(t, h)
	r := newRig(t, up, []Route{
		presetRouteWith(t, v1alpha1.PresetClaudeCode, "anthropic", "claude-credentials", up.URL,
			&v1alpha1.RequestPolicy{Body: &v1alpha1.RequestBody{ToolUse: rules}}),
	})
	return &bodyRig{rig: r, preset: v1alpha1.PresetClaudeCode}
}

// answerJSON serves a Messages API answer as JSON.
func answerJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// answerStream serves stream as an event stream, one flush per event.
func answerStream(contentType string, stream []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		f := w.(http.Flusher)
		for _, ev := range splitEvents(stream) {
			_, _ = io.WriteString(w, ev)
			f.Flush()
		}
	}
}

const (
	textAnswerBlock = `{"type":"text","text":"I will run the command."}`
	bashBlock       = `{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"rm -rf /"}}`
	readBlock       = `{"type":"tool_use","id":"toolu_02","name":"Read","input":{"file_path":"/etc/hosts"}}`
)

// answer is a Messages API answer with blocks as its content.
func answer(stop string, blocks ...string) string {
	return `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[` +
		strings.Join(blocks, ",") + `],"stop_reason":"` + stop + `","stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":30}}`
}

// sentMessage is what a test reads of an answer the guest got.
type sentMessage struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func readAnswer(t *testing.T, resp *http.Response, body string) sentMessage {
	t.Helper()
	wantStatus(t, resp, http.StatusOK)
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %q, body %d bytes", cl, len(body))
	}
	var m sentMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	return m
}

// wantToolUseAudit checks the line of one refused call and the request's own
// line after it. The refused call's line is written first, while the response
// is copied.
func (b *bodyRig) wantToolUseAudit(t *testing.T, tool string) {
	t.Helper()
	b.lines++
	req := b.waitAudit(b.lines)
	refused := b.waitAudit(b.lines - 1)
	wantAudit(t, refused, "workload=", "sandbox=brig-a", "route=anthropic", "method=POST", "path=/v1/messages",
		"check=tool_use", `reason="tool=`+tool+`"`)
	if strings.Contains(refused, "status=") {
		t.Errorf("the tool_use line carries a status: %s", refused)
	}
	wantAudit(t, req, "sandbox=brig-a", "route=anthropic", "status=200")
	if strings.Contains(req, "check=") {
		t.Errorf("the request's line carries a check: %s", req)
	}
}

// wantNoToolUseAudit checks that a request left only its own line.
func (b *bodyRig) wantNoToolUseAudit(t *testing.T) {
	t.Helper()
	if n := strings.Count(b.audit.String(), "check=tool_use"); n != 0 {
		t.Errorf("%d tool_use lines:\n%s", n, b.audit.String())
	}
}

func TestToolUseRefuseRewritesAJSONAnswer(t *testing.T) {
	r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse), answerJSON(http.StatusOK, answer("tool_use", textAnswerBlock, bashBlock)))
	resp, body := r.messages(userTurn("claude-sonnet-4-5", `"clean up"`))
	m := readAnswer(t, resp, body)
	if len(m.Content) != 2 || m.Content[0].Text != "I will run the command." ||
		m.Content[1].Type != "text" || m.Content[1].Text != refusalText("Bash") {
		t.Errorf("content %+v, want the text and the refusal", m.Content)
	}
	if m.StopReason != "end_turn" {
		t.Errorf("stop_reason %q, want end_turn", m.StopReason)
	}
	if m.Usage.OutputTokens != 30 {
		t.Errorf("usage changed: %+v", m.Usage)
	}
	if strings.Contains(body, "rm -rf") || strings.Contains(body, "toolu_01") {
		t.Errorf("the Bash call reached the guest:\n%s", body)
	}
	r.wantToolUseAudit(t, "Bash")
}

func TestToolUseDropRemovesTheBlock(t *testing.T) {
	r := newToolUseRig(t, bashRules(v1alpha1.ToolUseDrop), answerJSON(http.StatusOK, answer("tool_use", textAnswerBlock, bashBlock)))
	resp, body := r.messages(userTurn("claude-sonnet-4-5", `"clean up"`))
	m := readAnswer(t, resp, body)
	if len(m.Content) != 1 || m.Content[0].Type != "text" || m.Content[0].Text != "I will run the command." {
		t.Errorf("content %+v, want the text alone", m.Content)
	}
	if m.StopReason != "end_turn" {
		t.Errorf("stop_reason %q, want end_turn", m.StopReason)
	}
	r.wantToolUseAudit(t, "Bash")
}

func TestToolUseLeavesAKeptToolCall(t *testing.T) {
	for _, tc := range []struct {
		action v1alpha1.ToolUseAction
		types  []string
	}{
		{v1alpha1.ToolUseDrop, []string{"text", "tool_use"}},
		{v1alpha1.ToolUseRefuse, []string{"text", "text", "tool_use"}},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			r := newToolUseRig(t, bashRules(tc.action),
				answerJSON(http.StatusOK, answer("tool_use", textAnswerBlock, bashBlock, readBlock)))
			resp, body := r.messages(userTurn("claude-sonnet-4-5", `"look around"`))
			m := readAnswer(t, resp, body)
			var types []string
			for _, b := range m.Content {
				types = append(types, b.Type)
			}
			if strings.Join(types, ",") != strings.Join(tc.types, ",") {
				t.Fatalf("content types %q, want %q", types, tc.types)
			}
			last := m.Content[len(m.Content)-1]
			if last.Name != "Read" || string(last.Input) != `{"file_path":"/etc/hosts"}` {
				t.Errorf("the Read call is %+v", last)
			}
			if tc.action == v1alpha1.ToolUseRefuse && m.Content[1].Text != refusalText("Bash") {
				t.Errorf("content[1] %+v, want the refusal", m.Content[1])
			}
			if m.StopReason != "tool_use" {
				t.Errorf("stop_reason %q, want tool_use: the Read call is left", m.StopReason)
			}
			r.wantToolUseAudit(t, "Bash")
		})
	}
}

func TestToolUseRewritesAnEventStream(t *testing.T) {
	for _, action := range []v1alpha1.ToolUseAction{v1alpha1.ToolUseRefuse, v1alpha1.ToolUseDrop} {
		for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8"} {
			t.Run(string(action)+"/"+ct, func(t *testing.T) {
				fixture := streamFixture(t)
				r := newToolUseRig(t, bashRules(action), answerStream(ct, fixture))
				resp, body := r.messages(userTurn("claude-sonnet-4-5", `"clean up"`))
				wantStatus(t, resp, http.StatusOK)
				if got := resp.Header.Get("Content-Type"); got != ct {
					t.Errorf("Content-Type %q, want %q", got, ct)
				}
				wantFixtureRewritten(t, fixture, []byte(body), action)
				r.wantToolUseAudit(t, "Bash")
				if n := strings.Count(r.audit.String(), "check=tool_use"); n != 1 {
					t.Errorf("%d tool_use lines, want 1:\n%s", n, r.audit.String())
				}
			})
		}
	}
}

func TestToolUseLeavesAnUndeniedStreamUntouched(t *testing.T) {
	fixture := streamFixture(t)
	r := newToolUseRig(t, &v1alpha1.ToolUseRules{Deny: []string{"Write"}},
		answerStream("text/event-stream; charset=utf-8", fixture))
	resp, body := r.messages(userTurn("claude-sonnet-4-5", `"clean up"`))
	wantStatus(t, resp, http.StatusOK)
	if body != string(fixture) {
		t.Errorf("the stream changed:\n%s", body)
	}
	r.wantNoToolUseAudit(t)
}

func TestToolUseLeavesErrorsAndOtherPathsAlone(t *testing.T) {
	t.Run("an error answer", func(t *testing.T) {
		apiErr := `{"type":"error","error":{"type":"invalid_request_error","message":"tool_use Bash"}}`
		r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse), answerJSON(http.StatusBadRequest, apiErr))
		resp, body := r.messages(userTurn("claude-sonnet-4-5", `"hi"`))
		wantStatus(t, resp, http.StatusBadRequest)
		if body != apiErr {
			t.Errorf("body %q, want %q", body, apiErr)
		}
		r.wantNoToolUseAudit(t)
	})
	t.Run("a 400 shaped like an answer", func(t *testing.T) {
		looks := answer("tool_use", bashBlock)
		r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse), answerJSON(http.StatusBadRequest, looks))
		resp, body := r.messages(userTurn("claude-sonnet-4-5", `"hi"`))
		wantStatus(t, resp, http.StatusBadRequest)
		if body != looks {
			t.Errorf("body %q, want %q", body, looks)
		}
		r.wantNoToolUseAudit(t)
	})
	t.Run("count_tokens", func(t *testing.T) {
		// The call is refused before the upstream, so there is nothing to
		// rewrite and no tool_use audit line.
		looks := answer("tool_use", textAnswerBlock, bashBlock)
		r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse), answerJSON(http.StatusOK, looks))
		resp, body := r.do("POST", "/brig-a/r/anthropic/v1/messages/count_tokens", r.header(),
			userTurn("claude-sonnet-4-5", `"hi"`))
		wantAnthropicError(t, resp, body, http.StatusForbidden, "permission_error")
		r.wantNoToolUseAudit(t)
	})
}

// A JSON answer too large to read whole is refused, not cut to what was read
// or passed on unchecked.
func TestToolUseRefusesAnOversizedJSONAnswer(t *testing.T) {
	pad := `{"type":"text","text":"` + strings.Repeat("a", maxReplay) + `"}`
	r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse),
		answerJSON(http.StatusOK, answer("tool_use", pad, bashBlock)))
	resp, body := r.messages(userTurn("claude-sonnet-4-5", `"hi"`))
	wantStatus(t, resp, http.StatusBadGateway)
	if strings.Contains(body, "rm -rf") {
		t.Errorf("the Bash call reached the guest")
	}
	wantAudit(t, r.lastAudit(), "status=502", "check=response.size")
	r.wantNoToolUseAudit(t)
}

func TestToolUseRewriteRunsAfterRedaction(t *testing.T) {
	// The upstream reflects the credential into the input of both calls.
	reflect := func(w http.ResponseWriter, req *http.Request) {
		auth, _ := json.Marshal(req.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer("tool_use",
			`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":`+string(auth)+`}}`,
			`{"type":"tool_use","id":"toolu_02","name":"Read","input":{"file_path":`+string(auth)+`}}`))
	}
	r := newToolUseRig(t, bashRules(v1alpha1.ToolUseRefuse), reflect)
	resp, body := r.messages(userTurn("claude-sonnet-4-5", `"hi"`))
	m := readAnswer(t, resp, body)
	if strings.Contains(body, fakeAccess) {
		t.Errorf("the guest got the credential:\n%s", body)
	}
	if strings.Contains(body, `"name":"Bash"`) || strings.Contains(body, "toolu_01") {
		t.Errorf("the Bash call reached the guest:\n%s", body)
	}
	if len(m.Content) != 2 || m.Content[1].Name != "Read" ||
		!bytes.Contains(m.Content[1].Input, []byte("Bearer "+redacted)) {
		t.Errorf("content %+v, want the refusal and the redacted Read call", m.Content)
	}
	r.wantToolUseAudit(t, "Bash")
}
