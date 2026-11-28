package controlroom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRoom is a Control Room that records what the agent routes get.
type fakeRoom struct {
	mu        sync.Mutex
	calls     []string
	sandboxes [][]Sandbox
	events    [][]Event
	takes     map[string]bool // kinds it stores as themselves
	status    int
	retry     string // Retry-After sent with status
	seenKeys  map[string]bool
}

func (f *fakeRoom) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := r.Header.Get("X-API-Key"); got != "k3y" {
			t.Errorf("X-API-Key = %q", got)
		}
		c, err := r.Cookie("CSRF-TOKEN")
		if err != nil || c.Value == "" || c.Value != r.Header.Get("X-CSRFToken") {
			t.Errorf("CSRF cookie and header do not match")
		}
		f.calls = append(f.calls, r.URL.Path)
		if f.status != 0 {
			if f.retry != "" {
				w.Header().Set("Retry-After", f.retry)
			}
			w.WriteHeader(f.status)
			return
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields() // as the Control Room decodes
		switch r.URL.Path {
		case Base + "/agent/sandboxes":
			var body struct {
				Snapshot  bool      `json:"snapshot"`
				Sandboxes []Sandbox `json:"sandboxes"`
			}
			if err := dec.Decode(&body); err != nil {
				t.Errorf("R20 body: %v", err)
			}
			if body.Snapshot {
				t.Errorf("R20 sent a snapshot")
			}
			f.sandboxes = append(f.sandboxes, body.Sandboxes)
			_, _ = io.WriteString(w, `{"accepted":1}`)
		case Base + "/agent/events":
			var body struct {
				Events []Event `json:"events"`
			}
			if err := dec.Decode(&body); err != nil {
				t.Errorf("R21 body: %v", err)
			}
			f.events = append(f.events, body.Events)
			acc, dup, rej := 0, 0, 0
			for _, e := range body.Events {
				k := fmt.Sprintf("%s:%d:%s", e.File, e.Offset, e.Line)
				switch {
				case !f.takes[e.Kind]:
					rej++
				case f.seenKeys[k]:
					dup++
				default:
					f.seenKeys[k] = true
					acc++
				}
			}
			fmt.Fprintf(w, `{"accepted":%d,"duplicates":%d,"rejected":%d}`, acc, dup, rej)
		}
	})
}

func (f *fakeRoom) all() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Event
	for _, b := range f.events {
		out = append(out, b...)
	}
	return out
}

type registry map[string]string

func (r registry) Running() map[string]string {
	out := map[string]string{}
	for k, v := range r {
		out[k] = v
	}
	return out
}

const cid = "0123456789abcdef"

func finding(rule string) string {
	return fmt.Sprintf(`{"event.name":"nofire.vmi.finding","time_unix_nano":1795797693562341903,"severity_text":"WARNING","attributes":{"rule.id":%q,"nofire.finding.grade":"C","nofire.finding.mode":"kill","k8s.pod.name":"brig-ubuntu","other":"x"}}`, rule)
}

func action(a string) string {
	return fmt.Sprintf(`{"event.name":"nofire.vmi.action","time_unix_nano":1795797793944924058,"severity_text":"INFO","attributes":{"event.action":%q,"event.outcome":"success","rule.id":"custom.nofire.kernel_module_load","k8s.pod.name":"brig-ubuntu"}}`, a)
}

func toolCall(id, args string) string {
	b, _ := json.Marshal(map[string]any{"event.name": "nofire.vmi.tool_call", "time_unix_nano": 1, "attributes": map[string]any{
		"gen_ai.tool.call.id": id, "gen_ai.tool.name": "Bash", "gen_ai.tool.call.arguments": args, "gen_ai.provider.name": "anthropic"}})
	return string(b)
}

func toolResult(id, outcome, text string) string {
	b, _ := json.Marshal(map[string]any{"event.name": "nofire.vmi.tool_result", "time_unix_nano": 2, "attributes": map[string]any{
		"gen_ai.tool.call.id": id, "nofire.gen_ai.tool.result.outcome": outcome, "gen_ai.tool.call.result": text}})
	return string(b)
}

func setup(t *testing.T, takes ...string) (*fakeRoom, *Reporter, string) {
	t.Helper()
	f := &fakeRoom{takes: map[string]bool{}, seenKeys: map[string]bool{}}
	for _, k := range takes {
		f.takes[k] = true
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "k3y\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := &Reporter{Client: c, Dir: dir, Registry: registry{cid: "brig-ubuntu"}, Runtime: "hull",
		Sources: []Source{WardenSource(), ToolCallSource()},
		State:   filepath.Join(t.TempDir(), "state.json"), Log: log.New(io.Discard, "", 0)}
	r.Load()
	return f, r, dir
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := io.WriteString(fh, l); err != nil {
			t.Fatal(err)
		}
	}
}

