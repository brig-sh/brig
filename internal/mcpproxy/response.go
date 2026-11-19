package mcpproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxEventSize bounds one response message, a JSON body or the data of one SSE
// event. The filter holds a whole message to judge it, so without a bound a
// server could make the broker buffer without limit.
const maxEventSize = 1 << 20

// Dropped is a server request removed from a response stream.
type Dropped struct{ Method string }

// FilterResponse returns a body that yields the filtered response. method is
// the request's JSON-RPC method, which the filter does not depend on: a result
// with a tools member is filtered in every response, and server requests are
// dropped in every response. onDrop is called per dropped
// message. A content type other than application/json or text/event-stream,
// or a message the filter cannot judge, is an error: passing it raw would let
// the guest see what the policy did not allow.
//
// A server request in an application/json body has nothing else to deliver,
// so it is an error there rather than an empty body. A nil allow denies every
// tool. On error, FilterResponse has closed body.
func FilterResponse(contentType string, body io.ReadCloser, method string, allow Allow, onDrop func(Dropped)) (rc io.ReadCloser, err error) {
	defer func() {
		if err != nil {
			body.Close()
		}
	}()
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("unreadable response content type %q", contentType)
	}
	if onDrop == nil {
		onDrop = func(Dropped) {}
	}
	switch mt {
	case "application/json":
		defer body.Close() // the whole body is read here, so it is done either way
		raw, err := io.ReadAll(io.LimitReader(body, maxEventSize+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxEventSize {
			return nil, errors.New("response body too large")
		}
		if !utf8.Valid(raw) {
			return nil, errors.New("response body is not valid UTF-8")
		}
		out, dropped, err := filterMessage(raw, method, allow, onDrop)
		if err != nil {
			return nil, err
		}
		if dropped {
			return nil, errors.New("response is a server request")
		}
		return io.NopCloser(bytes.NewReader(out)), nil
	case "text/event-stream":
		return &sseFilter{src: body, r: bufio.NewReader(body), method: method, allow: allow, onDrop: onDrop}, nil
	}
	return nil, fmt.Errorf("unsupported response content type %q", mt)
}

// filterMessage judges one JSON-RPC message from the server. It returns the
// bytes to deliver, or dropped when the message is a server request.
func filterMessage(raw []byte, _ string, allow Allow, onDrop func(Dropped)) (out []byte, dropped bool, err error) {
	// Strict parsing, so that a spelling the proxy does not read as method or
	// id ("Method", "ID") cannot be one a lenient client reads as a request.
	msg, err := parseObject(bytes.TrimSpace(raw), true)
	if err != nil {
		return nil, false, fmt.Errorf("unreadable response message: %w", err)
	}
	if rawMethod, ok := msg["method"]; ok {
		if _, hasID := msg["id"]; hasID {
			var m string
			if err := json.Unmarshal(rawMethod, &m); err != nil {
				return nil, false, errors.New("server request with a method that is not a string")
			}
			onDrop(Dropped{Method: m})
			return nil, true, nil
		}
	}
	// Any result is searched for tools, whatever the request was: a server
	// that answers a ping or a tools/call with a tools member would otherwise
	// show the guest names the policy does not list. method is kept for the
	// caller's signature; nothing is decided on it.
	rawResult := bytes.TrimSpace(msg["result"])
	if len(rawResult) == 0 || rawResult[0] != '{' {
		return raw, false, nil
	}
	result, err := filterTools(rawResult, allow)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(result, rawResult) {
		return raw, false, nil
	}
	msg["result"] = result
	return canonical(msg), false, nil
}

// filterTools removes from a tools/list result the tools the policy does not
// name, keeping its other members such as nextCursor.
func filterTools(raw json.RawMessage, allow Allow) (json.RawMessage, error) {
	result, err := parseObject(raw, false)
	if err != nil {
		return nil, fmt.Errorf("unreadable tools/list result: %w", err)
	}
	for k := range result {
		if k != "tools" && strings.EqualFold(k, "tools") {
			return nil, errors.New("case-variant tools in tools/list result")
		}
	}
	rawTools, ok := result["tools"]
	if !ok {
		return raw, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil || tools == nil {
		return nil, errors.New("tools/list result has no tools array")
	}
	kept := []json.RawMessage{}
	for _, t := range tools {
		tool, err := parseObject(t, false)
		if err != nil {
			return nil, fmt.Errorf("unreadable tool in tools/list result: %w", err)
		}
		// A tool without a "name" member, or one under another spelling, is
		// dropped: the policy cannot say it names it.
		var name string
		if json.Unmarshal(tool["name"], &name) != nil || name == "" {
			continue
		}
		if allow != nil && allow(name) {
			kept = append(kept, t)
		}
	}
	if result["tools"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

var (
	nel = []byte("\u0085")
	ls  = []byte("\u2028")
	ps  = []byte("\u2029")

	// lineBreakLookalike matches what some line parsers treat as a line break
	// and the SSE spec does not.
	lineBreakLookalike = regexp.MustCompile("[\x0b\x0c\x1c-\x1e\u0085\u2028\u2029]")
)

// sseFilter yields the events of an SSE response one at a time, as each
// completes, so that an agent waiting on progress is not held until the end.
type sseFilter struct {
	src     io.Closer
	r       *bufio.Reader
	method  string
	allow   Allow
	onDrop  func(Dropped)
	started bool   // the stream's leading BOM has been dealt with
	buf     []byte // filtered bytes not yet read
	err     error  // returned once buf is drained
}

func (f *sseFilter) Close() error { return f.src.Close() }

func (f *sseFilter) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		f.buf, f.err = f.next()
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// next reads one event and returns what to deliver for it. At the end of the
// stream it returns io.EOF.
func (f *sseFilter) next() ([]byte, error) {
	var (
		lines    [][]byte // raw lines of the event, terminators included
		isData   []bool   // per line: is it a data field
		data     [][]byte
		dataSize int
		rawSize  int
	)
	if !f.started {
		f.started = true
		// SSE parsers drop a BOM at the start of the stream. Left in, it would
		// make the first field read as "\ufeffdata", an event with no data that
		// passes unjudged, while the client reads it as data.
		if b, err := f.r.Peek(3); err == nil && bytes.Equal(b, []byte("\xef\xbb\xbf")) {
			f.r.Discard(3)
		}
	}
	for {
		line, raw, err := f.readLine()
		if err != nil && err != io.EOF {
			return nil, err
		}
		eof := err == io.EOF
		blank := len(raw) > 0 && len(line) == 0
		if len(raw) > 0 && !blank {
			rawSize += len(raw)
			if rawSize > 2*maxEventSize {
				return nil, errors.New("event too large")
			}
			lines = append(lines, raw)
			// The checks below read valid UTF-8 only. An overlong or truncated
			// encoding of a line break matches none of them, and a lenient
			// decoder downstream could still read it as one.
			if !utf8.Valid(line) {
				return nil, errors.New("event line is not valid UTF-8")
			}
			field, value, _ := bytes.Cut(line, []byte(":"))
			// A field name the proxy reads as unknown could be one a lenient
			// client reads as data, as with a BOM or a Unicode lookalike.
			for _, c := range field {
				if c >= 0x80 {
					return nil, errors.New("event field name is not ASCII")
				}
			}
			value = bytes.TrimPrefix(value, []byte(" "))
			isData = append(isData, string(field) == "data")
			// Other parsers break a line on these, so a field line that holds
			// one could hide a second field or event from this parser and be
			// forwarded raw. A data payload holding one is judged as JSON.
			if string(field) != "data" && lineBreakLookalike.Match(line) {
				return nil, errors.New("event line holds a line-break lookalike")
			}
			if string(field) == "data" {
				dataSize += len(value) + 1
				if dataSize > maxEventSize {
					return nil, errors.New("event too large")
				}
				data = append(data, value)
			}
		}
		if !blank && !eof {
			continue
		}
		if len(lines) == 0 {
			if eof {
				return nil, io.EOF
			}
			continue // a blank line between events
		}
		// An unterminated last event is judged like a terminated one:
		// dropping it would be harmless, but passing it unjudged would not.
		out, derr := f.dispatch(lines, isData, data)
		if derr != nil {
			return nil, derr
		}
		if len(out) > 0 {
			if out[len(out)-1] != '\n' {
				out = append(out, '\n')
			}
			out = append(out, '\n') // the blank line that ends the event
		}
		if eof {
			return out, io.EOF
		}
		return out, nil
	}
}

// dispatch judges one event and returns its bytes without the blank line. An
// event without data carries no message and passes unchanged.
func (f *sseFilter) dispatch(lines [][]byte, isData []bool, data [][]byte) ([]byte, error) {
	var all []byte
	for _, l := range lines {
		all = append(all, l...)
	}
	if len(data) == 0 {
		return all, nil
	}
	payload := bytes.Join(data, []byte("\n"))
	// An empty data field is the priming event of a resumable stream, which
	// carries no message.
	if len(bytes.TrimSpace(payload)) == 0 {
		return all, nil
	}
	out, dropped, err := filterMessage(payload, f.method, f.allow, f.onDrop)
	if err != nil {
		return nil, err
	}
	if dropped {
		return nil, nil
	}
	// U+0085, U+2028 and U+2029 are legal raw in a JSON string but end a line
	// for some parsers, which would read the rest as a field of their own.
	// Marshalling escapes the last two; after it a raw U+0085 can only be in a
	// string.
	if bytes.Contains(out, nel) || bytes.Contains(out, ls) || bytes.Contains(out, ps) {
		m, err := json.Marshal(json.RawMessage(out))
		if err != nil {
			return nil, err
		}
		out = bytes.ReplaceAll(m, nel, []byte(`\u0085`))
	}
	if bytes.Equal(out, payload) {
		return all, nil
	}
	// Rewritten: one data line in place of the first, the other fields kept.
	var res []byte
	wrote := false
	for i, l := range lines {
		if !isData[i] {
			res = append(res, l...)
		} else if !wrote {
			res = append(res, "data: "...)
			res = append(res, out...)
			res = append(res, '\n')
			wrote = true
		}
	}
	return res, nil
}

// readLine reads one SSE line, ended by \n, \r or \r\n. It returns the line
// without its terminator and with it. A line longer than the event bound is an
// error, so a stream without line breaks cannot grow the buffer.
func (f *sseFilter) readLine() (line, raw []byte, err error) {
	for {
		b, err := f.r.ReadByte()
		if err != nil {
			return raw, raw, err
		}
		raw = append(raw, b)
		switch b {
		case '\n':
			return raw[:len(raw)-1], raw, nil
		case '\r':
			if n, perr := f.r.Peek(1); perr == nil && n[0] == '\n' {
				f.r.ReadByte()
				raw = append(raw, '\n')
				return raw[:len(raw)-2], raw, nil
			}
			return raw[:len(raw)-1], raw, nil
		}
		if len(raw) > 2*maxEventSize {
			return nil, nil, errors.New("event line too large")
		}
	}
}
