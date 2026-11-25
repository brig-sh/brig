package mcpproxy

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// oneByte hands out a single byte per Read, so an event is never seen whole.
type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func (o oneByte) Close() error { return nil }

func body(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func filter(t *testing.T, ct, in, method string, allow Allow) (string, []Dropped, error) {
	t.Helper()
	var drops []Dropped
	rc, _, err := FilterResponse(ct, body(in), method, allow, func(d Dropped) { drops = append(drops, d) })
	if err != nil {
		return "", drops, err
	}
	defer rc.Close()
	out, err := io.ReadAll(rc)
	return string(out), drops, err
}

func toolNames(t *testing.T, raw string) (names []string, cursor string) {
	t.Helper()
	var m struct {
		Result struct {
			Tools      []struct{ Name string } `json:"tools"`
			NextCursor string                  `json:"nextCursor"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	for _, tl := range m.Result.Tools {
		names = append(names, tl.Name)
	}
	return names, m.Result.NextCursor
}

const toolsList = `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a","inputSchema":{}},{"name":"b"},{"name":"c"}],"nextCursor":"n1"}}`

func TestFilterJSONToolsList(t *testing.T) {
	out, _, err := filter(t, "application/json", toolsList, "tools/list", only("a", "c"))
	if err != nil {
		t.Fatal(err)
	}
	names, cursor := toolNames(t, out)
	if strings.Join(names, ",") != "a,c" || cursor != "n1" {
		t.Fatalf("names %v cursor %q: %s", names, cursor, out)
	}
	if !strings.Contains(out, `"id":2`) || !strings.Contains(out, `"jsonrpc":"2.0"`) {
		t.Fatalf("lost jsonrpc or id: %s", out)
	}
}

func TestFilterSSEToolsList(t *testing.T) {
	in := "id: 7\nevent: message\ndata: " + toolsList + "\n\n"
	out, _, err := filter(t, "text/event-stream; charset=utf-8", in, "tools/list", only("a", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id: 7\n") || !strings.Contains(out, "event: message\n") {
		t.Fatalf("lost id or event field: %q", out)
	}
	data := strings.TrimSuffix(strings.SplitN(out, "data: ", 2)[1], "\n\n")
	names, cursor := toolNames(t, data)
	if strings.Join(names, ",") != "a,c" || cursor != "n1" || strings.Contains(out, `"b"`) {
		t.Fatalf("names %v cursor %q: %q", names, cursor, out)
	}
}

func TestFilterSSEMultilineData(t *testing.T) {
	in := "data: {\"jsonrpc\":\"2.0\",\"id\":2,\n" +
		"data: \"result\":{\"tools\":[{\"name\":\"a\"},{\"name\":\"b\"}]}\n" +
		"data: }\n\n"
	rc, _, err := FilterResponse("text/event-stream", oneByte{strings.NewReader(in)}, "tools/list", only("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"b"`) {
		t.Fatalf("unlisted tool reached the guest: %q", out)
	}
	if names, _ := toolNames(t, strings.TrimSuffix(strings.TrimPrefix(string(out), "data: "), "\n\n")); strings.Join(names, ",") != "a" {
		t.Fatalf("names %v: %q", names, out)
	}
}

func TestFilterDropsServerRequest(t *testing.T) {
	for _, m := range []string{"sampling/createMessage", "elicitation/create", "roots/list"} {
		in := "data: {\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"" + m + "\",\"params\":{}}\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"content\":[]}}\n\n"
		out, drops, err := filter(t, "text/event-stream", in, "tools/call", only("a"))
		if err != nil {
			t.Fatal(err)
		}
		if len(drops) != 1 || drops[0].Method != m {
			t.Fatalf("%s: drops %v", m, drops)
		}
		if strings.Contains(out, m) || !strings.Contains(out, "notifications/progress") || !strings.Contains(out, `"id":3`) {
			t.Fatalf("%s: out %q", m, out)
		}
	}
}

func TestFilterJSONServerRequest(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`
	_, drops, err := filter(t, "application/json", in, "tools/call", only("a"))
	if err == nil {
		t.Fatal("a server request in a JSON response was passed")
	}
	if len(drops) != 1 || drops[0].Method != "roots/list" {
		t.Fatalf("drops %v", drops)
	}
}

func TestFilterRefusesUnknownContentType(t *testing.T) {
	for _, ct := range []string{"text/plain", "", "application/jsonx", "not a type;;"} {
		if _, _, err := filter(t, ct, toolsList, "tools/list", only("a")); err == nil {
			t.Errorf("content type %q accepted", ct)
		}
	}
}

func TestFilterEventTooLarge(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"result":{"x":"` + strings.Repeat("a", 1<<20) + `"}}`
	if _, _, err := filter(t, "text/event-stream", "data: "+big+"\n\n", "tools/call", only("a")); err == nil {
		t.Error("oversized SSE event accepted")
	}
	if _, _, err := filter(t, "application/json", big, "tools/call", only("a")); err == nil {
		t.Error("oversized JSON body accepted")
	}
	// Many small data lines add up too.
	many := strings.Repeat("data: "+strings.Repeat("a", 1000)+"\n", 1100) + "\n"
	if _, _, err := filter(t, "text/event-stream", many, "tools/call", only("a")); err == nil {
		t.Error("event of many data lines accepted")
	}
}

func TestFilterToolsCallUntouched(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"<&>"}],"Extra": 1}}`
	out, _, err := filter(t, "application/json", in, "tools/call", only("a"))
	if err != nil || out != in {
		t.Fatalf("err %v, out %q", err, out)
	}
	sse := "id: 1\ndata: " + in + "\n\n"
	out, _, err = filter(t, "text/event-stream", sse, "tools/call", only("a"))
	if err != nil || out != sse {
		t.Fatalf("err %v, out %q", err, out)
	}
}

// The decision "has both method and id" is made on keys the proxy and a
// lenient client could read differently, so such a message is refused whole.
func TestFilterRefusesAmbiguousMessage(t *testing.T) {
	for _, in := range []string{
		`{"jsonrpc":"2.0","ID":1,"method":"roots/list"}`,
		`{"jsonrpc":"2.0","id":1,"Method":"roots/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"roots/list","method":"x"}`,
		`{"jsonrpc":"2.0","id":1,"méthod":"roots/list"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"roots/list"}]`,
		`not json`,
	} {
		if _, _, err := filter(t, "application/json", in, "tools/call", only("a")); err == nil {
			t.Errorf("JSON %s passed", in)
		}
		_, _, err := filter(t, "text/event-stream", "data: "+in+"\n\n", "tools/call", only("a"))
		if err == nil {
			t.Errorf("SSE %s passed", in)
		}
	}
}

func TestFilterToolsListDropsAmbiguousTools(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"Name":"b"},{"name":"a"},{"name":7},{"name":"b","description":"d"}]}}`
	out, _, err := filter(t, "application/json", in, "tools/list", only("a"))
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := toolNames(t, out); strings.Join(names, ",") != "a" {
		t.Fatalf("names %v: %s", names, out)
	}
	for _, bad := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"Tools":[{"name":"b"}]}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a"}],"tools":[]}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":"x"}}`,
	} {
		if _, _, err := filter(t, "application/json", bad, "tools/list", only("a")); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
}

