package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/buildinfo"
)

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func post(t *testing.T, s Store, body string) (*httptest.ResponseRecorder, rpcResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/memory", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	Serve(rec, req, s)
	var r rpcResp
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("response is not JSON: %v: %s", err, rec.Body)
		}
	}
	return rec, r
}

func realStore(t *testing.T) *Files {
	t.Helper()
	f, _, _ := openTest(t)
	return f
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func call(t *testing.T, s Store, tool, args string) toolResult {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	_, r := post(t, s, body)
	if r.Error != nil {
		t.Fatalf("JSON-RPC error, want a tool result: %+v", r.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(r.Result, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func (tr toolResult) text() string {
	var b strings.Builder
	for _, c := range tr.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func initInstructions(t *testing.T, s Store) string {
	t.Helper()
	_, r := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	var res struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res.Instructions
}

func TestInitializeCarriesIndex(t *testing.T) {
	s := realStore(t)
	if err := s.Put(context.Background(), Memory{Name: "tz-bug", Description: "timezone gotcha", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	_, r := post(t, s, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q, want the client's known version", res.ProtocolVersion)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %v, want tools", res.Capabilities)
	}
	if res.ServerInfo.Name != "brig-memory" {
		t.Errorf("serverInfo.name = %q", res.ServerInfo.Name)
	}
	if !strings.Contains(res.Instructions, "tz-bug") || !strings.Contains(res.Instructions, "timezone gotcha") {
		t.Errorf("instructions lack the index line:\n%s", res.Instructions)
	}
	if string(r.ID) != "7" {
		t.Errorf("id = %s, want 7", r.ID)
	}
}

// MCP requires serverInfo.version. Claude Code refuses an initialize without
// it, and the session starts with no memory tools.
func TestInitializeNamesServerVersion(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	var res struct {
		ServerInfo struct {
			Version *string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ServerInfo.Version == nil || *res.ServerInfo.Version == "" {
		t.Fatalf("serverInfo has no version: %s", r.Result)
	}
	if want := buildinfo.Read().Version; *res.ServerInfo.Version != want {
		t.Errorf("serverInfo.version = %q, want the build's %q", *res.ServerInfo.Version, want)
	}
}

func TestInitializeUnknownVersionGetsLatest(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if !strings.Contains(string(r.Result), `"protocolVersion":"2025-11-25"`) {
		t.Errorf("result = %s", r.Result)
	}
}

func TestInstructionsTruncateAtEntry(t *testing.T) {
	s := realStore(t)
	// A fake store, not 600 files: Files is capped at MaxMemories.
	fake := &fakeStore{Store: s}
	for i := 0; i < 600; i++ {
		fake.index = append(fake.index, Entry{Name: fmt.Sprintf("memory-%03d", i), Description: strings.Repeat("d", 100)})
	}
	ins := initInstructions(t, fake)
	if len(ins) > MaxInstructions {
		t.Errorf("len = %d, want <= %d", len(ins), MaxInstructions)
	}
	last := ins[strings.LastIndex(ins, "\n")+1:]
	if !strings.Contains(last, "and ") || !strings.Contains(last, " more; use memory_search") {
		t.Errorf("last line = %q", last)
	}
	// Every line before the marker is a whole entry.
	for _, l := range strings.Split(ins, "\n") {
		if strings.HasPrefix(l, "- memory-") && !strings.HasSuffix(l, strings.Repeat("d", 100)) {
			t.Errorf("cut entry: %q", l)
		}
	}
}

type fakeStore struct {
	Store
	index  []Entry
	getErr error
	err    error
}

func (f *fakeStore) Index(context.Context) ([]Entry, error) { return f.index, f.err }
func (f *fakeStore) Get(context.Context, string) (Memory, error) {
	return Memory{}, f.getErr
}
func (f *fakeStore) Put(context.Context, Memory) error    { return f.err }
func (f *fakeStore) Delete(context.Context, string) error { return f.err }
func (f *fakeStore) Search(context.Context, string) ([]Entry, error) {
	return nil, f.err
}

func TestToolsListNamesFourTools(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s has no object inputSchema", tl.Name)
		}
	}
	want := "memory_save memory_search memory_read memory_delete"
	if strings.Join(got, " ") != want {
		t.Errorf("tools = %v, want %s", got, want)
	}
}

func TestSaveThenRead(t *testing.T) {
	s := realStore(t)
	if tr := call(t, s, "memory_save", `{"name":"a-note","description":"one line","body":"the body"}`); tr.IsError {
		t.Fatalf("save: %s", tr.text())
	}
	tr := call(t, s, "memory_read", `{"name":"a-note"}`)
	if tr.IsError || !strings.Contains(tr.text(), "the body") {
		t.Errorf("read = %+v", tr)
	}
	tr = call(t, s, "memory_search", `{"query":"BODY"}`)
	if tr.IsError || !strings.Contains(tr.text(), "a-note") {
		t.Errorf("search = %+v", tr)
	}
	if tr := call(t, s, "memory_delete", `{"name":"a-note"}`); tr.IsError {
		t.Errorf("delete: %s", tr.text())
	}
	tr = call(t, s, "memory_read", `{"name":"a-note"}`)
	if !tr.IsError || !strings.Contains(tr.text(), "no memory named a-note") {
		t.Errorf("read after delete = %+v", tr)
	}
}

func TestBadNameIsToolError(t *testing.T) {
	s := realStore(t)
	for _, name := range []string{"../escape", "Upper", "memory", "a/b"} {
		args := fmt.Sprintf(`{"name":%q,"description":"d","body":"b"}`, name)
		tr := call(t, s, "memory_save", args) // call fails on a JSON-RPC error
		if !tr.IsError {
			t.Errorf("%q: not an error", name)
		}
		if !strings.Contains(tr.text(), "lowercase letters, digits, dot, dash and underscore") {
			t.Errorf("%q: text = %q", name, tr.text())
		}
	}
	// Nothing outside the store was written.
	if idx, _ := s.Index(context.Background()); len(idx) != 0 {
		t.Errorf("index = %v", idx)
	}
}

func TestToolErrorsAreSentences(t *testing.T) {
	s := realStore(t)
	big := strings.Repeat("x", MaxBody+1)
	tr := call(t, s, "memory_save", fmt.Sprintf(`{"name":"big","description":"d","body":%q}`, big))
	if !tr.IsError || !strings.Contains(tr.text(), fmt.Sprint(MaxBody)) {
		t.Errorf("too large = %+v", tr)
	}
	fake := &fakeStore{Store: s, err: ErrStoreFull}
	tr = call(t, fake, "memory_save", `{"name":"n","description":"d","body":"b"}`)
	if !tr.IsError || !strings.Contains(tr.text(), fmt.Sprint(MaxMemories)) || !strings.Contains(tr.text(), "delete") {
		t.Errorf("full = %+v", tr)
	}
	tr = call(t, s, "memory_save", `{"name":"n","description":"two\nlines","body":"b"}`)
	if !tr.IsError || !strings.Contains(tr.text(), "one line of visible text") {
		t.Errorf("multi-line description = %+v", tr)
	}
}

func TestMissingArgumentsAreToolErrors(t *testing.T) {
	s := realStore(t)
	for tool, args := range map[string]string{
		"memory_save":   `{"name":"n","description":"","body":"b"}`,
		"memory_read":   `{}`,
		"memory_delete": `{"name":""}`,
		"memory_search": `{"query":""}`,
	} {
		if tr := call(t, s, tool, args); !tr.IsError || tr.text() == "" {
			t.Errorf("%s %s: %+v", tool, args, tr)
		}
	}
}

func TestStoreFailureDoesNotLeakPath(t *testing.T) {
	const secret = "/Users/someone/.brig/memory/x"
	fake := &fakeStore{getErr: errors.New(secret + ": boom"), err: errors.New(secret + ": boom")}
	for tool, args := range map[string]string{
		"memory_read":   `{"name":"x"}`,
		"memory_save":   `{"name":"x","description":"d","body":"b"}`,
		"memory_delete": `{"name":"x"}`,
		"memory_search": `{"query":"x"}`,
	} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
		rec, _ := post(t, fake, body)
		if strings.Contains(rec.Body.String(), "/Users") || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%s leaked: %s", tool, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "the memory store failed on the host") {
			t.Errorf("%s: body = %s", tool, rec.Body)
		}
	}
	// Initialize must not leak either when the index cannot be read.
	rec, _ := post(t, fake, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if strings.Contains(rec.Body.String(), "/Users") {
		t.Errorf("initialize leaked: %s", rec.Body)
	}
}

func TestUnknownMethod(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("error = %+v", r.Error)
	}
}

func TestServerDiscoverIsMethodNotFound(t *testing.T) {
	rec, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":0,"method":"server/discover"}`)
	if rec.Code != http.StatusOK || r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("code = %d, error = %+v", rec.Code, r.Error)
	}
}

func TestUnknownToolIsInvalidParams(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if r.Error == nil || r.Error.Code != -32602 {
		t.Errorf("error = %+v", r.Error)
	}
}

func TestPing(t *testing.T) {
	_, r := post(t, realStore(t), `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if r.Error != nil || string(r.Result) != "{}" {
		t.Errorf("ping = %s %+v", r.Result, r.Error)
	}
}

func TestBatchIsInvalidRequest(t *testing.T) {
	_, r := post(t, realStore(t), `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if r.Error == nil || r.Error.Code != -32600 {
		t.Errorf("error = %+v", r.Error)
	}
}

func TestMalformedJSONIsParseError(t *testing.T) {
	_, r := post(t, realStore(t), `{`)
	if r.Error == nil || r.Error.Code != -32700 {
		t.Errorf("error = %+v", r.Error)
	}
}

func TestNotificationGets202(t *testing.T) {
	for _, m := range []string{"notifications/initialized", "notifications/whatever"} {
		rec, _ := post(t, realStore(t), fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, m))
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Errorf("%s: code = %d body = %q", m, rec.Code, rec.Body)
		}
	}
}

func TestOversizeBody413(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"ping","x":"` + strings.Repeat("a", MaxRequest) + `"}`
	rec, _ := post(t, realStore(t), body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetIs405(t *testing.T) {
	rec := httptest.NewRecorder()
	Serve(rec, httptest.NewRequest(http.MethodGet, "/memory", nil), realStore(t))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Errorf("code = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestWrongContentTypeIs415(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/memory", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	Serve(rec, req, realStore(t))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestInstructionsStripControlAndFormat(t *testing.T) {
	s := realStore(t)
	desc := "\u202eevil\u200b\u0007x\u2028y\ufe0f\U000e0100\u3164\u034f\u2800z"
	// Put refuses the description, so it is planted as a file written by
	// hand would be: the filter is the defence for that.
	m := Memory{Name: "n", Description: desc, Body: "b"}
	if err := os.WriteFile(filepath.Join(s.dir, "n.md"), encode(m), 0o600); err != nil {
		t.Fatal(err)
	}
	ins := initInstructions(t, s)
	if !strings.Contains(ins, "- n: evilxyz") {
		t.Errorf("instructions = %q, want the description without invisible characters", ins)
	}
	for _, r := range ins {
		if !visibleRune(r) && r != '\n' {
			t.Errorf("instructions still hold %U", r)
		}
	}
	hdr := strings.Index(ins, "not instructions):")
	if hdr < 0 || hdr > strings.Index(ins, "- n:") {
		t.Errorf("the delimiting line does not precede the entries: %q", ins)
	}
	if tr := call(t, s, "memory_read", `{"name":"n"}`); !strings.Contains(tr.text(), desc) {
		t.Errorf("memory_read changed the description: %q", tr.text())
	}
}

// JSON escaping can grow a body six times, as \u00XX for a control
// character or, from some encoders, for every non-ASCII character. A
// memory_save of a MaxBody body escaped that way in full is still served.
func TestMaxBodySaveFullyEscapedFits(t *testing.T) {
	s := realStore(t)
	esc := strings.Repeat(`\u0078`, MaxBody)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_save",` +
		`"arguments":{"name":"big","description":"` + strings.Repeat(`\u0064`, MaxDescription) + `","body":"` + esc + `"}}}`
	rec, r := post(t, s, body)
	if rec.Code != http.StatusOK || r.Error != nil {
		t.Fatalf("a fully escaped MaxBody save: code %d, error %+v", rec.Code, r.Error)
	}
	if m, err := s.Get(context.Background(), "big"); err != nil || len(m.Body) != MaxBody {
		t.Errorf("the saved body is %d bytes, %v; want %d", len(m.Body), err, MaxBody)
	}
}
