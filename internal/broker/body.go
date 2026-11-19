package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// messagesPath is the Anthropic Messages API call that request.body reads.
const messagesPath = "/v1/messages"

// modelsPath lists the models. It and the call that describes one model are
// the read calls a route with request.body passes unread: a GET there takes
// no body the upstream reads and answers with no tool call.
const modelsPath = "/v1/models"

// bodyCall is what a route with request.body does with a request.
type bodyCall int

const (
	// refuseCall is every call but the two below. /v1/chat/completions,
	// /v1/complete and /v1/messages/batches take a model, tools and a
	// prompt in shapes the rules do not read, and answer where the
	// rewrite does not look, so a list of what to refuse would always
	// trail the API.
	refuseCall bodyCall = iota
	// checkCall is POST /v1/messages, whose body the rules read.
	checkCall
	// readCall is GET /v1/models or GET /v1/models/{id}.
	readCall
)

// callOf sorts a request on a route with request.body. A trailing slash, a
// ;parameter on any segment, or a path in another case is the same call to
// an upstream that strips or folds them, so none of them moves a request
// from one kind to another. The method of the checked call is compared in
// any case too: Go's server passes "post" on as sent, and an upstream may
// serve it as POST.
func callOf(method, rest string) bodyCall {
	segs := strings.Split(rest, "/")
	for i, s := range segs {
		if j := strings.IndexAny(s, semicolons); j >= 0 {
			segs[i] = s[:j]
		}
	}
	path := strings.ToLower(strings.TrimRight(strings.Join(segs, "/"), "/"))
	switch {
	case strings.EqualFold(method, http.MethodPost) && path == messagesPath:
		return checkCall
	case method == http.MethodGet && path == modelsPath:
		return readCall
	case method == http.MethodGet && strings.HasPrefix(path, modelsPath+"/") &&
		!strings.Contains(path[len(modelsPath)+1:], "/"):
		return readCall
	}
	return refuseCall
}

// refusedCall is the message of a call that a route with request.body does
// not serve.
func refusedCall(method, rest string) string {
	return "brig broker: a route with request.body serves POST /v1/messages, GET /v1/models " +
		"and GET /v1/models/{id}; " + method + " " + rest + " is refused"
}

// bodyRules returns the request.body rules of a route, or nil.
func bodyRules(spec *v1alpha1.Route) *v1alpha1.RequestBody {
	if spec.Request == nil {
		return nil
	}
	return spec.Request.Body
}

// encodedRequest reports whether h gives the body a content coding other than
// identity. The rules read the body as sent, so a compressed one is refused
// rather than inflated a second time.
func encodedRequest(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				return true
			}
		}
	}
	return false
}

// bodyRefusal is a request.body miss. Its reason goes to the audit line, so
// it holds only what the broker chose to keep of the body: a model or tool
// name clipped to 64 bytes, a max_tokens number, or a fixed word. The audit
// line never holds the body, and the text a toolResultDeny pattern matched is
// the kind of thing the rule exists to keep out of logs as well as upstream.
type bodyRefusal struct {
	// field is the request.body rule that missed, such as "models", or ""
	// for a body the broker could not read.
	field, reason, message string
}

// status is 400 for a body the broker could not read and 403 for a miss.
func (r *bodyRefusal) status() int {
	if r.field == "" {
		return http.StatusBadRequest
	}
	return http.StatusForbidden
}

// errorType is the Anthropic error type of the status, so a client that reads
// the API's errors reads the broker's.
func (r *bodyRefusal) errorType() string {
	if r.field == "" {
		return "invalid_request_error"
	}
	return "permission_error"
}

func (r *bodyRefusal) rule(route string) string {
	rule := "spec.routes[" + route + "].request.body"
	if r.field == "" {
		return rule
	}
	return rule + "." + r.field
}

