package mcpproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
)

// MaxRequest bounds the body of a guest request. The proxy holds the whole
// message to judge it, so without a bound a guest could make the broker
// buffer without limit.
const MaxRequest = 1 << 20

// Outcomes recorded on the audit line that Decide does not produce.
const (
	// RefuseServerRequest is a server-to-client request dropped from a
	// response.
	RefuseServerRequest Verdict = "refused:server-request"
	// RefuseEncoding is a response with a Content-Encoding the filter
	// cannot read.
	RefuseEncoding Verdict = "refused:encoding"
	// VerdictError is a request that failed: no credential, an upstream
	// that could not be reached, or a response the filter could not judge.
	VerdictError Verdict = "error"
)

// maxRecordField bounds Method and Tool on a Record. Both are strings the
// guest chose, and an audit line must not grow with them.
const maxRecordField = 64

// Record is the audit line of one exchange. It never carries arguments,
// results, header values or the credential. Method and Tool come from the
// guest and are not quoted here; whoever prints the line does that.
type Record struct {
	Sandbox, Server, Method, Tool string
	Verdict                       Verdict
	Status                        int // HTTP status the guest received
	Duration                      time.Duration
}

// Proxy forwards the MCP requests of a guest to the upstream server, with
// the credential injected on the host.
type Proxy struct {
	// Credentials resolves the secret a server names.
	Credentials secret.Broker
	// Transport reaches the upstream. Nil means DefaultTransport.
	Transport http.RoundTripper
	// Audit, if set, receives one Record per exchange, and one per server
	// request dropped from a response.
	Audit func(Record)
	// Log, if set, receives the error of an upstream that could not be
	// reached. The guest is told only that it is unreachable: the error can
	// name host addresses and resolvers.
	Log func(server string, err error)

	once      sync.Once
	transport http.RoundTripper
}

// Target is the server a request is for, and the sandbox that sent it.
type Target struct {
	Sandbox secret.Workload
	Server  v1alpha1.MCPServer // expanded: a preset already applied
}

// forwardedRequestHeaders are the guest headers the upstream sees. Anything
// else, the guest's Authorization first, is dropped: a header the policy did
// not name could carry what the guest should not send, or steer the upstream.
var forwardedRequestHeaders = []string{"Content-Type", "Accept", "Mcp-Session-Id", "MCP-Protocol-Version", "Last-Event-ID"}

// returnedResponseHeaders are the upstream headers the guest sees. Set-Cookie
// would give the guest upstream state the broker does not track, and
// WWW-Authenticate would send the agent into an OAuth flow for a credential
// the broker holds.
var returnedResponseHeaders = []string{"Content-Type", "Mcp-Session-Id", "MCP-Protocol-Version"}

// DefaultTransport returns a new clone of http.DefaultTransport that requires
// TLS 1.2, uses no proxy and never decompresses. A caller that sets more on
// it, a timeout, changes its own copy alone.
func DefaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The credential goes to the upstream only, as on a route, not through
	// a proxy the host environment names.
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	// The filter reads plain bytes; Accept-Encoding: identity is set on
	// each request.
	t.DisableCompression = true
	return t
}

func (p *Proxy) roundTripper() http.RoundTripper {
	p.once.Do(func() {
		if p.Transport != nil {
			p.transport = p.Transport
			return
		}
		p.transport = DefaultTransport()
	})
	return p.transport
}

// exchange is the state of one Serve call.
type exchange struct {
	p     *Proxy
	w     http.ResponseWriter
	t     Target
	start time.Time
	rec   Record
}