func tick(r *Reporter, n int) {
	for i := 0; i < n; i++ {
		r.Tick(context.Background(), time.Now().Add(time.Duration(i)*time.Second))
	}
}

func TestRegistryGoesBeforeTheEventsThatNameIt(t *testing.T) {
	f, r, dir := setup(t, KindWarden)
	appendLines(t, filepath.Join(dir, cid+".warden.ndjson"), finding("custom.nofire.kernel_module_load")+"\n", action("kill")+"\n")
	tick(r, 2)
	if len(f.calls) < 2 || f.calls[0] != Base+"/agent/sandboxes" {
		t.Fatalf("calls = %v, want the registry first", f.calls)
	}
	if got := f.sandboxes[0]; len(got) != 1 || got[0] != (Sandbox{Sandbox: "brig-ubuntu", State: "running", Runtime: "hull"}) {
		t.Fatalf("R20 = %+v", got)
	}
	all := f.all()
	if len(all) != 2 || all[0].Kind != KindWarden || all[0].Sandbox != "brig-ubuntu" || all[0].Offset != 0 ||
		all[0].Line != finding("custom.nofire.kernel_module_load") || all[1].Parsed["event.action"] != "kill" {
		t.Fatalf("events = %+v", all)
	}
	if all[0].Parsed["other"] != nil || all[0].Parsed["container_id"] != cid {
		t.Fatalf("parsed = %v", all[0].Parsed)
	}
}

func TestARefusedKindStopsAloneAndKeepsItsPlace(t *testing.T) {
	f, r, dir := setup(t, KindToolCall) // takes tool calls, not warden findings
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, finding(fmt.Sprintf("r%d", i))+"\n")
	}
	appendLines(t, filepath.Join(dir, cid+".warden.ndjson"), lines...)
	tick(r, 1) // the capture is found on the first scan and starts at its end
	appendLines(t, filepath.Join(dir, cid+".ndjson"), toolCall("t1", `{"command":"id"}`)+"\n", toolResult("t1", "ok", "uid=0")+"\n")
	tick(r, 3)
	warden, tools := 0, 0
	for _, e := range f.all() {
		switch e.Kind {
		case KindWarden:
			warden++
		case KindToolCall:
			tools++
		}
	}
	if warden != 1 || tools != 2 {
		t.Fatalf("sent %d warden and %d tool events, want 1 (the probe) and 2", warden, tools)
	}
	if s := r.Stats(); len(s.Refused) != 1 || s.Refused[0] != KindWarden {
		t.Fatalf("refused = %v", s.Refused)
	}
	if r.offsets[cid+".warden.ndjson"] != 0 {
		t.Fatalf("warden offset moved to %d past records not kept", r.offsets[cid+".warden.ndjson"])
	}
}

func TestToolCallsAreRedactedCappedAndWrittenAgain(t *testing.T) {
	f, r, dir := setup(t, KindToolCall)
	tick(r, 1)
	secret := `{"command":"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123' https://x"}`
	long := strings.Repeat("a", 10<<10)
	capture := filepath.Join(dir, cid+".ndjson")
	appendLines(t, capture,
		`{"event.name":"nofire.vmi.http.response","attributes":{"gen_ai.output.messages":"tool_call"}}`+"\n",
		toolCall("t1", secret)+"\n", toolResult("t1", "error", long)+"\n")
	tick(r, 1)
	all := f.all()
	if len(all) != 2 {
		t.Fatalf("sent %d events, want the call and the result: %+v", len(all), all)
	}
	var call, res map[string]any
	_ = json.Unmarshal([]byte(all[0].Line), &call)
	_ = json.Unmarshal([]byte(all[1].Line), &res)
	args := call["attributes"].(map[string]any)["gen_ai.tool.call.arguments"].(string)
	if strings.Contains(args, "abcdefghijklmnop") || !strings.Contains(args, "Bearer [REDACTED]") {
		t.Fatalf("arguments not redacted: %s", args)
	}
	attrs := res["attributes"].(map[string]any)
	if n := len(attrs["gen_ai.tool.call.result"].(string)); n != maxFieldBytes || attrs["nofire.gen_ai.tool.result.truncated"] != true {
		t.Fatalf("result %d bytes, truncated %v", n, attrs["nofire.gen_ai.tool.result.truncated"])
	}
	if all[1].Parsed["nofire.gen_ai.tool.result.outcome"] != "error" || all[0].Parsed["gen_ai.tool.name"] != "Bash" {
		t.Fatalf("parsed = %v / %v", all[0].Parsed, all[1].Parsed)
	}
	if all[0].File != cid+".ndjson" || all[0].Sandbox != "brig-ubuntu" {
		t.Fatalf("event = %+v", all[0])
	}
}

