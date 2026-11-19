package mcpproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func only(names ...string) Allow {
	return func(tool string) bool {
		for _, n := range names {
			if n == tool {
				return true
			}
		}
		return false
	}
}

func call(id, name string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
}

func errorOf(t *testing.T, d Decision) (id string, code int, msg string) {
	t.Helper()
	var r struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ErrorBody(d), &r); err != nil {
		t.Fatalf("ErrorBody is not JSON: %v", err)
	}
	return string(r.ID), r.Error.Code, r.Error.Message
}

func TestDecideForwardsAllowedCall(t *testing.T) {
	in := call("1", "a")
	d := Decide([]byte(in), only("a"))
	if d.Verdict != Forward || d.Tool != "a" || d.Method != "tools/call" {
		t.Fatalf("decision = %+v", d)
	}
	if string(d.Body) != in {
		t.Errorf("body = %s, want the input", d.Body)
	}
}

func TestDecideRefusesDeniedCall(t *testing.T) {
	d := Decide([]byte(call(`"x-7"`, "b")), only("a"))
	if d.Verdict != RefuseTool || d.Body != nil || d.Tool != "b" {
		t.Fatalf("decision = %+v", d)
	}
	id, code, msg := errorOf(t, d)
	if id != `"x-7"` || code != -32601 || msg != "tool not allowed by policy" {
		t.Errorf("error = %s %d %q", id, code, msg)
	}
}

// The allowlist compares the name byte for byte. Folding case, trimming
// space or normalising would let "A" or "a " reach an upstream that treats it
// as "a".
func TestDecideToolNameIsExact(t *testing.T) {
	for _, name := range []string{"A", "a ", " a", "a​", "a\\u0000", "ab", ""} {
		d := Decide([]byte(call("1", name)), only("a"))
		if d.Verdict == Forward {
			t.Errorf("name %q was forwarded", name)
		}
	}
	// Key case and duplicates: a Go upstream would match "Name" to name, and a
	// duplicate key resolves differently between parsers.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","Name":"b"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"b","name":"a"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"ping","params":{"name":"b"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"a"}}`,
	} {
		if d := Decide([]byte(body), only("a")); d.Verdict == Forward {
			t.Errorf("%s was forwarded", body)
		}
	}
}

