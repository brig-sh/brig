package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
)

// maxReplay is the largest request body held before the request goes
// upstream: scanned whole for a session token, and kept for the retry after
// an upstream 401. Of a larger body only the start is scanned, and its 401
// goes back to the guest.
const maxReplay = 8 << 20

// ServeHTTP serves /<sandbox-id>/r/<route>/<rest>. The checks run in the order
// of docs/broker.md, and a request that fails one never reaches the upstream.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.now()
	rec := &recorder{ResponseWriter: w}
	// The path goes on the audit line once it splits, as <rest>.
	a := &auditLine{method: r.Method}
	defer func() {
		// A panic is a bug. net/http would recover it and drop the
		// connection, and the line would say 200 for a request that got no
		// answer. It is answered 500 instead when nothing was sent yet, and
		// the line says check=panic. ErrAbortHandler is ReverseProxy giving
		// up on a response it started, and goes on as it is.
		v := recover()
		if v != nil && v != http.ErrAbortHandler {
			log.Printf("brig broker: panic: %v\n%s", v, debug.Stack())
			a.check = "panic"
			if rec.status == 0 {
				http.Error(rec, "brig broker: internal error", http.StatusInternalServerError)
				v = nil
			} else {
				v = http.ErrAbortHandler
			}
		}
		h.writeAudit(r.Context(), a, rec.code(), h.now().Sub(start))
		if v != nil {
			panic(v)
		}
	}()

	// ReverseProxy turns an upgrade into a raw pipe that carries the
	// credential past every later request, and it sets Upgrade again after
	// Rewrite. CONNECT asks for the same pipe. So both are refused before
	// anything else.
	if r.Method == http.MethodConnect || upgrade(r.Header) {
		a.check = "upgrade"
		http.Error(rec, "brig broker: a route does not take a protocol upgrade or CONNECT", http.StatusBadRequest)
		return
	}

	p, ok := splitRoutePath(r.URL)
	if !ok {
		a.check = "path"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}
	// A request is served only for a sandbox that reaches the broker where it
	// arrived. Any other sandbox's path gets the answer of one that does not
	// exist, before a lookup or a token check could tell the two apart.
	arrived, ok := h.arrival(p.sandbox, r.RemoteAddr)
	a.arrived = arrived
	if !ok {
		a.check = "arrival"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}
	a.sandbox, a.route, a.path = p.sandbox, p.route, p.rest
	sb := h.sandbox(p.sandbox)
	if sb == nil {
		a.check = "sandbox"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}
	rt := sb.routes[p.route]
	if rt == nil {
		a.check = "route"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}

	token := sessionToken(rt.spec.Inject, r.Header)
	if token == "" {
		a.check = errTokenMissing.Error()
		http.Error(rec, "brig broker: the session token was refused", http.StatusUnauthorized)
		return
	}
	c, err := verify(sb.key, token, p.sandbox, p.route, h.now())
	if err != nil {
		a.check = err.Error()
		http.Error(rec, "brig broker: the session token was refused", http.StatusUnauthorized)
		return
	}
	wl := sb.workload(c.Session)
	a.workload = workloadID(wl)
	sb.remember(token, c.Route, time.Unix(c.Exp, 0))

	// A token anywhere but its carrier is refused before anything else reads
	// the request. The body is held for the scan, and goes upstream from the
	// copy.
	held, err := h.holdBody(rec, r, p.sandbox)
	defer held.release()
	if err != nil {
		// The rest of the body is not read, and the server must not wait
		// for it before it answers.
		rec.Header().Set("Connection", "close")
	}
	switch {
	case errors.Is(err, errBudget):
		a.check = "budget"
		rec.Header().Set("Retry-After", "1")
		http.Error(rec, "brig broker: too many request bodies are held at once; try again", http.StatusServiceUnavailable)
		return
	case timedOut(err):
		a.check = "body"
		http.Error(rec, "brig broker: the request body did not arrive in time", http.StatusRequestTimeout)
		return
	case err != nil:
		a.check = "body"
		http.Error(rec, "brig broker: the request body could not be read", http.StatusBadRequest)
		return
	}
	// The hold can outlast the token. One that expired meanwhile is refused
	// as on arrival, and the scan runs at the same instant, so a token that
	// passes here is still among the live ones the scan looks for.
	now := h.now()
	if !now.Before(time.Unix(c.Exp, 0)) {
		a.check = errTokenExpired.Error()
		http.Error(rec, "brig broker: the session token was refused", http.StatusUnauthorized)
		return
	}
	hit, partial, err := findPlaceholder(sb.matcher(now), r, p, rt.spec.Inject, token, held)
	if partial {
		a.placeholder = "partial"
	}
	switch {
	case errors.Is(err, errInflated):
		a.check = "body"
		http.Error(rec, "brig broker: the request body decompresses to more than 8 MiB", http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, errContentEncoding):
		a.check = "content-encoding"
		http.Error(rec, "brig broker: a request body to a route is plain, gzip or deflate, and readable",
			http.StatusUnsupportedMediaType)
		return
	case hit != nil:
		sb.placeholders.Add(1)
		a.check, a.where, a.encoding = "placeholder", hit.where, hit.encoding
		// The line of a refusal carries neither the path nor a method that
		// held the token.
		a.path = redacted
		if hit.where == "method" {
			a.method = redacted
		}
		http.Error(rec, fmt.Sprintf("brig: refused: the session token was sent outside its carrier (route %s, in %s)",
			hit.route, hit.where), http.StatusForbidden)
		return
	}

	// request.allow matches the path the upstream serves, so a path that
	// the upstream could resolve to another one is refused first.
	repos := rt.spec.Request != nil && rt.spec.Request.Allow != nil && len(rt.spec.Request.Allow.Repos) > 0
	if !canonical(p.rest, p.escapedRest, repos) {
		a.check = "path"
		http.Error(rec, "brig broker: the path has a dot segment, an empty segment or an escaped separator",
			http.StatusBadRequest)
		return
	}
	if rt.spec.Request != nil {
		if ref := allow(rt.spec.Request.Allow, r.Method, p.rest); ref != nil {
			a.check = "request.allow." + ref.field
			a.policy, a.rule, a.reason = rt.policy, ref.rule(p.route), ref.reason
			rec.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rec.Header().Set("X-Content-Type-Options", "nosniff")
			rec.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(rec, ref.body(rt.policy, p.route, p.sandbox))
			return
		}
	}

	cred := rt.spec.Credential
	doc, err := h.creds.Resolve(r.Context(), cred.SecretRef.Name, wl)
	if err != nil {
		a.check = "credential"
		msg := "brig broker: the credential of route " + p.route + " could not be resolved"
		if errors.Is(err, secret.ErrUnsupported) {
			// The one reason the guest can act on: the host has no store.
			msg += ": brig's secret store is not available on this host; see the broker log"
		}
		http.Error(rec, msg, http.StatusBadGateway)
		return
	}
	value, err := credentialValue(cred, doc)
	if err != nil {
		a.check = "credential"
		http.Error(rec, "brig broker: the credential of route "+p.route+" could not be resolved", http.StatusBadGateway)
		return
	}
	h.forward(rec, r, a, p, rt, token, wl, doc, value, held)
}