func TestACaptureFoundAtStartIsReadFromItsEnd(t *testing.T) {
	f, r, dir := setup(t, KindToolCall)
	appendLines(t, filepath.Join(dir, cid+".ndjson"), toolCall("old", "{}")+"\n")
	const other = "fedcba9876543210"
	r.Registry.(registry)[other] = "brig-other"
	tick(r, 1)
	appendLines(t, filepath.Join(dir, other+".ndjson"), toolCall("new-session", "{}")+"\n")
	appendLines(t, filepath.Join(dir, cid+".ndjson"), toolCall("later", "{}")+"\n")
	tick(r, 1)
	var ids []any
	for _, e := range f.all() {
		ids = append(ids, e.Parsed["gen_ai.tool.call.id"])
	}
	if fmt.Sprint(ids) != "[new-session later]" && fmt.Sprint(ids) != "[later new-session]" {
		t.Fatalf("sent %v, want the later call and the new session's, not the old one", ids)
	}
}

func TestALargeCaptureCatchesUpOverScans(t *testing.T) {
	f, r, dir := setup(t, KindToolCall)
	r.Sources = []Source{ToolCallSource()}
	r.Sources[0].MaxScan = 4 << 10
	tick(r, 1)
	filler := `{"event.name":"nofire.vmi.syscall.enter","attributes":{"pad":"` + strings.Repeat("p", 900) + `"}}` + "\n"
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, filler)
	}
	lines = append(lines, toolCall("end", "{}")+"\n")
	appendLines(t, filepath.Join(dir, cid+".ndjson"), lines...)
	tick(r, 1)
	if n := len(f.all()); n != 0 {
		t.Fatalf("one scan read past its budget: %d events", n)
	}
	tick(r, 6)
	if all := f.all(); len(all) != 1 || all[0].Parsed["gen_ai.tool.call.id"] != "end" {
		t.Fatalf("events = %+v", all)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	f, r, dir := setup(t, KindWarden)
	appendLines(t, filepath.Join(dir, cid+".warden.ndjson"), finding("a")+"\n")
	tick(r, 2)
	r2 := &Reporter{Client: r.Client, Dir: dir, Registry: registry{}, Runtime: "hull",
		Sources: r.Sources, State: r.State, Log: r.Log}
	r2.Load()
	appendLines(t, filepath.Join(dir, cid+".warden.ndjson"), finding("b")+"\n")
	tick(r2, 2)
	var rules []any
	for _, e := range f.all() {
		rules = append(rules, e.Parsed["rule.id"])
	}
	if fmt.Sprint(rules) != "[a b]" {
		t.Fatalf("rules = %v, want no resend after the restart", rules)
	}
	last := f.sandboxes[len(f.sandboxes)-1]
	if last[0].State != "stopped" {
		t.Fatalf("a sandbox gone while the reporter was down was not reported stopped: %+v", f.sandboxes)
	}
	if e := f.all()[1]; e.Sandbox != "brig-ubuntu" {
		t.Fatalf("a stopped container's record lost its sandbox: %+v", e)
	}
}

func TestFailuresBackOffAndA429WaitsAsAsked(t *testing.T) {
	f, r, _ := setup(t)
	f.status = 503
	now := time.Now()
	r.Tick(context.Background(), now)
	if r.backoff != minBackoff {
		t.Fatalf("backoff = %s", r.backoff)
	}
	for i := 0; i < 8; i++ {
		now = now.Add(r.backoff)
		r.Tick(context.Background(), now)
	}
	if r.backoff != maxBackoff {
		t.Fatalf("backoff = %s, want the cap", r.backoff)
	}
	f.status, f.retry = 429, "17"
	now = now.Add(r.backoff)
	r.Tick(context.Background(), now)
	if r.backoff != 17*time.Second {
		t.Fatalf("after a 429 with Retry-After backoff = %s", r.backoff)
	}
	// Without Retry-After, a 429 backs off like any failure.
	f.retry = ""
	now = now.Add(r.backoff)
	r.Tick(context.Background(), now)
	if r.backoff != 34*time.Second {
		t.Fatalf("after a 429 without Retry-After backoff = %s", r.backoff)
	}
	f.status = 401
	r.Tick(context.Background(), now.Add(r.backoff))
	if r.backoff != authBackoff {
		t.Fatalf("after a 401 backoff = %s", r.backoff)
	}
}

