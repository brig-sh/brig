package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// maxEvent is the largest event of an event stream the rewriter holds. The
// largest event of a Messages stream is a delta of a few kilobytes.
const maxEvent = 1 << 20

// errEventTooLarge ends a stream whose event the rewriter will not hold. The
// rewriter reads an event whole to find a tool call in it, and an event it
// passed on unread could be the start of a denied block: the stream is cut,
// and ReverseProxy aborts the response, rather than let a call through
// unchecked.
var errEventTooLarge = errors.New("an event of the upstream stream is over 1 MiB")

// sseRewriter rewrites the event stream of a Messages API answer, event by
// event, as it arrives. The events of a tool_use block whose tool is denied
// become the three events of a text block that holds the refusal under
// refuse, and go under drop. Every other event passes with the bytes the
// upstream sent, except for an index renumbered after a drop and a
// stop_reason of tool_use when no tool call is left.
//
// Each Read returns the bytes of at most one upstream event, the ones it
// stands for. ReverseProxy flushes after each Read, so each event reaches the
// guest when its blank line arrives, as it would without the rewrite.
type sseRewriter struct {
	rc      io.ReadCloser
	rules   *v1alpha1.ToolUseRules
	refused func(string)
	// pending is what was read from rc and not yet sent: the start of the
	// next event. eof says rc has nothing more.
	pending []byte
	eof     bool
	// readErr is the error that ended rc, io.EOF at a clean end.
	readErr error
	buf     []byte
	// denied holds the upstream indexes of the denied tool_use blocks.
	denied map[int]bool
	// dropped counts the denied blocks dropped so far. A later block's index
	// goes down by it, so the indexes the guest sees have no gap.
	dropped int
	// kept counts the tool_use blocks started and kept.
	kept int
	// touched says a block was refused or dropped.
	touched bool
	out     []byte
	err     error
}

func newSSERewriter(rc io.ReadCloser, rules *v1alpha1.ToolUseRules, refused func(string)) *sseRewriter {
	return &sseRewriter{rc: rc, rules: rules, refused: refused, buf: make([]byte, 32<<10), denied: map[int]bool{}}
}

