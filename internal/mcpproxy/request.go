package mcpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Verdict is what Decide concluded about one client message. Its string is
// the outcome recorded on the audit line.
type Verdict string

const (
	Forward      Verdict = "forwarded"
	RefuseTool   Verdict = "refused:tool"
	RefuseMethod Verdict = "refused:method"
	RefuseBatch  Verdict = "refused:batch"
	RefuseParse  Verdict = "refused:parse"
	RefuseParams Verdict = "refused:params"
)

// Decision is the verdict on one client message, with what the audit line and
// the error response need.
type Decision struct {
	Verdict Verdict
	Method  string          // JSON-RPC method, for the audit line
	Tool    string          // tools/call name, if any
	ID      json.RawMessage // for the error response
	Body    []byte          // body to forward (initialize rewritten); nil unless Forward
}

// Allow reports whether the policy names a tool. It sees the name exactly as
// the client sent it.
type Allow func(tool string) bool

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
)

// strippedCapabilities are the client capabilities that let the server send
// the client a request of its own. The guest cannot answer for the policy, so
// a server that never learns the client has them never asks.
var strippedCapabilities = []string{"sampling", "elicitation", "roots"}

// Decide judges one client message. Only initialize, ping, notifications/*,
// tools/list and a tools/call of an allowed tool are forwarded, with the
// client's replies to server requests. A nil allow denies every tool.
func Decide(body []byte, allow Allow) Decision {
	trimmed := bytes.TrimSpace(body)
	// A batch is refused whole: the tools it names are checked one by one
	// nowhere, and a refused member would still need an answer in the array.
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return Decision{Verdict: RefuseBatch}
	}
	msg, err := parseObject(trimmed, true)
	if err != nil {
		return Decision{Verdict: RefuseParse}
	}
	d := Decision{ID: msg["id"]}

	rawMethod, hasMethod := msg["method"]
	if !hasMethod {
		// The client's reply to a server ping.
		_, hasResult := msg["result"]
		_, hasError := msg["error"]
		if hasResult || hasError {
			d.Verdict, d.Body = Forward, canonical(msg)
			return d
		}
		d.Verdict = RefuseMethod
		return d
	}
	if err := json.Unmarshal(rawMethod, &d.Method); err != nil {
		d.Verdict = RefuseParams
		return d
	}

	switch {
	case d.Method == "ping", d.Method == "tools/list", strings.HasPrefix(d.Method, "notifications/"):
		d.Verdict, d.Body = Forward, canonical(msg)
	case d.Method == "initialize":
		if err := stripCapabilities(msg); err != nil {
			d.Verdict = RefuseParams
			return d
		}
		d.Verdict, d.Body = Forward, canonical(msg)
	case d.Method == "tools/call":
		return decideCall(msg, d, allow)
	default:
		d.Verdict = RefuseMethod
	}
	return d
}

func decideCall(msg map[string]json.RawMessage, d Decision, allow Allow) Decision {
	params, err := parseObject(msg["params"], true)
	if err != nil {
		d.Verdict = RefuseParams
		return d
	}
	if err := json.Unmarshal(params["name"], &d.Tool); err != nil || d.Tool == "" {
		d.Tool = ""
		d.Verdict = RefuseParams
		return d
	}
	if allow == nil || !allow(d.Tool) {
		d.Verdict = RefuseTool
		return d
	}
	d.Verdict, d.Body = Forward, canonical(msg)
	return d
}

// canonical is the body that is forwarded: the members Decide read, and no
// others. Forwarding the client's original bytes would let any difference
// between how Decide and the upstream read them, such as a member the
// upstream matches under another spelling, carry a message past the
// decision.
func canonical(msg map[string]json.RawMessage) []byte {
	out, _ := json.Marshal(struct {
		JSONRPC json.RawMessage `json:"jsonrpc,omitempty"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  json.RawMessage `json:"method,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   json.RawMessage `json:"error,omitempty"`
	}{msg["jsonrpc"], msg["id"], msg["method"], msg["params"], msg["result"], msg["error"]})
	return out
}

// stripCapabilities removes, from an initialize in msg, the client
// capabilities that invite server-to-client requests. A message without
// params.capabilities has nothing to remove.
func stripCapabilities(msg map[string]json.RawMessage) error {
	if len(msg["params"]) == 0 {
		return nil
	}
	// params keeps its protocolVersion and clientInfo spellings, so only the
	// spelling of "capabilities" is pinned: a "Capabilities" member would
	// reach a case-insensitive upstream unstripped.
	params, err := parseObject(msg["params"], false)
	if err != nil {
		return err
	}
	for k := range params {
		if k != "capabilities" && strings.EqualFold(k, "capabilities") {
			return errors.New("case-variant capabilities")
		}
	}
	rawCaps, ok := params["capabilities"]
	if !ok {
		return nil
	}
	caps, err := parseObject(rawCaps, true)
	if err != nil {
		return err
	}
	for _, c := range strippedCapabilities {
		delete(caps, c)
	}
	if params["capabilities"], err = json.Marshal(caps); err != nil {
		return err
	}
	msg["params"], err = json.Marshal(params)
	return err
}

// parseObject decodes one JSON object into its members. It refuses trailing
// data, a key that appears twice, and a key with a non-ASCII byte: parsers
// disagree on which duplicate wins, and Go's decoder folds case with Unicode
// rules ("paramſ" matches "params"), so the proxy and the upstream could read
// different members out of the same bytes. With strict, a key with an ASCII
// uppercase letter is refused too: the keys of JSON-RPC and of the members
// Decide reads are lowercase, and "Method" would be read as method upstream.
func parseObject(raw []byte, strict bool) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("not an object")
	}
	members := map[string]json.RawMessage{}
	folded := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := tok.(string)
		for i := 0; i < len(key); i++ {
			if c := key[i]; c >= 0x80 || (strict && c >= 'A' && c <= 'Z') {
				return nil, errors.New("key is not lowercase ASCII")
			}
		}
		if lower := strings.ToLower(key); folded[lower] {
			return nil, errors.New("duplicate key")
		} else {
			folded[lower] = true
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		members[key] = v
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return members, nil
}

// ErrorBody is the JSON-RPC error response to a refused message, carrying the
// request's id (null when it had none or could not be parsed).
func ErrorBody(d Decision) []byte {
	code, msg := codeNoMethod, "method not found"
	switch d.Verdict {
	case RefuseParse:
		code, msg = codeParse, "parse error"
	case RefuseBatch:
		code, msg = codeInvalidRequest, "batch requests are not supported"
	case RefuseParams:
		code, msg = codeInvalidParams, "invalid params"
	case RefuseTool:
		msg = "tool not allowed by policy"
	}
	id := d.ID
	if len(id) == 0 || d.Verdict == RefuseParse || d.Verdict == RefuseBatch {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, msg}})
	return out
}