// forward sends the request to the route's upstream with the credential
// injected, and streams the response back with the credential redacted.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, a *auditLine, p routePath, rt *route,
	token string, wl secret.Workload, doc []byte, value string, held *heldBody) {
	in := rt.spec.Inject
	trip := &upstreamTrip{h: h, cred: rt.spec.Credential, inject: in, workload: wl, doc: doc, audit: a, body: held}
	trip.addSecret(value)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme:   rt.upstream.Scheme,
				Host:     rt.upstream.Host,
				Path:     p.rest,
				RawPath:  p.escapedRest,
				RawQuery: r.URL.RawQuery,
			}
			pr.Out.Host = ""
			// Request trailers are not forwarded. The held body is read to
			// its end before this clone, and the header rules below would
			// not see them.
			pr.Out.Trailer = nil
			a.dropped = dropGuestHeaders(in, pr.Out.Header, token)
			// The transport asks for gzip, and undoes it, only when the
			// request has no Range and is not a HEAD. Otherwise the
			// upstream is asked for the body as it is.
			if pr.Out.Header.Get("Range") != "" || pr.Out.Method == http.MethodHead {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
			injectCredential(in, pr.Out.Header, value)
		},
		Transport:      trip,
		ModifyResponse: trip.redactResponse,
		// An event stream reaches the guest event by event.
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errEncoded) {
				a.check = "response.encoding"
				http.Error(w, "brig broker: the upstream of route "+p.route+" sent a body in an encoding the broker cannot read",
					http.StatusBadGateway)
				return
			}
			a.check = "upstream"
			http.Error(w, "brig broker: the upstream of route "+p.route+" could not be reached", http.StatusBadGateway)
		},
	}
	// The transport can still read the request body after the upstream
	// answers. It reads once more past the last byte to check that the body
	// ends, and a long body may still be arriving. By default, the server
	// reads the rest of the body and closes it when the response header goes
	// out. That read waits for the guest to finish the body. The transport
	// then fails on the closed body and closes the upstream connection while
	// the guest reads the response. Full duplex leaves the body to the
	// transport. HTTP/2 is full duplex already and returns an error here.
	_ = http.NewResponseController(w).EnableFullDuplex()
	rp.ServeHTTP(w, r)
}