func TestRetryAfterIsReadInSecondsAndAsADate(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"": 0, "5": 5 * time.Second, "-1": 0, "soon": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", v, got, want)
		}
	}
}

// Only a kind's probe decides that a deployment does not take it.
func TestRejectionsAfterTheProbeAreCountedNotRefused(t *testing.T) {
	f, r, dir := setup(t, KindWarden)
	path := filepath.Join(dir, cid+".warden.ndjson")
	appendLines(t, path, finding("r0")+"\n")
	tick(r, 1)
	f.mu.Lock()
	f.takes[KindWarden] = false
	f.mu.Unlock()
	appendLines(t, path, finding("r1")+"\n", finding("r2")+"\n")
	tick(r, 2)
	s := r.Stats()
	if len(s.Refused) != 0 || s.Rejected != 2 {
		t.Fatalf("refused = %v, rejected = %d; want none and 2", s.Refused, s.Rejected)
	}
	fi, _ := os.Stat(path)
	if r.offsets[cid+".warden.ndjson"] != fi.Size() {
		t.Fatalf("offset = %d, want past the rejected lines", r.offsets[cid+".warden.ndjson"])
	}
}

func TestARefusedKindIsProbedAgainLater(t *testing.T) {
	f, r, dir := setup(t)
	appendLines(t, filepath.Join(dir, cid+".warden.ndjson"), finding("r0")+"\n", finding("r1")+"\n")
	now := time.Now()
	r.Tick(context.Background(), now)
	if s := r.Stats(); len(s.Refused) != 1 {
		t.Fatalf("refused = %v", s.Refused)
	}
	f.mu.Lock()
	f.takes[KindWarden] = true
	f.mu.Unlock()
	r.Tick(context.Background(), now.Add(time.Minute))
	if n := len(f.all()); n != 1 {
		t.Fatalf("sent %d events before the re-probe, want the first probe only", n)
	}
	r.Tick(context.Background(), now.Add(reprobeAfter))
	r.Tick(context.Background(), now.Add(reprobeAfter+time.Second))
	if s := r.Stats(); len(s.Refused) != 0 || s.Accepted != 2 {
		t.Fatalf("after the re-probe refused = %v, accepted = %d", s.Refused, s.Accepted)
	}
}

func TestEventsOfAnUnnamedSandboxAreSkipped(t *testing.T) {
	f, r, dir := setup(t, KindWarden)
	const other = "fedcba9876543210"
	noPod := `{"event.name":"nofire.vmi.finding","attributes":{"rule.id":"x"}}`
	appendLines(t, filepath.Join(dir, other+".warden.ndjson"), noPod+"\n")
	tick(r, 1)
	if n := len(f.all()); n != 0 || r.Stats().Unnamed != 1 {
		t.Fatalf("sent %d, unnamed %d; want 0 and 1", n, r.Stats().Unnamed)
	}
}

func TestOffsetsOfRemovedFilesAreDropped(t *testing.T) {
	_, r, dir := setup(t, KindWarden)
	path := filepath.Join(dir, cid+".warden.ndjson")
	appendLines(t, path, finding("r0")+"\n")
	tick(r, 1)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tick(r, 1)
	if _, ok := r.offsets[cid+".warden.ndjson"]; ok {
		t.Fatal("kept the offset of a removed file")
	}
}

func TestACallersClientRefusesRedirects(t *testing.T) {
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the key followed a redirect: %s", r.Header.Get("X-API-Key"))
	}))
	defer away.Close()
	srv := httptest.NewServer(http.RedirectHandler(away.URL, http.StatusTemporaryRedirect))
	defer srv.Close()
	hc := &http.Client{Timeout: time.Second}
	c, err := NewClient(srv.URL, "k3y", hc)
	if err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	if err := c.ReportSandboxes(context.Background(), nil); !errors.As(err, &se) || se.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want the redirect as a status", err)
	}
	if hc.CheckRedirect != nil {
		t.Fatal("changed the caller's client")
	}
}

func TestNamesAndClientsTheRoomRefuses(t *testing.T) {
	if _, err := NewClient("my.nofire.ai", "k", nil); err == nil {
		t.Error("took a URL without a scheme")
	}
	if _, err := NewClient("https://my.nofire.ai", " \n", nil); err == nil {
		t.Error("took an empty key")
	}
	for _, n := range []string{"brig-ubuntu", "sbx-14b284a2af3300ae", "a.b_c"} {
		if !ValidName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "Brig", "-x", "a/b", strings.Repeat("a", 129)} {
		if ValidName(n) {
			t.Errorf("%q taken", n)
		}
	}
}