// unreadableBody is the message of a body the rules cannot read. A body the
// broker cannot read whole is not forwarded: what it did not read, it did not
// check.
const unreadableBody = "brig broker: a request to /v1/messages on a route with request.body rules is JSON of at most 8 MiB"

// clipName is the most of a model or tool name a refusal carries.
const clipName = 64

// checkBody checks a POST /v1/messages against the route's request.body. It
// runs before the credential is resolved, so a refused request never reads
// it. held is nil for a request without a body.
func checkBody(rules *v1alpha1.RequestBody, held *heldBody) *bodyRefusal {
	if rules == nil {
		return nil
	}
	var body []byte
	if held != nil {
		var ok bool
		if body, ok = held.complete(); !ok {
			return &bodyRefusal{reason: "size", message: unreadableBody}
		}
	}
	var req messagesRequest
	if err := decodeMessages(body, &req); err != nil {
		return &bodyRefusal{reason: "json", message: unreadableBody}
	}
	if len(rules.Models) > 0 && !slices.ContainsFunc(rules.Models, func(g string) bool { return matchStar(g, req.Model) }) {
		model := clip(req.Model, clipName)
		return &bodyRefusal{"models", "model=" + model,
			fmt.Sprintf("brig broker refused this request: model %q is not in request.body.models %s", model, list(rules.Models))}
	}
	if rules.MaxTokens > 0 && req.MaxTokens != nil && *req.MaxTokens > float64(rules.MaxTokens) {
		n := number(*req.MaxTokens)
		return &bodyRefusal{"maxTokens", "max_tokens=" + n,
			fmt.Sprintf("brig broker refused this request: max_tokens %s is over request.body.maxTokens %d", n, rules.MaxTokens)}
	}
	if t := rules.Tools; t != nil {
		for _, tool := range req.Tools {
			if slices.Contains(t.Deny, tool.Name) || (len(t.Allow) > 0 && !slices.Contains(t.Allow, tool.Name)) {
				name := clip(tool.Name, clipName)
				return &bodyRefusal{"tools", "tool=" + name,
					fmt.Sprintf("brig broker refused this request: the tool %q is refused by request.body.tools", name)}
			}
		}
	}
	if len(rules.ToolResultDeny) > 0 && toolResultHit(rules.ToolResultDeny, req.Messages) {
		// The message says which rule, never what matched: the guest wrote
		// the text, and the answer must not hand a secret back to it.
		return &bodyRefusal{"toolResultDeny", "tool_result",
			"brig broker refused this request: a tool result or user text matches request.body.toolResultDeny"}
	}
	return nil
}

// toolResultHit reports whether a pattern matches the text of a tool_result
// block or a user text block. The patterns were proved by validation, so
// they compile.
func toolResultHit(patterns []string, msgs []message) bool {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	hit := func(s string) bool {
		return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(s) })
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch {
			case b.Type == "tool_result":
				if slices.ContainsFunc(b.Content, hit) {
					return true
				}
			case b.Type == "text" && m.Role == "user":
				if hit(b.Text) {
					return true
				}
			}
		}
	}
	return false
}

// number writes a max_tokens value as an integer when it is one, which is
// every value a client sends. A fraction or a huge value keeps its shortest
// form, so the reason never rounds a value under the cap.
func number(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return clip(strconv.FormatFloat(f, 'g', -1, 64), clipName)
}

// messagesRequest is what the rules read of a Messages API request.
type messagesRequest struct {
	Model     string
	MaxTokens *float64
	Tools     []tool
	Messages  []message
}

type tool struct{ Name string }

type message struct {
	Role string
	// Content is the message's blocks. A plain string is one text block.
	Content []block
}

type block struct {
	Type, Text string
	// Content is the text of a tool_result: its string content, or the text
	// of each of its text blocks.
	Content []string
}

// textBlock is a block of a tool_result's content.
type textBlock struct{ Type, Text string }