// upgrade reports whether h asks for a protocol upgrade.
func upgrade(h http.Header) bool {
	if len(h.Values("Upgrade")) > 0 {
		return true
	}
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// upstreamTrip carries one guest request upstream. On a 401 for a credential
// with a lifecycle it renews the credential once and sends the request once
// more.
type upstreamTrip struct {
	h        *Handler
	cred     *v1alpha1.CredentialRef
	inject   *v1alpha1.Inject
	workload secret.Workload
	doc      []byte
	audit    *auditLine
	// body is the request body held for the retry. ReverseProxy wraps the
	// body it hands the transport, so the trip keeps its own reference.
	body *heldBody
	// secrets are the exact strings the broker put on the upstream request
	// that carry a credential. The response is redacted of each.
	secrets []string
}

// addSecret records the strings that injecting value puts on the request:
// value itself and, on a basicAuth route, the base64 the header carries.
func (u *upstreamTrip) addSecret(value string) {
	u.secrets = append(u.secrets, value)
	if u.inject.BasicAuth != nil {
		raw := u.inject.BasicAuth.Username + ":" + value
		u.secrets = append(u.secrets, base64.StdEncoding.EncodeToString([]byte(raw)))
	}
}

func (u *upstreamTrip) renewable() bool {
	return u.h.renewer != nil && u.cred.Lifecycle != ""
}

func (u *upstreamTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := u.h.transport.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !u.renewable() {
		return resp, err
	}
	var replay []byte
	if req.Body != nil && req.Body != http.NoBody {
		if u.body == nil {
			return resp, nil
		}
		var ok bool
		if replay, ok = u.body.complete(); !ok {
			u.audit.renew = "body"
			return resp, nil
		}
	}
	fresh, ok := u.renewed(req.Context())
	if !ok {
		return resp, nil
	}
	value, err := credentialValue(u.cred, fresh)
	if err != nil {
		u.audit.renew = "failed"
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	retry := req.Clone(req.Context())
	if replay != nil {
		retry.Body = io.NopCloser(bytes.NewReader(replay))
		retry.ContentLength = int64(len(replay))
		retry.GetBody = nil
	}
	injectCredential(u.inject, retry.Header, value)
	u.addSecret(value)
	u.audit.renew = "retried"
	return u.h.transport.RoundTrip(retry)
}

// renewed returns the document to retry with after an upstream 401. It
// forces a renewal, unless one was forced for the same secret within
// renewInterval. Then it reads the secret again, and retries only when another
// request has renewed it since.
func (u *upstreamTrip) renewed(ctx context.Context) ([]byte, bool) {
	name := u.cred.SecretRef.Name
	if !u.h.mayForceRenew(name) {
		doc, err := u.h.creds.Resolve(ctx, name, u.workload)
		if err != nil || bytes.Equal(doc, u.doc) {
			u.audit.renew = "interval"
			return nil, false
		}
		return doc, true
	}
	fresh, err := u.h.renewer.ForceRenew(ctx, name, u.workload, u.doc)
	if err != nil {
		u.audit.renew = "failed"
		return nil, false
	}
	return fresh, true
}

// routePath is a request path split at its route prefix.
type routePath struct {
	sandbox, route string
	// rest is the path after the route prefix, decoded, and escapedRest is
	// the same path as the guest escaped it. Both start with a slash.
	rest, escapedRest string
}

// splitRoutePath splits /<sandbox>/r/<route>/<rest>. A sandbox id and a route
// name hold no character that needs escaping, so the escaped path has the
// same prefix as the decoded one.
func splitRoutePath(u *url.URL) (routePath, bool) {
	sb, rt, rest, ok := splitPrefix(u.Path)
	if !ok {
		return routePath{}, false
	}
	esb, ert, erest, ok := splitPrefix(u.EscapedPath())
	if !ok || esb != sb || ert != rt {
		return routePath{}, false
	}
	return routePath{sandbox: sb, route: rt, rest: rest, escapedRest: erest}, true
}

func splitPrefix(p string) (sandbox, route, rest string, ok bool) {
	s, ok := strings.CutPrefix(p, "/")
	if !ok {
		return "", "", "", false
	}
	sandbox, s, ok = strings.Cut(s, "/")
	if !ok || sandbox == "" {
		return "", "", "", false
	}
	marker, s, ok := strings.Cut(s, "/")
	if !ok || marker != "r" {
		return "", "", "", false
	}
	route, rest, _ = strings.Cut(s, "/")
	if route == "" {
		return "", "", "", false
	}
	return sandbox, route, "/" + rest, true
}

// lookalikes are the code points that an upstream which normalizes Unicode
// may read as a slash, a backslash or a percent sign: those that NFKC folds
// to one (U+FF0F, U+FF3C, U+FE68, U+FF05, U+FE6A, and U+2100, U+2101,
// U+2105 and U+2106, which fold to a/c, a/s, c/o and c/u), and the slash
// and backslash look-alikes that no normalization folds but a lossy
// conversion to a legacy charset may (U+2215, U+2044, U+29F8, U+29F5,
// U+29F9, U+2216). ．．／admin is one segment here, and ../admin to such an
// upstream.
const lookalikes = "\uff0f\uff3c\ufe68\uff05\ufe6a\u2100\u2101\u2105\u2106" +
	"\u2215\u2044\u29f8\u29f5\u29f9\u2216"

// dots are the code points that NFKC folds to dots alone (U+FF0E, U+FE52,
// U+2024, U+2025, U+FE30, U+2026, U+FE19), and the ideographic full stops
// that IDNA reads as a dot (U+3002, U+FF61). A segment of them and ASCII
// dots is a dot segment to an upstream that normalizes.
const dots = ".\uff0e\ufe52\u2024\u2025\ufe30\u2026\ufe19\u3002\uff61"

// semicolons are ; and the code points that NFKC folds to it (U+FF1B,
// U+FE54, U+FE14, U+037E), U+037E under NFC too. A segment's parameter
// starts at any of them: an upstream that normalizes and then strips path
// parameters reads ..；x as .. and leaves the allowed tree.
const semicolons = ";\uff1b\ufe54\ufe14\u037e"

// canonical reports whether rest reaches the upstream as written. It refuses
// a dot segment, also with a ;parameter after it, in Unicode dots, or with
// a Unicode semicolon before its parameter, an empty segment but a trailing
// one, a backslash or a control character, a Unicode slash, backslash or
// percent sign, an escaped slash, backslash or dot, and an escaped percent
// sign. A % left after one decode is the start of an escape to an upstream
// that decodes twice: %25%32%65 reads as %2e here and as a dot there. On a
// route with request.allow.repos it refuses any byte outside ASCII as well,
// since the repository is read from the first two segments.
func canonical(rest, escapedRest string, repos bool) bool {
	lower := strings.ToLower(escapedRest)
	for _, esc := range []string{"%2f", "%5c", "%2e", "%252f", "%255c", "%252e"} {
		if strings.Contains(lower, esc) {
			return false
		}
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < ' ' || c == 0x7f || c == '\\' || c == '%' || (repos && c >= 0x80) {
			return false
		}
	}
	if strings.ContainsAny(rest, lookalikes) {
		return false
	}
	segs := strings.Split(rest[1:], "/")
	for i, seg := range segs {
		if j := strings.IndexAny(seg, semicolons); j >= 0 {
			seg = seg[:j]
		}
		switch {
		case seg == "" && i != len(segs)-1:
			return false
		case seg != "" && strings.Trim(seg, dots) == "":
			return false
		}
	}
	return true
}

// recorder keeps the status a response was sent with, for the audit line.
// Unwrap lets ReverseProxy flush and hijack through it.
type recorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader drops a 1xx. ReverseProxy passes on each 1xx the upstream
// sends, such as 103 Early Hints, with its headers as sent, before the
// response that redactResponse sees. The broker's own server still answers
// a guest's Expect: 100-continue.
func (r *recorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		clear(r.Header())
		return
	}
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// auditLine is what the audit line of one request says. It holds names,
// counts and the request path, and never a header value, a token, a
// credential or a body.
type auditLine struct {
	workload, sandbox, route, method, path string
	// arrived is the sandbox whose divert socket the request arrived on, or
	// "" for the loopback port. The broker sets it, not the sender.
	arrived string
	// check names the check that refused the request.
	check string
	// policy, rule and reason repeat the refusal body of a request.allow
	// miss.
	policy, rule, reason string
	// dropped counts the values inject.headers.allow dropped, per header.
	dropped map[string]int
	// renew says what an upstream 401 led to: retried, failed, or body when
	// the body was too large to send again.
	renew string
	// where and encoding say where a session token outside its carrier was
	// found, and how it was spelled. placeholder is partial when only the
	// start of the body was scanned.
	where, encoding, placeholder string
}

// The longest method and path an audit line carries. Anything that reaches
// the broker's port can send a request, and the audit log must not grow by
// what the sender chooses to write.
const (
	maxAuditMethod = 16
	maxAuditPath   = 256
)

// clip cuts s to n bytes.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (h *Handler) writeAudit(ctx context.Context, a *auditLine, status int, dur time.Duration) {
	attrs := make([]slog.Attr, 0, 16)
	// A request whose token has not verified carries only what the broker
	// chose: the method, the status, the check and where it arrived. Its
	// path, sandbox and route are whatever the sender wrote.
	if a.workload == "" {
		attrs = append(attrs,
			slog.String("method", clip(a.method, maxAuditMethod)),
			slog.Int("status", status),
			slog.Duration("dur", dur),
		)
		if a.check != "" {
			attrs = append(attrs, slog.String("check", a.check))
		}
		if a.arrived != "" {
			attrs = append(attrs, slog.String("arrived", a.arrived))
		}
		h.audit.LogAttrs(ctx, slog.LevelInfo, "broker", attrs...)
		return
	}
	attrs = append(attrs,
		slog.String("workload", a.workload),
		slog.String("sandbox", a.sandbox),
		slog.String("route", a.route),
		slog.String("method", clip(a.method, maxAuditMethod)),
		slog.String("path", clip(a.path, maxAuditPath)),
		slog.Int("status", status),
		slog.Duration("dur", dur),
	)
	if a.check != "" {
		attrs = append(attrs, slog.String("check", a.check))
	}
	if a.arrived != "" {
		attrs = append(attrs, slog.String("arrived", a.arrived))
	}
	if a.rule != "" {
		attrs = append(attrs, slog.String("policy", a.policy), slog.String("rule", a.rule),
			slog.String("reason", a.reason))
	}
	for name, n := range a.dropped {
		attrs = append(attrs, slog.Int("dropped."+strings.ToLower(name), n))
	}
	if a.renew != "" {
		attrs = append(attrs, slog.String("renew", a.renew))
	}
	if a.where != "" {
		attrs = append(attrs, slog.String("where", a.where), slog.String("encoding", a.encoding))
	}
	if a.placeholder != "" {
		attrs = append(attrs, slog.String("placeholder", a.placeholder))
	}
	h.audit.LogAttrs(ctx, slog.LevelInfo, "broker", attrs...)
}