func (s *sseRewriter) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		ev, whole, err := s.next()
		if err != nil {
			s.err = err
		}
		switch {
		case whole:
			s.out = s.rewrite(ev)
		default:
			// The end of a stream without its blank line is no event: a
			// client drops it. It goes on as it came.
			s.out = ev
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// Close closes the body under the rewriter: the redactor, which drops the
// trailers that hold a secret.
func (s *sseRewriter) Close() error { return s.rc.Close() }

// next returns the raw bytes of one event, up to and with its blank line, and
// whole true. At the end of the stream it returns the bytes left without a
// blank line, whole false, and the error of rc. A line ends at \n, \r\n or a
// lone \r, as the event stream format says.
func (s *sseRewriter) next() (ev []byte, whole bool, err error) {
	pos := 0
	for {
		if end, blank, ok := lineEnd(s.pending, pos, s.eof); ok {
			if blank {
				if end > maxEvent {
					return nil, false, errEventTooLarge
				}
				ev = s.pending[:end:end]
				s.pending = s.pending[end:]
				return ev, true, nil
			}
			pos = end
			continue
		}
		if len(s.pending) > maxEvent {
			return nil, false, errEventTooLarge
		}
		if s.eof {
			ev, s.pending = s.pending, nil
			return ev, false, s.readErr
		}
		// An event handed out ends where s.pending starts, and its capacity
		// ends there too, so this append never writes over it.
		n, rerr := s.rc.Read(s.buf)
		s.pending = append(s.pending, s.buf[:n]...)
		// The events whole before the end are still rewritten first.
		if rerr != nil {
			s.eof, s.readErr = true, rerr
		}
	}
}

// lineEnd finds the end of the line that starts at pos in b. It returns the
// offset after its terminator and whether the line is blank. ok is false
// when b holds no whole line from pos yet. A \r at the end of b waits for the
// next byte, which may be its \n, unless the stream has ended.
func lineEnd(b []byte, pos int, eof bool) (end int, blank, ok bool) {
	i := bytes.IndexAny(b[pos:], "\r\n")
	if i < 0 {
		return 0, false, false
	}
	j := pos + i
	end = j + 1
	if b[j] == '\r' {
		if end == len(b) && !eof {
			return 0, false, false
		}
		if end < len(b) && b[end] == '\n' {
			end++
		}
	}
	return end, i == 0, true
}

// sseEvent is the fields of one event the rewriter reads.
type sseEvent struct {
	name    string
	hasName bool
	data    []byte
}

// parseEvent reads the event and data fields of a raw event. Data lines are
// joined with \n. A comment line, one that starts with a colon, and a field
// the rewriter does not read are left out.
func parseEvent(raw []byte) sseEvent {
	var ev sseEvent
	var data [][]byte
	for pos := 0; pos < len(raw); {
		end, _, ok := lineEnd(raw, pos, true)
		if !ok {
			end = len(raw)
		}
		line := bytes.TrimRight(raw[pos:end], "\r\n")
		pos = end
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		switch string(field) {
		case "event":
			ev.name, ev.hasName = string(value), true
		case "data":
			data = append(data, value)
		}
	}
	ev.data = bytes.Join(data, []byte("\n"))
	return ev
}

// encode writes an event the rewriter changed.
func (e sseEvent) encode(data []byte) []byte {
	var b bytes.Buffer
	if e.hasName {
		b.WriteString("event: " + e.name + "\n")
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// rewrite returns the bytes the guest gets for one raw upstream event.
func (s *sseRewriter) rewrite(raw []byte) []byte {
	ev := parseEvent(raw)
	var data map[string]json.RawMessage
	if json.Unmarshal(ev.data, &data) != nil {
		return raw
	}
	var typ string
	if json.Unmarshal(data["type"], &typ) != nil {
		return raw
	}
	switch typ {
	case "content_block_start", "content_block_delta", "content_block_stop":
		var index int
		if json.Unmarshal(data["index"], &index) != nil {
			return raw
		}
		if typ == "content_block_start" {
			if blockType, name := blockKind(data["content_block"]); blockType == "tool_use" {
				if denied(s.rules, name) {
					return s.refuse(index, name)
				}
				s.kept++
			}
		}
		if s.denied[index] {
			return nil
		}
		if s.dropped == 0 {
			return raw
		}
		data["index"] = json.RawMessage(strconv.Itoa(index - s.dropped))
		return s.reencode(ev, raw, data)
	case "message_delta":
		if !s.touched || s.kept > 0 {
			return raw
		}
		var delta map[string]json.RawMessage
		var stop string
		if json.Unmarshal(data["delta"], &delta) != nil ||
			json.Unmarshal(delta["stop_reason"], &stop) != nil || stop != "tool_use" {
			return raw
		}
		delta["stop_reason"] = json.RawMessage(`"end_turn"`)
		enc, err := marshalRaw(delta)
		if err != nil {
			return raw
		}
		data["delta"] = enc
		return s.reencode(ev, raw, data)
	}
	return raw
}

// refuse records the denied tool_use block at index and returns what stands
// for its start: the three events of a text block that holds the refusal
// under refuse, nothing under drop. The block's delta and stop events go.
func (s *sseRewriter) refuse(index int, name string) []byte {
	s.denied[index] = true
	s.touched = true
	s.refused(name)
	if s.rules.Mode() == v1alpha1.ToolUseDrop {
		s.dropped++
		return nil
	}
	i := index - s.dropped
	start, _ := marshalRaw(map[string]any{
		"type": "content_block_start", "index": i,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	delta, _ := marshalRaw(map[string]any{
		"type": "content_block_delta", "index": i,
		"delta": map[string]string{"type": "text_delta", "text": refusalText(name)},
	})
	stop, _ := marshalRaw(map[string]any{"type": "content_block_stop", "index": i})
	var b bytes.Buffer
	b.Write(sseEvent{name: "content_block_start", hasName: true}.encode(start))
	b.Write(sseEvent{name: "content_block_delta", hasName: true}.encode(delta))
	b.Write(sseEvent{name: "content_block_stop", hasName: true}.encode(stop))
	return b.Bytes()
}

// reencode writes an event with changed data, or the raw event when the data
// does not encode.
func (s *sseRewriter) reencode(ev sseEvent, raw []byte, data map[string]json.RawMessage) []byte {
	enc, err := marshalRaw(data)
	if err != nil {
		return raw
	}
	return ev.encode(enc)
}