// decodeMessages reads a Messages API request into req.
//
// encoding/json matches keys without regard to case and keeps the last of
// two equal keys. The upstream reads "model" alone, so a body with "model"
// and "MODEL" would be checked for one model and served by another. Every
// object the rules read is decoded by its exact keys, and a key that appears
// twice, or a body that is not valid UTF-8, is refused instead of guessed.
func decodeMessages(body []byte, req *messagesRequest) error {
	if !utf8.Valid(body) {
		return errBadBody
	}
	f, err := fields(body)
	if err != nil {
		return err
	}
	if err := member(f, "model", &req.Model); err != nil {
		return err
	}
	if err := member(f, "max_tokens", &req.MaxTokens); err != nil {
		return err
	}
	var tools, msgs []json.RawMessage
	if err := member(f, "tools", &tools); err != nil {
		return err
	}
	if err := member(f, "messages", &msgs); err != nil {
		return err
	}
	for _, raw := range tools {
		tf, err := fields(raw)
		if err != nil {
			return err
		}
		var t tool
		if err := member(tf, "name", &t.Name); err != nil {
			return err
		}
		req.Tools = append(req.Tools, t)
	}
	for _, raw := range msgs {
		m, err := decodeMessage(raw)
		if err != nil {
			return err
		}
		req.Messages = append(req.Messages, m)
	}
	return nil
}

func decodeMessage(raw json.RawMessage) (message, error) {
	var m message
	mf, err := fields(raw)
	if err != nil {
		return m, err
	}
	if err := member(mf, "role", &m.Role); err != nil {
		return m, err
	}
	content, ok := mf["content"]
	if !ok {
		return m, nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		m.Content = []block{{Type: "text", Text: s}}
		return m, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return m, err
	}
	for _, rb := range blocks {
		bf, err := fields(rb)
		if err != nil {
			return m, err
		}
		var b block
		if err := member(bf, "type", &b.Type); err != nil {
			return m, err
		}
		if b.Type == "text" {
			if err := member(bf, "text", &b.Text); err != nil {
				return m, err
			}
		}
		if b.Type == "tool_result" {
			if b.Content, err = toolResultText(bf["content"]); err != nil {
				return m, err
			}
		}
		m.Content = append(m.Content, b)
	}
	return m, nil
}

// toolResultText returns the text of a tool_result's content: the string, or
// the text of each text block.
func toolResultText(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var out []string
	for _, rb := range blocks {
		bf, err := fields(rb)
		if err != nil {
			return nil, err
		}
		var tb textBlock
		if err := member(bf, "type", &tb.Type); err != nil {
			return nil, err
		}
		if tb.Type != "text" {
			continue
		}
		if err := member(bf, "text", &tb.Text); err != nil {
			return nil, err
		}
		out = append(out, tb.Text)
	}
	return out, nil
}

// errBadBody is a body that is not one JSON object with distinct keys.
var errBadBody = errors.New("not a JSON object with distinct keys")

// fields splits one JSON object into its members by their exact keys. It
// refuses anything but an object, a key that appears twice, and bytes after
// the object.
func fields(b []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errBadBody
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errBadBody
		}
		if _, dup := out[key]; dup {
			return nil, errBadBody
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, errBadBody
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errBadBody
	}
	return out, nil
}

// member decodes the member key of f into v, and leaves v as it is when f has
// no such member.
func member(f map[string]json.RawMessage, key string, v any) error {
	raw, ok := f[key]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// writeAnthropicError answers in the error shape of the Anthropic API, so a
// client shows the message rather than a parse failure.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	type apiError struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	body, _ := json.Marshal(struct {
		Type  string   `json:"type"`
		Error apiError `json:"error"`
	}{"error", apiError{errType, message}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// matchStar matches s against a glob where * is the only wildcard and matches
// any run of characters, none included. A model name holds slashes and dots,
// which path.Match would treat apart.
func matchStar(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return s == pattern
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	s = s[len(first) : len(s)-len(last)]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}
