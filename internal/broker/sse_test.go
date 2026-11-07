package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// streamFixture is a Messages API stream whose answer is a text block and a
// call to Bash, as api.anthropic.com sends it.
func streamFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/messages-stream-tool-use.sse")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewriteStream runs a whole stream through an sseRewriter and returns what
// the guest would get and the tools refused, in order.
func rewriteStream(t *testing.T, in []byte, rules *v1alpha1.ToolUseRules) ([]byte, []string) {
	t.Helper()
	var refused []string
	rw := newSSERewriter(io.NopCloser(bytes.NewReader(in)), rules, func(tool string) { refused = append(refused, tool) })
	out, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("the rewriter failed: %v", err)
	}
	return out, refused
}

// splitEvents splits a stream into its events, each with its blank line.
func splitEvents(b []byte) []string {
	var out []string
	for _, ev := range strings.SplitAfter(string(b), "\n\n") {
		if ev != "" {
			out = append(out, ev)
		}
	}
	return out
}

// sseSeen is what a test reads of one event.
type sseSeen struct {
	event, typ string
	index      int
	blockType  string
	deltaType  string
	text       string
	stop       string
}

func readEvent(t *testing.T, raw string) sseSeen {
	t.Helper()
	ev := parseEvent([]byte(raw))
	var d struct {
		Type         string `json:"type"`
		Index        *int   `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(ev.data, &d); err != nil {
		t.Fatalf("event %q holds no JSON: %v", raw, err)
	}
	s := sseSeen{event: ev.name, typ: d.Type, index: -1, blockType: d.ContentBlock.Type,
		deltaType: d.Delta.Type, text: d.ContentBlock.Text + d.Delta.Text, stop: d.Delta.StopReason}
	if d.Index != nil {
		s.index = *d.Index
	}
	if s.event != s.typ {
		t.Errorf("event %q carries data of type %q", s.event, s.typ)
	}
	return s
}

func readEvents(t *testing.T, b []byte) []sseSeen {
	t.Helper()
	var out []sseSeen
	for _, raw := range splitEvents(b) {
		out = append(out, readEvent(t, raw))
	}
	return out
}

func bashRules(action v1alpha1.ToolUseAction) *v1alpha1.ToolUseRules {
	return &v1alpha1.ToolUseRules{Deny: []string{"Bash"}, Action: action}
}

// wantFixtureRewritten checks the fixture after the Bash call was refused or
// dropped.
func wantFixtureRewritten(t *testing.T, fixture, out []byte, action v1alpha1.ToolUseAction) {
	t.Helper()
	got := readEvents(t, out)
	want := []sseSeen{
		{event: "message_start", typ: "message_start", index: -1},
		{event: "content_block_start", typ: "content_block_start", index: 0, blockType: "text"},
		{event: "content_block_delta", typ: "content_block_delta", index: 0, deltaType: "text_delta", text: "I will run "},
		{event: "content_block_delta", typ: "content_block_delta", index: 0, deltaType: "text_delta", text: "the command."},
		{event: "content_block_stop", typ: "content_block_stop", index: 0},
	}
	if action != v1alpha1.ToolUseDrop {
		want = append(want,
			sseSeen{event: "content_block_start", typ: "content_block_start", index: 1, blockType: "text"},
			sseSeen{event: "content_block_delta", typ: "content_block_delta", index: 1, deltaType: "text_delta", text: refusalText("Bash")},
			sseSeen{event: "content_block_stop", typ: "content_block_stop", index: 1},
		)
	}
	want = append(want,
		sseSeen{event: "message_delta", typ: "message_delta", index: -1, stop: "end_turn"},
		sseSeen{event: "message_stop", typ: "message_stop", index: -1},
	)
	if !slices.Equal(got, want) {
		t.Errorf("events\n%+v\nwant\n%+v\nstream:\n%s", got, want, out)
	}
	for _, leak := range []string{"input_json_delta", "toolu_01", "rm -rf", "tool_use"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Errorf("the stream still holds %q:\n%s", leak, out)
		}
	}
	// The text block and the start of the message pass with the bytes the
	// upstream sent.
	in, kept := splitEvents(fixture), splitEvents(out)
	if len(kept) < 5 || !slices.Equal(kept[:5], in[:5]) {
		t.Errorf("the first five events changed:\n%q\nwant\n%q", kept[:min(5, len(kept))], in[:5])
	}
	if kept[len(kept)-1] != in[len(in)-1] {
		t.Errorf("message_stop changed: %q", kept[len(kept)-1])
	}
}

func TestSSERewriterRefusesADeniedToolUse(t *testing.T) {
	fixture := streamFixture(t)
	out, refused := rewriteStream(t, fixture, bashRules(v1alpha1.ToolUseRefuse))
	wantFixtureRewritten(t, fixture, out, v1alpha1.ToolUseRefuse)
	if !slices.Equal(refused, []string{"Bash"}) {
		t.Errorf("refused %q, want [Bash]", refused)
	}
	// An empty action is refuse.
	out2, _ := rewriteStream(t, fixture, bashRules(""))
	if !bytes.Equal(out2, out) {
		t.Errorf("an empty action rewrote the stream otherwise:\n%s", out2)
	}
}

func TestSSERewriterDropsADeniedToolUse(t *testing.T) {
	fixture := streamFixture(t)
	out, refused := rewriteStream(t, fixture, bashRules(v1alpha1.ToolUseDrop))
	wantFixtureRewritten(t, fixture, out, v1alpha1.ToolUseDrop)
	for _, e := range readEvents(t, out) {
		if e.index == 1 {
			t.Errorf("an event of the dropped block is left: %+v", e)
		}
	}
	if !slices.Equal(refused, []string{"Bash"}) {
		t.Errorf("refused %q, want [Bash]", refused)
	}
}

func TestSSERewriterPassesAnUndeniedStreamByteForByte(t *testing.T) {
	fixture := streamFixture(t)
	rules := &v1alpha1.ToolUseRules{Deny: []string{"Write"}}
	for _, tc := range []struct{ name, prefix string }{
		{"the fixture", ""},
		{"a ping and a comment", "event: ping\ndata: {\"type\": \"ping\"}\n\n: keep-alive\n\n"},
		{"CRLF line ends", "event: ping\r\ndata: {\"type\":\"ping\"}\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]byte(tc.prefix), fixture...)
			out, refused := rewriteStream(t, in, rules)
			if !bytes.Equal(out, in) {
				t.Errorf("the stream changed:\n%s\nwant\n%s", out, in)
			}
			if len(refused) != 0 {
				t.Errorf("refused %q", refused)
			}
		})
	}
	t.Run("an end without a blank line", func(t *testing.T) {
		in := append(bytes.Clone(fixture), "event: content_block_start\ndata: {\"type\":\"content_block_start\""...)
		out, _ := rewriteStream(t, in, rules)
		if !bytes.Equal(out, in) {
			t.Errorf("the stream changed:\n%s", out)
		}
	})
}

// twoCalls is a stream whose answer calls Bash at index 1 and Read at index 2.
const twoCalls = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Two calls."}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_02","name":"Read","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/etc/hosts\"}"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":2}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":40}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func TestSSERewriterRenumbersAfterADrop(t *testing.T) {
	out, refused := rewriteStream(t, []byte(twoCalls), bashRules(v1alpha1.ToolUseDrop))
	got := readEvents(t, out)
	var read []sseSeen
	for _, e := range got {
		if e.index >= 1 {
			read = append(read, e)
		}
	}
	want := []sseSeen{
		{event: "content_block_start", typ: "content_block_start", index: 1, blockType: "tool_use"},
		{event: "content_block_delta", typ: "content_block_delta", index: 1, deltaType: "input_json_delta"},
		{event: "content_block_stop", typ: "content_block_stop", index: 1},
	}
	if !slices.Equal(read, want) {
		t.Errorf("the events after the dropped block\n%+v\nwant\n%+v\nstream:\n%s", read, want, out)
	}
	if bytes.Contains(out, []byte("toolu_01")) || !bytes.Contains(out, []byte(`"name":"Read"`)) {
		t.Errorf("the Bash call is left or the Read call is gone:\n%s", out)
	}
	if !strings.Contains(string(out), `/etc/hosts`) {
		t.Errorf("the Read call lost its input:\n%s", out)
	}
	if e := got[len(got)-2]; e.typ != "message_delta" || e.stop != "tool_use" {
		t.Errorf("message_delta %+v, want stop_reason tool_use", e)
	}
	if !slices.Equal(refused, []string{"Bash"}) {
		t.Errorf("refused %q, want [Bash]", refused)
	}
}

func TestSSERewriterKeepsAToolCallBesideARefusedOne(t *testing.T) {
	out, refused := rewriteStream(t, []byte(twoCalls), bashRules(v1alpha1.ToolUseRefuse))
	in, kept := splitEvents([]byte(twoCalls)), splitEvents(out)
	got := readEvents(t, out)
	if len(got) != len(in) {
		t.Fatalf("%d events, want %d:\n%s", len(got), len(in), out)
	}
	if e := got[5]; e.index != 1 || e.deltaType != "text_delta" || e.text != refusalText("Bash") {
		t.Errorf("the Bash block is %+v, want the refusal at index 1", e)
	}
	// The Read block, the message_delta and message_stop keep their bytes.
	if !slices.Equal(kept[7:], in[7:]) {
		t.Errorf("the events after the refused block changed:\n%q\nwant\n%q", kept[7:], in[7:])
	}
	if e := got[len(got)-2]; e.stop != "tool_use" {
		t.Errorf("message_delta %+v, want stop_reason tool_use", e)
	}
	if !slices.Equal(refused, []string{"Bash"}) {
		t.Errorf("refused %q, want [Bash]", refused)
	}
}

func TestSSERewriterReturnsOneEventPerRead(t *testing.T) {
	fixture := streamFixture(t)
	// The first half ends inside the content_block_start of the Bash call.
	cut := bytes.Index(fixture, []byte(`"name":"Bash"`))
	pr, pw := io.Pipe()
	second := make(chan struct{})
	go func() {
		_, _ = pw.Write(fixture[:cut])
		<-second
		_, _ = pw.Write(fixture[cut:])
		_ = pw.Close()
	}()
	rw := newSSERewriter(pr, bashRules(v1alpha1.ToolUseRefuse), func(string) {})
	events := make(chan string)
	go func() {
		br := bufio.NewReader(rw)
		var ev strings.Builder
		for {
			line, err := br.ReadString('\n')
			ev.WriteString(line)
			if err != nil {
				close(events)
				return
			}
			if line == "\n" {
				events <- ev.String()
				ev.Reset()
			}
		}
	}()
	want := splitEvents(fixture)
	// The five events whole in the first half arrive before the rest is sent.
	for i := range 5 {
		select {
		case e := <-events:
			if e != want[i] {
				t.Fatalf("event %d %q, want %q", i, e, want[i])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d did not arrive before the upstream sent the rest", i)
		}
	}
	close(second)
	n := 0
	for range events {
		n++
	}
	if n != 5 {
		t.Errorf("%d events after the cut, want 5", n)
	}
}

func TestSSERewriterRefusesAnOversizedEvent(t *testing.T) {
	big := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"" +
		strings.Repeat("a", maxEvent) + "\"}}\n\n"
	for _, tc := range []struct{ name, in string }{
		{"with its blank line", big},
		{"without one", strings.TrimSuffix(big, "\n\n")},
		{"one long line", strings.Repeat("a", maxEvent+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			rw := newSSERewriter(io.NopCloser(bytes.NewReader(in)), bashRules(v1alpha1.ToolUseRefuse), func(string) {})
			out, err := io.ReadAll(rw)
			if !errors.Is(err, errEventTooLarge) {
				t.Errorf("err %v, want errEventTooLarge", err)
			}
			if len(out) != 0 {
				t.Errorf("%d bytes of the event went on", len(out))
			}
		})
	}
}
