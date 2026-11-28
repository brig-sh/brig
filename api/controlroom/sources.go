package controlroom

import (
	"encoding/json"
	"strings"
)

// The R21 kinds the Reporter sends. A deployment that does not know a kind
// stores its events as ingest.unparsed, so the Reporter probes each kind with
// one event before it sends more (see Reporter).
const (
	// KindWarden is a warden finding or action.
	KindWarden = "warden_finding"
	// KindToolCall is a model's tool call, or the result of one.
	KindToolCall = "model_tool_call"
)

// maxFieldBytes caps a tool call's arguments and a result's text, after
// redaction.
const maxFieldBytes = 4 << 10

// A Source is one kind of file the Reporter tails in the capture directory,
// <container-id><Suffix>, and how a line of it becomes an event.
type Source struct {
	// Kind is the R21 kind of the events.
	Kind string
	// Suffix names the files: the container id followed by it.
	Suffix string
	// Skip lists longer suffixes that also end in Suffix and belong to
	// another source, such as .warden.ndjson for .ndjson.
	Skip []string
	// FromEnd starts a file found on the first scan at its end. A capture
	// directory keeps every old session, and those would all go out at once.
	FromEnd bool
	// MaxScan bounds what one scan reads of one file, so a large capture
	// catches up over several scans without holding the others back.
	MaxScan int64
	// Parse turns one line into the event's line and summary, or reports
	// that the line is not one to send. sandbox is the name the Registry
	// gave the container, or "" when it has none.
	Parse func(line, containerID, sandbox string) (Event, bool)
}

// match returns the container id of a file name this source reads.
func (s Source) match(name string) (string, bool) {
	if !strings.HasSuffix(name, s.Suffix) {
		return "", false
	}
	for _, skip := range s.Skip {
		if strings.HasSuffix(name, skip) {
			return "", false
		}
	}
	id := strings.TrimSuffix(name, s.Suffix)
	return id, id != ""
}

// record is the part of a capture or warden line the sources read.
type record struct {
	Name     string         `json:"event.name"`
	Severity string         `json:"severity_text"`
	Attrs    map[string]any `json:"attributes"`
}

// WardenSource sends the warden's findings and actions, the
// nofire.vmi.finding and nofire.vmi.action records it writes to
// <container-id>.warden.ndjson. A line goes as the warden wrote it.
func WardenSource() Source {
	return Source{Kind: KindWarden, Suffix: ".warden.ndjson", MaxScan: 8 << 20, Parse: parseWarden}
}

func parseWarden(line, id, sandbox string) (Event, bool) {
	var rec record
	if json.Unmarshal([]byte(line), &rec) != nil {
		return Event{}, false
	}
	if rec.Name != "nofire.vmi.finding" && rec.Name != "nofire.vmi.action" {
		return Event{}, false
	}
	if sandbox == "" {
		// The warden names the sandbox in its own record, which covers a
		// container the Registry no longer knows.
		if n, _ := rec.Attrs["k8s.pod.name"].(string); ValidName(n) {
			sandbox = n
		}
	}
	parsed := map[string]any{"record": rec.Name, "container_id": id}
	if rec.Severity != "" {
		parsed["severity"] = rec.Severity
	}
	for k, v := range rec.Attrs {
		switch {
		case k == "rule.id", k == "rule.name", k == "event.action", k == "event.outcome",
			strings.HasPrefix(k, "nofire.finding."), strings.HasPrefix(k, "nofire.action."):
			parsed[k] = v
		}
	}
	return Event{Kind: KindWarden, Sandbox: sandbox, Line: line, Parsed: parsed}, true
}

// ToolCallSource sends the sidecar's nofire.vmi.tool_call and
// nofire.vmi.tool_result records from <container-id>.ndjson. The arguments of
// a call and the text of a result are redacted and capped, so the line sent is
// the record written again, not the line as read. The sidecar emits a result
// once per call, so the Reporter does no dedupe of its own.
func ToolCallSource() Source {
	return Source{
		Kind: KindToolCall, Suffix: ".ndjson", Skip: []string{".warden.ndjson"},
		FromEnd: true, MaxScan: 8 << 20, Parse: parseToolCall,
	}
}

// toolFields are the free-text attributes of a tool-call record, which carry
// what the model sent a tool and what the tool answered.
var toolFields = []struct{ text, truncated string }{
	{"gen_ai.tool.call.arguments", "nofire.gen_ai.tool.call.arguments_truncated"},
	{"gen_ai.tool.call.result", "nofire.gen_ai.tool.result.truncated"},
}

func parseToolCall(line, id, sandbox string) (Event, bool) {
	// A capture is mostly other records: decode only the lines that can be
	// a tool-call record.
	if !strings.Contains(line, `"nofire.vmi.tool_`) {
		return Event{}, false
	}
	var full map[string]any
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if dec.Decode(&full) != nil {
		return Event{}, false
	}
	name, _ := full["event.name"].(string)
	if name != "nofire.vmi.tool_call" && name != "nofire.vmi.tool_result" {
		return Event{}, false
	}
	attrs, _ := full["attributes"].(map[string]any)
	if attrs == nil {
		return Event{}, false
	}
	for _, f := range toolFields {
		s, ok := attrs[f.text].(string)
		if !ok {
			continue
		}
		s, cut := capBytes(Redact(s), maxFieldBytes)
		attrs[f.text] = s
		if cut {
			attrs[f.truncated] = true
		}
	}
	b, err := json.Marshal(full)
	if err != nil {
		return Event{}, false
	}
	parsed := map[string]any{"record": name, "container_id": id}
	for _, k := range []string{"gen_ai.tool.call.id", "gen_ai.tool.name", "gen_ai.provider.name",
		"gen_ai.request.model", "nofire.gen_ai.tool.result.outcome"} {
		if v, ok := attrs[k]; ok {
			parsed[k] = v
		}
	}
	return Event{Kind: KindToolCall, Sandbox: sandbox, Line: string(b), Parsed: parsed}, true
}
