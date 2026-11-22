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

// toolBetas are Anthropic-Beta values that may let a request declare or
// change tools inside messages. request.body.tools reads the top-level tools
// alone, and brig has not read the shapes these betas carry, so a route with
// tools rules drops them, as the presets drop a value they have not
// reviewed.
var toolBetas = []string{"inline-tools-2026-09-15", "mid-conversation-tool-changes-2026-07-01"}

// dropToolBetas removes the toolBetas from the Anthropic-Beta values of h,
// in any case, and returns how many it removed.
func dropToolBetas(h http.Header) int {
	values := h.Values("Anthropic-Beta")
	var keep []string
	dropped := 0
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			item = strings.TrimSpace(item)
			switch {
			case item == "":
			case slices.ContainsFunc(toolBetas, func(b string) bool { return strings.EqualFold(b, item) }):
				dropped++
			default:
				keep = append(keep, item)
			}
		}
	}
	if dropped == 0 {
		return 0
	}
	h.Del("Anthropic-Beta")
	if len(keep) > 0 {
		h.Set("Anthropic-Beta", strings.Join(keep, ","))
	}
	return dropped
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
	if len(rules.ToolResultDeny) > 0 {
		// A model or a tool name that an allow list let through is a value
		// the policy named, not text. A pattern that matched it would refuse
		// every request for that model or tool.
		vetted := func(at []*jsonFrame) bool {
			return len(rules.Models) > 0 && atModel(at) ||
				rules.Tools != nil && len(rules.Tools.Allow) > 0 && atToolName(at)
		}
		hit, err := textHit(rules.ToolResultDeny, body, vetted)
		if err != nil {
			return &bodyRefusal{reason: "json", message: unreadableBody}
		}
		if hit {
			// The message says which rule, never what matched: the guest
			// wrote the text, and the answer must not hand a secret back to
			// it.
			return &bodyRefusal{"toolResultDeny", "text",
				"brig broker refused this request: text in the request body matches request.body.toolResultDeny"}
		}
	}
	return nil
}

// textHit reports whether a pattern matches a string of body, a key or a
// value, at any depth. The model reads more than tool results: the system
// prompt, the history the guest sends back, tool inputs, documents and
// search results, at whatever depth the API nests them. A list of the parts
// to read would miss the next block type, so every string is read but a
// value at a position skip names. The patterns were proved by validation,
// so they compile.
//
// It reads tokens rather than decoding into a map, because a map keeps one
// of two equal keys and the upstream may read the other.
func textHit(patterns []string, body []byte, skip func([]*jsonFrame) bool) (bool, error) {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	hit := func(s string) bool {
		return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(s) })
	}
	var stack []*jsonFrame
	// done marks the end of a value: in an object, a key comes next.
	done := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			if k, ok := t.(string); ok {
				stack[n-1].key, stack[n-1].wantKey = k, false
				if hit(k) {
					return true, nil
				}
				continue
			}
		}
		switch v := t.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, &jsonFrame{object: true, wantKey: true})
			case '[':
				stack = append(stack, &jsonFrame{})
			default:
				stack = stack[:len(stack)-1]
				done()
			}
		case string:
			if !skip(stack) && hit(v) {
				return true, nil
			}
			done()
		default:
			done()
		}
	}
}

// atModel reports whether textHit is at the value of the top-level model.
func atModel(stack []*jsonFrame) bool {
	return len(stack) == 1 && stack[0].object && stack[0].key == "model"
}

// atToolName reports whether textHit is at the name of an entry of the
// top-level tools.
func atToolName(stack []*jsonFrame) bool {
	return len(stack) == 3 && stack[0].object && stack[0].key == "tools" &&
		!stack[1].object && stack[2].object && stack[2].key == "name"
}

// jsonFrame is an object or an array that textHit is inside. key is the key
// of the member being read, and wantKey says a key comes next.
type jsonFrame struct {
	object, wantKey bool
	key             string
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
}

type tool struct{ Name string }

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
	var tools []json.RawMessage
	if err := member(f, "tools", &tools); err != nil {
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
	return nil
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
