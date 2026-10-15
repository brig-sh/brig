package broker

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// redacted replaces a credential in a response body.
const redacted = "[redacted]"

// redactResponse removes the credential from a response before the guest gets
// it. An upstream that reflects request headers, in a header, a cookie or the
// body, would otherwise hand the guest the value the route keeps from it.
//
// It drops every Set-Cookie, and every header whose value holds one of the
// injected strings. It replaces those strings in the body, as it streams, and
// drops the trailers that hold one. It matches the exact strings that went
// upstream. A credential the upstream sends back encoded another way, such as
// URL-escaped, is not found.
//
// The match needs a plain body. The transport undoes the gzip it asked for
// and drops Content-Encoding. A body still encoded is refused with
// errEncoded, which the guest gets as a 502.
func (u *upstreamTrip) redactResponse(resp *http.Response) error {
	if encoded(resp) {
		return errEncoded
	}
	secrets := slices.Clone(u.secrets)
	resp.Header.Del("Set-Cookie")
	for name, values := range resp.Header {
		if slices.ContainsFunc(values, func(v string) bool { return holdsAny(v, secrets) }) {
			delete(resp.Header, name)
		}
	}
	// The length changes with each replacement.
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Body = newRedactor(resp, secrets)
	return nil
}

// errEncoded is a response body in a content coding the broker cannot read.
var errEncoded = errors.New("the response body has a content coding")

// encoded reports whether resp has a body in a coding other than identity.
func encoded(resp *http.Response) bool {
	if resp.Body == nil || resp.Body == http.NoBody {
		return false
	}
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, coding := range strings.Split(v, ",") {
			if c := strings.TrimSpace(coding); c != "" && !strings.EqualFold(c, "identity") {
				return true
			}
		}
	}
	return false
}

func holdsAny(s string, secrets []string) bool {
	return slices.ContainsFunc(secrets, func(x string) bool { return strings.Contains(s, x) })
}

// redactor replaces the secrets in a body as it streams.
//
// A secret can span two reads. The redactor holds back the longest end of
// what it read that is the start of a secret, and sends everything before
// it. A secret is a header value and holds no line break, so the end of an
// event in an event stream is never held, and each event reaches the guest
// when the upstream sends it.
type redactor struct {
	rc      io.ReadCloser
	resp    *http.Response
	secrets [][]byte
	buf     []byte
	// held is the end of the last read that could start a secret, and out
	// is what is ready to send.
	held, out []byte
	err       error
}

func newRedactor(resp *http.Response, secrets []string) *redactor {
	r := &redactor{rc: resp.Body, resp: resp, buf: make([]byte, 32<<10)}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, []byte(s))
		}
	}
	// The longest first, so a secret that holds a shorter one is replaced
	// whole.
	slices.SortFunc(r.secrets, func(a, b []byte) int { return len(b) - len(a) })
	return r
}

func (r *redactor) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.rc.Read(r.buf)
		data := append(r.held, r.buf[:n]...)
		r.held = nil
		for _, s := range r.secrets {
			data = bytes.ReplaceAll(data, s, []byte(redacted))
		}
		if err != nil {
			r.out, r.err = data, err
			break
		}
		keep := r.startOfSecret(data)
		r.out = data[:len(data)-keep]
		r.held = bytes.Clone(data[len(data)-keep:])
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// startOfSecret returns the length of the longest end of data that is a
// proper prefix of a secret.
func (r *redactor) startOfSecret(data []byte) int {
	longest := 0
	for _, s := range r.secrets {
		for k := min(len(s)-1, len(data)); k > longest; k-- {
			if bytes.HasPrefix(s, data[len(data)-k:]) {
				longest = k
				break
			}
		}
	}
	return longest
}

// Close closes the body and drops the trailers that hold a secret.
// ReverseProxy copies the trailers after it closes the body.
func (r *redactor) Close() error {
	err := r.rc.Close()
	var secrets []string
	for _, s := range r.secrets {
		secrets = append(secrets, string(s))
	}
	for name, values := range r.resp.Trailer {
		if slices.ContainsFunc(values, func(v string) bool { return holdsAny(v, secrets) }) {
			delete(r.resp.Trailer, name)
		}
	}
	return err
}