func TestDecideCallWithoutToolName(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":null}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`,
	} {
		d := Decide([]byte(body), only("a"))
		if d.Verdict != RefuseParams || d.Body != nil {
			t.Errorf("%s: decision = %+v", body, d)
		}
		if _, code, _ := errorOf(t, d); code != -32602 {
			t.Errorf("code = %d", code)
		}
	}
	// "*" is the policy's allow-everything, but a call still names a tool.
	if d := Decide([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`), func(string) bool { return true }); d.Verdict != RefuseParams {
		t.Errorf("allow-all forwarded a call without a name: %+v", d)
	}
}

func TestDecideRefusesBatch(t *testing.T) {
	in := "  [" + call("1", "a") + "," + call("2", "b") + "]"
	d := Decide([]byte(in), only("a"))
	if d.Verdict != RefuseBatch || d.Body != nil {
		t.Fatalf("decision = %+v", d)
	}
	if _, code, _ := errorOf(t, d); code != -32600 {
		t.Errorf("code = %d", code)
	}
}

func TestDecideRefusesOtherMethods(t *testing.T) {
	for _, m := range []string{"resources/read", "prompts/get", "completion/complete", "logging/setLevel", "tools/call ", "Tools/List", "notification/x"} {
		d := Decide([]byte(`{"jsonrpc":"2.0","id":3,"method":"`+m+`"}`), only("a"))
		if d.Verdict != RefuseMethod || d.Body != nil || d.Method != m {
			t.Errorf("%s: decision = %+v", m, d)
			continue
		}
		if id, code, _ := errorOf(t, d); id != "3" || code != -32601 {
			t.Errorf("%s: error id=%s code=%d", m, id, code)
		}
	}
}

func TestDecidePassesLifecycle(t *testing.T) {
	for _, m := range []string{"ping", "notifications/initialized", "notifications/cancelled", "tools/list"} {
		in := `{"jsonrpc":"2.0","id":1,"method":"` + m + `"}`
		d := Decide([]byte(in), only())
		if d.Verdict != Forward || d.Method != m || string(d.Body) != in {
			t.Errorf("%s: decision = %+v", m, d)
		}
	}
}

// A client's reply to a server ping is a response, not a request, and is
// forwarded.
func TestDecideForwardsClientResponses(t *testing.T) {
	for _, in := range []string{`{"jsonrpc":"2.0","id":9,"result":{}}`, `{"jsonrpc":"2.0","id":9,"error":{"code":-1,"message":"x"}}`} {
		d := Decide([]byte(in), only())
		if d.Verdict != Forward || d.Method != "" || string(d.Body) != in {
			t.Errorf("%s: decision = %+v", in, d)
		}
	}
	if d := Decide([]byte(`{"jsonrpc":"2.0","id":9}`), only()); d.Verdict == Forward {
		t.Errorf("an object with no method, result or error was forwarded")
	}
}

// A server that cannot ask the client for sampling, elicitation or roots
// cannot make the client act on its behalf.
func TestDecideStripsClientCapabilities(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"},` +
		`"capabilities":{"sampling":{},"elicitation":{},"roots":{"listChanged":true},"experimental":{"x":1}}}}`
	d := Decide([]byte(in), only())
	if d.Verdict != Forward || d.Method != "initialize" {
		t.Fatalf("decision = %+v", d)
	}
	var out struct {
		ID     int `json:"id"`
		Params struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			ClientInfo      map[string]string          `json:"clientInfo"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(d.Body, &out); err != nil {
		t.Fatal(err)
	}
	caps := out.Params.Capabilities
	if _, ok := caps["experimental"]; !ok || len(caps) != 1 {
		t.Errorf("capabilities = %v, want only experimental", caps)
	}
	if out.ID != 1 || out.Params.ProtocolVersion != "2025-06-18" || out.Params.ClientInfo["name"] != "c" {
		t.Errorf("other fields lost: %s", d.Body)
	}
}

func TestDecideInitializeWithoutCapabilitiesIsUnchanged(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"x"}}`
	d := Decide([]byte(in), only())
	if d.Verdict != Forward || string(d.Body) != in {
		t.Errorf("decision = %+v", d)
	}
}

func TestDecideParseError(t *testing.T) {
	for _, in := range []string{`{`, ``, `null`, `1`, `"x"`, `{"method":"ping"} {}`} {
		d := Decide([]byte(in), only("a"))
		if d.Verdict != RefuseParse || d.Body != nil {
			t.Errorf("%q: decision = %+v", in, d)
			continue
		}
		id, code, _ := errorOf(t, d)
		if code != -32700 || id != "null" {
			t.Errorf("%q: id=%s code=%d", in, id, code)
		}
	}
	if !strings.Contains(string(ErrorBody(Decision{Verdict: RefuseParse})), `"jsonrpc":"2.0"`) {
		t.Error("error body lacks jsonrpc version")
	}
}

func refused(t *testing.T, body string, want Verdict) {
	t.Helper()
	d := Decide([]byte(body), only("a"))
	if d.Verdict != want || d.Body != nil {
		t.Errorf("%s: decision = %+v, want %s and no body", body, d, want)
	}
}

// A message with no exact "method" but a result was forwarded as a client
// response, and a case-insensitive upstream read its Method and Params.
func TestDecideRefusesCaseVariantKeys(t *testing.T) {
	refused(t, `{"jsonrpc":"2.0","id":1,"result":{},"Method":"tools/call","Params":{"name":"denied"}}`, RefuseParse)
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","Params":{"name":"a"}}`, RefuseParse)
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"ping","Ĳ":1}`, RefuseParse)
	refused(t, `{"jsonrpc":"2.0","id":1,"méthod":"ping"}`, RefuseParse)
	// U+017F folds to s under encoding/json.
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a"},"paramſ":{"name":"b"}}`, RefuseParse)
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","naMe":"b"}}`, RefuseParams)
	for _, caps := range []string{`{"Sampling":{}}`, `{"ROOTS":{}}`, `{"elicitatioN":{}}`} {
		refused(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":`+caps+`}}`, RefuseParams)
	}
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"Capabilities":{"sampling":{}}}}`, RefuseParams)
	refused(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilitieſ":{"sampling":{}}}}`, RefuseParams)
}

func TestDecideForwardsACanonicalBody(t *testing.T) {
	in := `{ "jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name":"a","arguments":{"x":1}}, "extra": 1 }`
	d := Decide([]byte(in), only("a"))
	if d.Verdict != Forward {
		t.Fatalf("decision = %+v", d)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["extra"]; ok || len(got) != 4 {
		t.Errorf("body = %s, want only jsonrpc, id, method, params", d.Body)
	}
	for k, want := range map[string]string{"jsonrpc": `"2.0"`, "id": `4`, "method": `"tools/call"`, "params": `{"name":"a","arguments":{"x":1}}`} {
		if string(got[k]) != want {
			t.Errorf("%s = %s, want %s", k, got[k], want)
		}
	}
}