func TestFilterSSEWithoutDataPassesThrough(t *testing.T) {
	in := ": keepalive\n\nid: 5\nretry: 1000\n\ndata:\nid: 6\n\n"
	out, _, err := filter(t, "text/event-stream", in, "tools/list", only("a"))
	if err != nil || out != in {
		t.Fatalf("err %v, out %q", err, out)
	}
}

func TestFilterSSELineEndings(t *testing.T) {
	in := "data: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":2,\"result\":{\"tools\":[{\"name\":\"b\"}]}}\r\n\r\n"
	out, _, err := filter(t, "text/event-stream", in, "tools/list", only("a"))
	if err != nil || strings.Contains(out, `"b"`) {
		t.Fatalf("err %v, out %q", err, out)
	}
}

func TestFilterErrorsAreReadErrors(t *testing.T) {
	// An event that fails after an earlier one was flushed still delivers the
	// earlier one, then fails the read.
	in := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\ndata: nope\n\n"
	out, _, err := filter(t, "text/event-stream", in, "tools/call", only("a"))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(out, "data: {\"jsonrpc\":\"2.0\",\"id\":1") {
		t.Fatalf("first event not flushed: %q", out)
	}
}

// A BOM-stripping client reads "data" out of a stream that opens with one, so
// the filter must judge that first event too.
func TestFilterSSEBOMDoesNotHideAnEvent(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage","params":{}}`
	out, drops, err := filter(t, "text/event-stream", "\xef\xbb\xbfdata: "+req+"\n\n", "tools/call", only("a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].Method != "sampling/createMessage" || strings.Contains(out, "sampling") {
		t.Fatalf("drops %v, out %q", drops, out)
	}
	out, _, err = filter(t, "text/event-stream", "\xef\xbb\xbfdata: "+toolsList+"\n\n", "tools/list", only("a"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"b"`) || !strings.Contains(out, `"a"`) {
		t.Fatalf("unfiltered after BOM: %q", out)
	}
	// Only the stream's start is special: a BOM later is part of a field name.
	in := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n\xef\xbb\xbfdata: " + req + "\n\n"
	if _, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
		t.Error("BOM inside the stream hid an event")
	}
	// One byte per read, so the BOM arrives split.
	rc, _, err := FilterResponse("text/event-stream", oneByte{strings.NewReader("\xef\xbb\xbfdata: " + req + "\n\n")}, "tools/call", only("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); strings.Contains(string(b), "sampling") {
		t.Fatalf("split BOM hid an event: %q", b)
	}
}

func TestFilterSSERefusesNonASCIIFieldName(t *testing.T) {
	in := "dátá: " + toolsList + "\n\n"
	if _, _, err := filter(t, "text/event-stream", in, "tools/list", only("a")); err == nil {
		t.Error("non-ASCII field name accepted")
	}
}

type closeRecorder struct {
	io.Reader
	closed int
}

func (c *closeRecorder) Close() error { c.closed++; return nil }

func TestFilterClosesBodyOnError(t *testing.T) {
	for _, ct := range []string{"text/plain", "", "not a type;;"} {
		b := &closeRecorder{Reader: strings.NewReader(toolsList)}
		if _, _, err := FilterResponse(ct, b, "tools/list", only("a"), nil); err == nil {
			t.Fatalf("%q accepted", ct)
		}
		if b.closed == 0 {
			t.Errorf("%q: body left open", ct)
		}
	}
	b := &closeRecorder{Reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)}
	if _, _, err := FilterResponse("application/json", b, "tools/call", only("a"), nil); err == nil || b.closed == 0 {
		t.Errorf("JSON error: err %v, closed %d", err, b.closed)
	}
}

// A server can put a tools result in the response to any request; the guest
// reads the names whatever request it made.
func TestFilterToolsInAnyResponse(t *testing.T) {
	for _, method := range []string{"tools/call", "ping", "initialize", ""} {
		out, _, err := filter(t, "application/json", toolsList, method, only("a"))
		if err != nil {
			t.Fatal(err)
		}
		if names, _ := toolNames(t, out); strings.Join(names, ",") != "a" {
			t.Fatalf("%s: names %v: %s", method, names, out)
		}
		out, _, err = filter(t, "text/event-stream", "data: "+toolsList+"\n\n", method, only("a"))
		if err != nil || strings.Contains(out, `"b"`) {
			t.Fatalf("%s: err %v, out %q", method, err, out)
		}
	}
	if _, _, err := filter(t, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"Tools":[{"name":"b"}]}}`, "tools/call", only("a")); err == nil {
		t.Error("case-variant tools passed in a tools/call response")
	}
}

// Other parsers break lines on bytes this one does not, so a field line that
// holds one could carry a second field or event past the filter.
func TestFilterSSERefusesLineBreakLookalikes(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"result":{}}`
	for name, seps := range map[string]string{
		"VT": "\x0b", "FF": "\x0c", "FS": "\x1c", "GS": "\x1d", "RS": "\x1e",
		"NEL": "\u0085", "LS": " ", "PS": " ",
	} {
		for _, in := range []string{
			"id: 1" + seps + "data: " + msg + "\n\n",
			"event: x" + seps + "\n\n",
			": c" + seps + "data: " + msg + "\n\n",
			"retry" + seps + ": 1\n\n",
			"data" + seps + ": " + msg + "\n\n",
		} {
			if _, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
				t.Errorf("%s: %q accepted", name, in)
			}
		}
	}
	// Raw in a data payload, the C0 ones are not JSON.
	for _, sep := range []string{"\x0b", "\x0c", "\x1c", "\x1d", "\x1e"} {
		in := `data: {"jsonrpc":"2.0","id":1,"result":{"x":"a` + sep + `b"}}` + "\n\n"
		if _, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
			t.Errorf("%q in data accepted", sep)
		}
		in = "data: {\"jsonrpc\":\"2.0\",\"id\":1," + sep + "\"result\":{}}\n\n"
		if _, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
			t.Errorf("%q between members accepted", sep)
		}
	}
}

func TestFilterSSEEscapesLineBreakLookalikesInData(t *testing.T) {
	in := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"x\":\"a b\u0085c d\"}}\n\n" +
		"id: 2\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"
	out, _, err := filter(t, "text/event-stream", in, "tools/call", only("a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{" ", "\u0085", " "} {
		if strings.Contains(out, raw) {
			t.Fatalf("raw %q reached the guest: %q", raw, out)
		}
	}
	for _, esc := range []string{"a\\u2028b", "b\\u0085c", "c\\u2029d"} {
		if !strings.Contains(out, esc) {
			t.Fatalf("missing %s: %q", esc, out)
		}
	}
	// The event that held none stays byte-identical.
	if !strings.HasSuffix(out, "id: 2\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n") {
		t.Fatalf("other event changed: %q", out)
	}
	// A rewritten tools/list is escaped the same way.
	tl := `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a","description":"x` + "\u0085" + `y"}]}}`
	out, _, err = filter(t, "text/event-stream", "data: "+tl+"\n\n", "tools/list", only("a"))
	if err != nil || strings.Contains(out, "\u0085") || !strings.Contains(out, `x\u0085y`) {
		t.Fatalf("err %v, out %q", err, out)
	}
}

// A decoder that is lenient about overlong or truncated sequences could read
// them as line breaks the checks for valid UTF-8 never matched.
func TestFilterRefusesInvalidUTF8(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"result":{"x":"a%sb"}}`
	for name, bad := range map[string]string{"C0 8A": "\xc0\x8a", "E0 80 8A": "\xe0\x80\x8a", "truncated E2 80": "\xe2\x80"} {
		in := "data: " + strings.Replace(msg, "%s", bad, 1) + "\n\n"
		if _, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
			t.Errorf("SSE data with %s accepted", name)
		}
		if _, _, err := filter(t, "text/event-stream", "id: 1"+bad+"\ndata: "+strings.Replace(msg, "%s", "", 1)+"\n\n", "tools/call", only("a")); err == nil {
			t.Errorf("SSE id with %s accepted", name)
		}
		if _, _, err := filter(t, "text/event-stream", "id: 1"+bad, "tools/call", only("a")); err == nil {
			t.Errorf("unterminated SSE id with %s accepted", name)
		}
		if _, _, err := filter(t, "application/json", strings.Replace(msg, "%s", bad, 1), "tools/call", only("a")); err == nil {
			t.Errorf("JSON body with %s accepted", name)
		}
	}
	ok := "id: café\ndata: " + strings.Replace(msg, "%s", "café", 1) + "\n\n"
	if out, _, err := filter(t, "text/event-stream", ok, "tools/call", only("a")); err != nil || out != ok {
		t.Fatalf("valid UTF-8 changed: err %v, out %q", err, out)
	}
	okJSON := strings.Replace(msg, "%s", "café", 1)
	if out, _, err := filter(t, "application/json", okJSON, "tools/call", only("a")); err != nil || out != okJSON {
		t.Fatalf("valid UTF-8 JSON changed: err %v, out %q", err, out)
	}
}

// The filter judges a body as UTF-8, so a client that decodes it under another
// charset could read JSON the filter never saw.
func TestFilterRefusesCharsetOtherThanUTF8(t *testing.T) {
	for _, ct := range []string{
		"application/json; charset=utf-7",
		"text/event-stream; charset=UTF-16",
		"application/json; charset=iso-8859-1",
		`application/json; charset=""`,
	} {
		in := toolsList
		if strings.HasPrefix(ct, "text/event-stream") {
			in = "data: " + toolsList + "\n\n"
		}
		if _, _, err := filter(t, ct, in, "tools/list", only("a")); err == nil {
			t.Errorf("content type %q accepted", ct)
		}
	}
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/json; charset=UTF-8",
		`application/json; charset="Utf-8"`,
		"text/event-stream; charset=UTF-8",
	} {
		in := toolsList
		if strings.HasPrefix(ct, "text/event-stream") {
			in = "data: " + toolsList + "\n\n"
		}
		if _, _, err := filter(t, ct, in, "tools/list", only("a")); err != nil {
			t.Errorf("content type %q refused: %v", ct, err)
		}
	}
}

// The filter judges only the data field. Any other field name is one a lenient
// client could still read as data, so it is refused rather than passed raw.
func TestFilterSSERefusesUnknownFieldName(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage","params":{}}`
	for _, field := range []string{"Data", "DATA", "data ", " data", "data\t", "x-data", "foo"} {
		in := field + ": " + req + "\n\n"
		if out, _, err := filter(t, "text/event-stream", in, "tools/call", only("a")); err == nil {
			t.Errorf("field %q accepted: %q", field, out)
		}
		// Without a colon the whole line is the field name.
		if out, _, err := filter(t, "text/event-stream", field+"\n\n", "tools/call", only("a")); err == nil {
			t.Errorf("bare field %q accepted: %q", field, out)
		}
	}
}

