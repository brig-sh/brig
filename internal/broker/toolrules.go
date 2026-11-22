package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// isEventStream reports whether h gives the body the media type of an event
// stream. api.anthropic.com sends "text/event-stream; charset=utf-8": an
// exact match would send a real stream down the JSON branch, which would
// find no JSON and hand a denied block to the guest as the upstream wrote it.
func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && strings.EqualFold(mt, "text/event-stream")
}

// errAnswerTooLarge is a JSON answer to a POST /v1/messages too large to be
// read whole for its tool calls. It is refused rather than passed on, or cut
// to what was read: what the broker did not read, it did not check.
var errAnswerTooLarge = errors.New("the answer is over 8 MiB")

// rewriteToolUse replaces or drops the tool_use blocks of a Messages API
// answer whose tool request.body.toolUse denies. An event stream is rewritten
// event by event as it arrives; a JSON answer is read whole and rewritten. An
// error answer, or any status but 200, carries no tool call and passes as it
// is.
func (u *upstreamTrip) rewriteToolUse(resp *http.Response) error {
	if u.toolUse == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	ctx := resp.Request.Context()
	refused := func(tool string) { u.h.auditToolUse(ctx, u.audit, tool) }
	if isEventStream(resp.Header) {
		resp.Body = newSSERewriter(resp.Body, u.toolUse, refused)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReplay+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxReplay {
		return errAnswerTooLarge
	}
	// A body that is not a JSON message carries no tool call a client would
	// run, and goes back as it was read.
	body, _ = rewriteMessage(body, u.toolUse, refused)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	// The redactor dropped the length. The body is whole now, so it has one
	// again.
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// rewriteMessage rewrites the content of a Messages API answer. A tool_use
// block whose tool is denied becomes refusalBlock under refuse and is
// removed under drop, and refused is called once for each. When no tool_use
// block is left, a stop_reason of tool_use becomes end_turn, so the client
// does not go on to answer a tool call that is not there. While one is left,
// under either action, the client has a call to answer and stop_reason stays.
//
// The message and its blocks are decoded by their exact keys, as a client
// reads them, into raw values, so the fields the rewrite does not touch keep
// their bytes. The keys of a rewritten message come out in another order,
// which no client reads. It returns body unchanged, and false, when nothing
// was denied or the body is not a message.
func rewriteMessage(body []byte, rules *v1alpha1.ToolUseRules, refused func(string)) ([]byte, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil {
		return body, false
	}
	var content []json.RawMessage
	if json.Unmarshal(msg["content"], &content) != nil {
		return body, false
	}
	out := make([]json.RawMessage, 0, len(content))
	changed, toolUseLeft := false, false
	for _, raw := range content {
		typ, name := blockKind(raw)
		if typ != "tool_use" {
			out = append(out, raw)
			continue
		}
		if !denied(rules, name) {
			toolUseLeft = true
			out = append(out, raw)
			continue
		}
		changed = true
		refused(name)
		if rules.Mode() == v1alpha1.ToolUseRefuse {
			out = append(out, refusalBlock(name))
		}
	}
	if !changed {
		return body, false
	}
	enc, err := marshalRaw(out)
	if err != nil {
		return body, false
	}
	msg["content"] = enc
	var stop string
	if !toolUseLeft && json.Unmarshal(msg["stop_reason"], &stop) == nil && stop == "tool_use" {
		msg["stop_reason"] = json.RawMessage(`"end_turn"`)
	}
	b, err := marshalRaw(msg)
	if err != nil {
		return body, false
	}
	return b, true
}

// blockKind returns the type of a content block and, for a tool call, the
// tool's name. Both are read by their exact keys.
func blockKind(raw json.RawMessage) (typ, name string) {
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil {
		return "", ""
	}
	_ = json.Unmarshal(b["type"], &typ)
	_ = json.Unmarshal(b["name"], &name)
	return typ, name
}

// marshalRaw encodes v without escaping <, > and &, so the text the rewrite
// keeps has the bytes the upstream sent.
func marshalRaw(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// denied reports whether request.body.toolUse refuses a call to the tool.
func denied(rules *v1alpha1.ToolUseRules, name string) bool {
	return slices.Contains(rules.Deny, name)
}

// refusalText is what the model's turn says in place of a refused tool call.
// It names the tool and the reason, so the agent, and the person who reads
// the transcript, see why the call did not run.
func refusalText(tool string) string {
	return "brig broker refused the tool " + tool + ": the policy of this route does not allow it."
}

// refusalBlock is the text block that takes the place of a refused tool call.
func refusalBlock(tool string) json.RawMessage {
	b, _ := marshalRaw(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text", refusalText(tool)})
	return b
}