// Serve answers one guest request for t. The caller has already checked
// where the request came from and the guest's token; Serve drops the guest's
// Authorization and sends the credential t names instead.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, t Target) {
	x := &exchange{p: p, w: w, t: t, start: time.Now(),
		rec: Record{Sandbox: t.Sandbox.Sandbox, Server: t.Server.Name}}
	// Deferred, so that a stream aborted by panic is still recorded.
	defer func() { x.emit(x.rec) }()

	switch r.Method {
	case http.MethodPost:
		x.post(r)
	case http.MethodDelete:
		// Ends the MCP session upstream. There is no message to judge.
		x.rec.Verdict = Forward
		x.forward(r, http.MethodDelete, nil, "")
	default:
		// A GET opens a stream on which the server sends requests of its
		// own, which the guest cannot answer for the policy.
		w.Header().Set("Allow", "POST, DELETE")
		x.fail(RefuseMethod, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (x *exchange) post(r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequest+1))
	if err != nil {
		x.fail(VerdictError, http.StatusBadRequest, "could not read the request")
		return
	}
	if len(body) > MaxRequest {
		x.fail(RefuseParse, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	d := Decide(body, x.t.Server.Tools.Allows)
	x.rec.Verdict, x.rec.Method, x.rec.Tool = d.Verdict, clip(d.Method), clip(d.Tool)
	if d.Verdict != Forward {
		x.refuse(d)
		return
	}
	x.forward(r, http.MethodPost, d.Body, d.Method)
}

// refuse answers a refused message without asking the upstream.
func (x *exchange) refuse(d Decision) {
	// A notification takes no response. A JSON-RPC body with a null id
	// would answer a request the guest never sent.
	notification := len(d.ID) == 0 && d.Verdict != RefuseParse && d.Verdict != RefuseBatch
	if notification {
		x.rec.Status = http.StatusAccepted
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	x.rec.Status = http.StatusOK
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(http.StatusOK)
	x.w.Write(ErrorBody(d))
}

// forward sends the request upstream and relays the filtered answer.
func (x *exchange) forward(r *http.Request, method string, body []byte, rpcMethod string) {
	srv := x.t.Server
	value, err := x.credential(r)
	if err != nil {
		x.fail(VerdictError, http.StatusBadGateway, err.Error())
		return
	}
	up, err := http.NewRequestWithContext(r.Context(), method, srv.URL, bytes.NewReader(body))
	if err != nil {
		x.fail(VerdictError, http.StatusBadGateway, fmt.Sprintf("mcp server %q has an unusable url", srv.Name))
		return
	}
	for _, name := range forwardedRequestHeaders {
		if vs := r.Header.Values(name); len(vs) > 0 {
			up.Header[http.CanonicalHeaderKey(name)] = vs
		}
	}
	if err := injectHeaders(srv.Inject, up.Header, value); err != nil {
		x.fail(VerdictError, http.StatusBadGateway, fmt.Sprintf("mcp server %q: %v", srv.Name, err))
		return
	}
	// Last, so that no set header can ask for an encoding the filter cannot
	// read.
	up.Header.Set("Accept-Encoding", "identity")

	resp, err := x.p.roundTripper().RoundTrip(up)
	if err != nil {
		if x.p.Log != nil {
			x.p.Log(srv.Name, err)
		}
		x.fail(VerdictError, http.StatusBadGateway, fmt.Sprintf("mcp server %q is unreachable", srv.Name))
		return
	}
	defer resp.Body.Close()
	x.relay(resp, rpcMethod, value)
}

// credential resolves the secret the server names, or returns nil when it
// names none. The error names the secret, never its value.
func (x *exchange) credential(r *http.Request) ([]byte, error) {
	c := x.t.Server.Credential
	if c == nil {
		return nil, nil
	}
	name := c.SecretRef.Name
	// The field of a document-shaped secret is not read here yet. Sending
	// the whole document instead would hand the upstream every other token
	// in it.
	if c.Field != "" {
		return nil, fmt.Errorf("credential.field is not supported on mcp server %q yet", x.t.Server.Name)
	}
	if x.p.Credentials == nil {
		return nil, fmt.Errorf("secret %q cannot be read: no credential store", name)
	}
	v, err := x.p.Credentials.Resolve(r.Context(), name, x.t.Sandbox)
	if errors.Is(err, secret.ErrNotFound) {
		return nil, fmt.Errorf("secret %q not found; save it with brig secret set %s", name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("secret %q cannot be read", name)
	}
	// A secret saved from a file usually ends in one newline.
	if s, ok := bytes.CutSuffix(v, []byte("\n")); ok {
		v = bytes.TrimSuffix(s, []byte("\r"))
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("secret %q is empty", name)
	}
	// A line break would end the header and start one of the secret's
	// choosing; any other control byte breaks the header.
	for _, c := range v {
		if c < ' ' || c == 0x7f {
			return nil, fmt.Errorf("secret %q is not a header value", name)
		}
	}
	return v, nil
}

// injectHeaders applies inject.headers to h: Remove, then Allow, then Set
// with value in place of {value}, as on a route.
func injectHeaders(in *v1alpha1.Inject, h http.Header, value []byte) error {
	if in == nil || in.Headers == nil {
		return nil
	}
	hi := in.Headers
	for _, name := range hi.Remove {
		h.Del(name)
	}
	for _, a := range hi.Allow {
		var keep []string
		for _, v := range h.Values(a.Name) {
			for _, item := range strings.Split(v, ",") {
				if item = strings.TrimSpace(item); item != "" && slices.Contains(a.Values, item) {
					keep = append(keep, item)
				}
			}
		}
		h.Del(a.Name)
		if len(keep) > 0 {
			h.Set(a.Name, strings.Join(keep, ","))
		}
	}
	for _, s := range hi.Set {
		v := s.Value
		if strings.Contains(v, v1alpha1.PlaceholderValue) {
			if value == nil {
				return fmt.Errorf("header %s needs a credential and the server names none", s.Name)
			}
			v = strings.Replace(v, v1alpha1.PlaceholderValue, string(value), 1)
		}
		h.Set(s.Name, v)
	}
	return nil
}

// relay writes the upstream response to the guest: the allowed headers, and
// the body filtered and with the credential redacted.
func (x *exchange) relay(resp *http.Response, rpcMethod string, value []byte) {
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		x.fail(RefuseEncoding, http.StatusBadGateway, "the mcp server answered with an encoded body")
		return
	}
	h := x.w.Header()
	for _, name := range returnedResponseHeaders {
		if vs := resp.Header.Values(name); len(vs) > 0 {
			h[http.CanonicalHeaderKey(name)] = vs
		}
	}
	if value != nil {
		redactHeader(h, value)
	}

	// An empty body, as for an accepted notification or an ended session,
	// has nothing to judge.
	br := bufio.NewReader(resp.Body)
	if _, err := br.Peek(1); err != nil {
		h.Del("Content-Type")
		x.writeHeader(resp.StatusCode)
		return
	}
	src := struct {
		io.Reader
		io.Closer
	}{br, resp.Body}
	onDrop := func(d Dropped) {
		r := x.rec
		r.Verdict, r.Method, r.Tool, r.Status = RefuseServerRequest, clip(d.Method), "", resp.StatusCode
		x.emit(r)
	}
	filtered, err := FilterResponse(resp.Header.Get("Content-Type"), src, rpcMethod, x.t.Server.Tools.Allows, onDrop)
	if err != nil {
		if resp.StatusCode >= 300 {
			// An error status carries its meaning without the body, which
			// the filter could not judge.
			h.Del("Content-Type")
			x.writeHeader(resp.StatusCode)
			return
		}
		h.Del("Content-Type")
		h.Del("Mcp-Session-Id")
		h.Del("MCP-Protocol-Version")
		x.fail(VerdictError, http.StatusBadGateway, "the mcp server's response could not be judged")
		return
	}
	defer filtered.Close()
	var out io.Reader = filtered
	if value != nil {
		out = redact(filtered, value)
	}
	x.stream(resp.StatusCode, out)
}

// stream copies body to the guest, flushing as each read arrives, since an
// agent waits on progress events. A read that fails before anything is
// written is a 502. One that fails after the headers are out aborts the
// response, so that the guest sees a broken stream and not a clean end.
func (x *exchange) stream(status int, body io.Reader) {
	buf := make([]byte, 32<<10)
	flusher, _ := x.w.(http.Flusher)
	wrote := false
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if !wrote {
				x.writeHeader(status)
				wrote = true
			}
			if _, werr := x.w.Write(buf[:n]); werr != nil {
				return // the guest went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			if !wrote {
				x.writeHeader(status)
			}
			return
		}
		if err != nil {
			if !wrote {
				h := x.w.Header()
				h.Del("Content-Type")
				h.Del("Mcp-Session-Id")
				h.Del("MCP-Protocol-Version")
				x.fail(VerdictError, http.StatusBadGateway, "the mcp server's response could not be judged")
				return
			}
			x.rec.Verdict = VerdictError
			// The deferred record in Serve runs as this unwinds, and
			// net/http drops the connection without logging.
			panic(http.ErrAbortHandler)
		}
	}
}

func (x *exchange) writeHeader(status int) {
	x.rec.Status = status
	x.w.WriteHeader(status)
}

// fail answers with a plain-text error and records verdict.
func (x *exchange) fail(verdict Verdict, status int, msg string) {
	x.rec.Verdict = verdict
	x.rec.Status = status
	http.Error(x.w, msg, status)
}

// emit sends one audit line.
func (x *exchange) emit(r Record) {
	if x.p.Audit == nil {
		return
	}
	r.Duration = time.Since(x.start)
	x.p.Audit(r)
}

// clip cuts s to maxRecordField bytes, at a rune boundary.
func clip(s string) string {
	if len(s) <= maxRecordField {
		return s
	}
	i := maxRecordField
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