func TestFilterSSEKnownFieldsPass(t *testing.T) {
	in := ": comment\n:\nevent: message\nid: 3\nretry: 500\ndata\n\nevent\nid\nretry\n\n"
	out, _, err := filter(t, "text/event-stream", in, "tools/call", only("a"))
	if err != nil || out != in {
		t.Fatalf("err %v, out %q", err, out)
	}
	ok := "event: message\nid: 4\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	if out, _, err := filter(t, "text/event-stream", ok, "tools/call", only("a")); err != nil || out != ok {
		t.Fatalf("err %v, out %q", err, out)
	}
}

// The media type returned is the one judged, bare, so the caller need not
// parse the upstream's Content-Type again.
func TestFilterReturnsMediaType(t *testing.T) {
	for ct, want := range map[string]string{
		"Application/JSON; charset=UTF-8": "application/json",
		"text/event-stream; foo=bar":      "text/event-stream",
	} {
		rc, mt, err := FilterResponse(ct, body(`{"jsonrpc":"2.0","id":1,"result":{}}`), "ping", only("a"), nil)
		if err != nil {
			t.Fatalf("%q: %v", ct, err)
		}
		rc.Close()
		if mt != want {
			t.Errorf("%q: media type = %q, want %q", ct, mt, want)
		}
	}
}
