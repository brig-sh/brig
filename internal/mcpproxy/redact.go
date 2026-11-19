package mcpproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
)

// redacted stands in for the credential in a response. It holds no quote or
// backslash, so a JSON string it lands in stays valid.
const redacted = "[redacted]"

// redactForms returns the spellings of value that a response can carry it
// in: as it is, and escaped in a JSON string, with and without the HTML
// escaping of Go's encoder. An upstream that echoes the credential back, in
// an error message or a debug field, must not hand it to the guest. This is
// no defence against an upstream that encodes it on purpose; the upstream
// already holds the credential.
func redactForms(value []byte) [][]byte {
	forms := [][]byte{value}
	if s, err := json.Marshal(string(value)); err == nil {
		forms = append(forms, s[1:len(s)-1])
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(string(value)) == nil {
		s := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		forms = append(forms, s[1:len(s)-1])
	}
	// Go never escapes "/", but PHP's json_encode and several Java and .NET
	// encoders write it as \/, and base64 and JWT tokens often hold one.
	// FilterResponse keeps a string's escapes, so that spelling would reach
	// the guest as the upstream wrote it.
	for _, f := range forms[1:] {
		if bytes.Contains(f, []byte("/")) {
			forms = append(forms, bytes.ReplaceAll(f, []byte("/"), []byte(`\/`)))
		}
	}
	var out [][]byte
	for _, f := range forms {
		if len(f) > 0 && !slices.ContainsFunc(out, func(o []byte) bool { return bytes.Equal(o, f) }) {
			out = append(out, f)
		}
	}
	// Longest first, so a form that holds another is replaced whole.
	slices.SortFunc(out, func(a, b []byte) int { return len(b) - len(a) })
	return out
}

// redact returns body with every form of value replaced by redacted. A value
// split across two reads is still found: the end of what was read is held
// back while it could be the start of a form.
func redact(body io.ReadCloser, value []byte) io.ReadCloser {
	return &redactor{src: body, forms: redactForms(value), buf: make([]byte, 32<<10)}
}

type redactor struct {
	src     io.ReadCloser
	forms   [][]byte
	buf     []byte
	pending []byte // read, not yet released: may start a form
	out     []byte // redacted, ready to return
	err     error
}

func (r *redactor) Close() error { return r.src.Close() }

func (r *redactor) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			if len(r.pending) == 0 {
				return 0, r.err
			}
			// Nothing follows, so what was held back cannot become a form.
			r.out, r.pending = r.pending, nil
			break
		}
		n, err := r.src.Read(r.buf)
		r.err = err
		if n == 0 {
			continue
		}
		r.pending = append(r.pending, r.buf[:n]...)
		for _, f := range r.forms {
			r.pending = bytes.ReplaceAll(r.pending, f, []byte(redacted))
		}
		// Only a tail that is the start of a form is held. Holding a fixed
		// length instead would hold the blank line that ends an SSE event,
		// and the agent would wait on the event until the next one came.
		keep := r.heldTail()
		r.out = append(r.out, r.pending[:len(r.pending)-keep]...)
		r.pending = append([]byte(nil), r.pending[len(r.pending)-keep:]...)
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// heldTail is the length of the longest tail of pending that is a proper
// prefix of a form.
func (r *redactor) heldTail() int {
	best := 0
	for _, f := range r.forms {
		for k := min(len(f)-1, len(r.pending)); k > best; k-- {
			if bytes.HasPrefix(f, r.pending[len(r.pending)-k:]) {
				best = k
				break
			}
		}
	}
	return best
}

// redactHeader drops every header of h whose value holds a form of value.
func redactHeader(h http.Header, value []byte) {
	forms := redactForms(value)
	for name, vs := range h {
		for _, v := range vs {
			if slices.ContainsFunc(forms, func(f []byte) bool { return bytes.Contains([]byte(v), f) }) {
				h.Del(name)
				break
			}
		}
	}
}
