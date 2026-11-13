package broker

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/graphql"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/tlspem"
)

// maxReplay is the largest request body held before the request goes
// upstream: scanned whole for a session token, and kept for the retry after
// an upstream 401. Of a larger body only the start is scanned, and its 401
// goes back to the guest.
const maxReplay = 8 << 20

// ServeHTTP serves /<sandbox-id>/r/<route>/<rest>, and /<rest> at a host
// route's own name <route>.brig.internal. The checks run in the order
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

	// A request that names a host route by its Host is served for the sandbox
	// whose divert socket it arrived on, and never for one it names itself.
	var (
		p        routePath
		ok       bool
		hostName string
	)
	if name, isHost := hostRoute(r.Host); isHost {
		hostName = name
		id, _ := h.arrival("", r.RemoteAddr)
		a.arrived = id
		// Loopback names no sandbox, and a clear-text arrival would expose the token.
		if id == "" || (h.vendOnly != nil && h.vendOnly(r)) {
			a.check = "arrival"
			http.Error(rec, "brig broker: no such route", http.StatusNotFound)
			return
		}
		p = routePath{sandbox: id, route: name, rest: r.URL.Path, escapedRest: r.URL.EscapedPath()}
		if p.rest == "" {
			p.rest, p.escapedRest = "/", "/"
		}
		// OPTIONS * has no slash, and rest must start with one.
		if !strings.HasPrefix(p.rest, "/") {
			a.check = "path"
			http.Error(rec, "brig broker: no such route", http.StatusNotFound)
			return
		}
	} else {
		p, ok = splitRoutePath(r.URL)
		if !ok {
			a.check = "path"
			http.Error(rec, "brig broker: no such route", http.StatusNotFound)
			return
		}
		// A request is served only for a sandbox that reaches the broker where it
		// arrived. Any other sandbox's path gets the answer of one that does not
		// exist, before a lookup or a token check could tell the two apart.
		var arrived string
		arrived, ok = h.arrival(p.sandbox, r.RemoteAddr)
		a.arrived = arrived
		if !ok {
			a.check = "arrival"
			http.Error(rec, "brig broker: no such route", http.StatusNotFound)
			return
		}
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
	// A route that does not vend is not served in clear text. The refusal
	// comes before the token is read, so the token opens nothing there, and
	// reads as for a route the sandbox does not have.
	if h.vendOnly != nil && !rt.spec.Vends() && h.vendOnly(r) {
		a.check = "route.cleartext"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}
	// A route is served only the way its mode asks: a host route by its name,
	// an endpoint by its path, and a route in any other mode not here.
	if (hostName != "" && rt.spec.Mode != v1alpha1.ModeHost) ||
		(hostName == "" && rt.spec.Mode != "" && rt.spec.Mode != v1alpha1.ModeEndpoint) {
		a.check = "route.mode"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}

	token := sessionToken(&rt.spec, r.Header)
	if token == "" {
		a.check = errTokenMissing.Error()
		refuseToken(rec, rt)
		return
	}
	c, err := verify(sb.key, token, p.sandbox, p.route, h.now())
	if err != nil {
		a.check = err.Error()
		refuseToken(rec, rt)
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
		refuseToken(rec, rt)
		return
	}
	hit, partial, err := findPlaceholder(sb.matcher(now), r, p, &rt.spec, token, held)
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

	// A route that vends has no upstream to confuse, and serves the route
	// URL alone: any other path is a request for something it does not have.
	vends := rt.spec.Vends()
	if vends && p.rest != "/" {
		a.check = "path"
		http.Error(rec, "brig broker: no such route", http.StatusNotFound)
		return
	}
	// request.allow matches the path the upstream serves (on github-api, the
	// path gh sent), so a path that the upstream could resolve to another one
	// is refused first.
	repos := rt.spec.Request != nil && rt.spec.Request.Allow != nil && len(rt.spec.Request.Allow.Repos) > 0
	var objects map[int]bool
	if rt.services != nil {
		objects = storageObjects(p.escapedRest)
	}
	if !vends && !canonical(p.rest, p.escapedRest, repos, objects) {
		a.check = "path"
		http.Error(rec, "brig broker: the path has a dot segment, an empty segment or an escaped separator",
			http.StatusBadRequest)
		return
	}
	// With request.allow.graphql, only that rule governs /api/graphql, and
	// the rest of the API is read-only under methods and paths.
	gq := graphQLRules(rt)
	isGraphQL := false
	if gq != nil {
		var reason string
		isGraphQL, reason = graphQLEndpoint(r, p)
		if reason != "" {
			refuseRule(rec, a, rt, p, "request.allow.graphql", routeRule(p.route)+".request.allow.graphql", reason)
			return
		}
	}
	if rt.spec.Request != nil && !isGraphQL {
		if ref := allow(rt.spec.Request.Allow, r.Method, p.rest); ref != nil {
			refuseRule(rec, a, rt, p, "request.allow."+ref.field, ref.rule(routeRule(p.route)), ref.reason)
			return
		}
	}
	// A route with services forwards to the host of the service its path
	// names, the path after the service.
	upstream, out := rt.upstream, p
	// gh addresses a GitHub Enterprise host; api.github.com serves the same
	// calls under other paths. Any other path is refused before the
	// credential is read.
	if rt.spec.Preset == v1alpha1.PresetGitHubAPI {
		var ok bool
		if out, ok = githubAPIPath(p); !ok {
			a.check = "github-api.path"
			http.Error(rec, "brig broker: no such route", http.StatusNotFound)
			return
		}
	}
	var gqRepos []graphql.Repo
	if isGraphQL {
		var reason string
		if gqRepos, reason = checkGraphQL(r, held, gq); reason != "" {
			refuseRule(rec, a, rt, p, "request.allow.graphql", routeRule(p.route)+".request.allow.graphql", reason)
			return
		}
	}
	if rt.services != nil {
		var ok bool
		if upstream, out, ok = serviceTarget(rec, r, a, rt, p, objects); !ok {
			return
		}
	}

	// A route that presents a client certificate and sets no header has no
	// credential: the certificate is its identity, and it is read when the
	// request goes upstream.
	var (
		doc   []byte
		value string
	)
	if cred := rt.spec.Credential; cred != nil {
		doc, err = h.creds.Resolve(r.Context(), cred.SecretRef.Name, wl)
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
		// A route that vends always has a credential: the aws preset takes
		// no inject, so it cannot be a route with a certificate alone.
		if vends {
			h.vend(rec, r, a, p, doc)
			return
		}
		if h.vendDoc != nil && h.vendDoc(doc) {
			a.check = "credential.vendonly"
			http.Error(rec, "brig broker: the credential of route "+p.route+" is for a route that vends, "+
				"such as one on the aws preset", http.StatusBadGateway)
			return
		}
		if value, err = h.credential(r, p, rt, cred, doc); err != nil {
			credentialRefusal(rec, a, p, err)
			return
		}
	}
	if reason := h.confirmRepos(r, rt, upstream, value, gqRepos); reason != "" {
		refuseRule(rec, a, rt, p, "request.allow.graphql", routeRule(p.route)+".request.allow.graphql.repos", reason)
		return
	}
	h.forward(rec, r, a, out, rt, upstream, token, wl, doc, value, held)
}

// refuseRule answers 403 for a request that a rule of the policy refused,
// with a body that names the rule, and puts the rule on the audit line.
func refuseRule(w http.ResponseWriter, a *auditLine, rt *route, p routePath, check, rule, reason string) {
	a.check = check
	a.policy, a.rule, a.reason = rt.policy, rule, reason
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, refusalBody(rt.policy, p.route, rule, reason, p.sandbox))
}

// methodOverrides are the headers that tell a server to serve a request as
// another method. Google APIs honour X-HTTP-Method-Override, so a GET that
// request.allow.methods let through would be served as the DELETE it names.
// The other two cost nothing to refuse, and some servers read them.
var methodOverrides = []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"}

// queryOverrides are the query parameters that Google APIs read as the
// method to serve. They are matched in any case: whether every Google
// front end reads them case-sensitively is not something brig can check,
// and refusing a parameter that differs only in case costs a client
// nothing.
var queryOverrides = []string{"$httpMethod", "httpMethod"}

// queryOverride returns the method-override parameter rawQuery names, or "".
// It reads the query itself instead of through url.ParseQuery, which drops a
// pair it cannot parse, such as one after a semicolon, while the upstream
// gets the query as the guest wrote it. Any pair that names the parameter
// counts, whatever its value and however often it appears, and a key that
// does not decode is compared as written.
func queryOverride(rawQuery string) string {
	for _, pair := range strings.FieldsFunc(rawQuery, func(c rune) bool { return c == '&' || c == ';' }) {
		key, _, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(key); err == nil {
			key = k
		}
		for _, name := range queryOverrides {
			if strings.EqualFold(key, name) {
				return name
			}
		}
	}
	return ""
}

// serviceTarget applies the checks of a route with services, and returns the
// origin of the service the path names and the path with the service cut
// off. It answers the request itself and returns false when a check refuses
// it. Every check runs before the credential is read. objects are the
// segments of p.escapedRest that name a Cloud Storage object.
func serviceTarget(w http.ResponseWriter, r *http.Request, a *auditLine, rt *route,
	p routePath, objects map[int]bool) (*url.URL, routePath, bool) {
	// The service is read from the decoded path and the path is sent as the
	// guest escaped it, so the two must name the same service. canonical
	// has refused an escaped slash outside an object name already; this
	// refuses any other escape in the service segment.
	name, rest := cutService(p.rest)
	ename, erest := cutService(p.escapedRest)
	if name != ename {
		a.check = "path"
		http.Error(w, "brig broker: the service in the path is escaped", http.StatusBadRequest)
		return nil, p, false
	}
	// The override is refused rather than dropped: a client that sends it
	// asked for the other method, and sending the request as the method it
	// was checked as would do something the client did not ask for.
	// The refusal names the route, not a service: the route refuses an
	// override whatever service the path names.
	for _, hdr := range methodOverrides {
		if len(r.Header.Values(hdr)) > 0 {
			refuseRule(w, a, rt, p, "request.method-override", routeRule(p.route),
				"header "+hdr+" names another method; send the request with the method it is for")
			return nil, p, false
		}
	}
	if param := queryOverride(r.URL.RawQuery); param != "" {
		refuseRule(w, a, rt, p, "request.method-override", routeRule(p.route),
			"query parameter "+param+" names another method; send the request with the method it is for")
		return nil, p, false
	}
	u := rt.services[name]
	if u == nil {
		reason := fmt.Sprintf("service %q is not in the list %s", name, list(rt.serviceNames))
		if name == "" {
			reason = "the path names no service of the list " + list(rt.serviceNames)
		}
		refuseRule(w, a, rt, p, "request.services", routeRule(p.route)+".services", reason)
		return nil, p, false
	}
	// A batch request carries many requests in its body, each with its own
	// method and path, and request.allow sees only the outer one.
	if seg, _, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/"); isBatch(seg) {
		refuseRule(w, a, rt, p, "request.batch", routeRule(p.route)+".services["+name+"]",
			"a batch request is not taken; send each request on its own")
		return nil, p, false
	}
	if svc := serviceSpec(&rt.spec, name); svc != nil && svc.Request != nil {
		_, match := cutService(matchPath(p.rest, p.escapedRest, objects))
		if ref := allow(svc.Request.Allow, r.Method, match); ref != nil {
			owner := routeRule(p.route) + ".services[" + name + "]"
			refuseRule(w, a, rt, p, "request.allow."+ref.field, ref.rule(owner), ref.reason)
			return nil, p, false
		}
	}
	p.rest, p.escapedRest = rest, erest
	return u, p, true
}

// serviceSpec returns the service name of spec, or nil.
func serviceSpec(spec *v1alpha1.Route, name string) *v1alpha1.Service {
	for i := range spec.Services {
		if spec.Services[i].Name == name {
			return &spec.Services[i]
		}
	}
	return nil
}

// cutService splits /<service>/<rest> into the service and /<rest>. The path
// of the bare route, /, names no service.
func cutService(path string) (service, rest string) {
	service, rest, _ = strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return service, "/" + rest
}

// isBatch reports whether seg, the first segment after the service, is the
// batch endpoint. Its case and any ;parameter are ignored, since a server
// may ignore them too.
func isBatch(seg string) bool {
	if i := strings.IndexAny(seg, semicolons); i >= 0 {
		seg = seg[:i]
	}
	return strings.EqualFold(seg, "batch")
}

// forward sends the request to upstream with the credential injected, and
// streams the response back with the credential redacted. p.rest is the path
// the upstream gets.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, a *auditLine, p routePath, rt *route,
	upstream *url.URL, token string, wl secret.Workload, doc []byte, value string, held *heldBody) {
	in := rt.spec.Inject
	trip := &upstreamTrip{h: h, route: rt, cred: rt.spec.Credential, inject: in, workload: wl, doc: doc, audit: a, body: held}
	trip.addSecret(value)
	// After the response has been copied: a transport that a rotation retired
	// while this request was on it closes its connections now.
	defer trip.done()
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme:   upstream.Scheme,
				Host:     upstream.Host,
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
			// The sandbox went between the lookup of the route and the dial.
			// It is answered as a sandbox the broker no longer serves.
			if errors.Is(err, errRouteClosed) {
				a.check = "sandbox"
				http.Error(w, "brig broker: no such route", http.StatusNotFound)
				return
			}
			// A failure of the TLS side names its check and a fixed body,
			// and leaves the cause out of both.
			var tf *tlsFailure
			if errors.As(err, &tf) {
				a.check = tf.check
				http.Error(w, tlsBody(p.route, tf.check), http.StatusBadGateway)
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
	route    *route
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
	// tt is the transport of a route with inject.tls, with the certificate it
	// presents, picked on the first round trip. A retry after a 401 uses the
	// same one, and the audit line names the certificate of this transport:
	// the one the connection carried.
	tt *tlsTransport
}

// done ends the request's use of its transport. A transport that a rotation
// retired meanwhile closes its connections after the last request that held
// it.
func (u *upstreamTrip) done() {
	if u.tt != nil {
		u.route.release(u.tt)
	}
}

// addSecret records the strings that injecting value puts on the request:
// value itself and, on a basicAuth route, the base64 the header carries.
func (u *upstreamTrip) addSecret(value string) {
	// A route without a credential injects nothing, and an empty string
	// would match every position of a body.
	if value == "" {
		return
	}
	u.secrets = append(u.secrets, value)
	if u.inject.BasicAuth != nil {
		raw := u.inject.BasicAuth.Username + ":" + value
		u.secrets = append(u.secrets, base64.StdEncoding.EncodeToString([]byte(raw)))
	}
}

// renewable reports whether a 401 renews the credential and retries. A
// minted token is not, and the mints clause is defence in depth: the gcp
// preset has no lifecycle today, so no gcp route retries without it. If a
// lifecycle is added to that preset later, the clause still keeps a retry
// from renewing the pointer or sending it as the credential, and gcloud
// would hand back the same cached token anyway.
func (u *upstreamTrip) renewable() bool {
	return u.cred != nil && u.h.renewer != nil && u.cred.Lifecycle != "" && !u.h.mints(u.doc)
}

// prepareTLS resolves the CA and the client certificate of the route, and
// picks its transport. Both are read for every request, like a header
// credential, and cached by their bytes.
func (u *upstreamTrip) prepareTLS(ctx context.Context) error {
	t := u.inject.TLS
	var (
		pool *x509.CertPool
		sum  [32]byte
	)
	if t.CA != nil {
		var err error
		if pool, sum, err = u.h.caPool(ctx, t.CA.SecretRef.Name, u.workload); err != nil {
			return &tlsFailure{check: "tls.ca", cause: err}
		}
	}
	var cc *tlspem.ClientCertificate
	if t.ClientCertificate != nil {
		var err error
		cc, err = u.h.clientCertificate(ctx, t.ClientCertificate.SecretRef.Name, u.workload)
		if err != nil {
			return &tlsFailure{check: certCheck(err), cause: err}
		}
	}
	tt, err := u.h.routeTransport(u.route, pool, sum, cc)
	if err != nil {
		return err
	}
	u.tt = tt
	// The audit line names the certificate of the transport. That is the one
	// the connection presents, since a transport is built for one chain.
	if tt.leaf != nil {
		u.audit.cert = tt.leaf.fingerprint
		u.audit.certNotAfter = tt.leaf.notAfter
	}
	return nil
}

// send makes one upstream round trip. A route with inject.tls goes through
// the transport of its sandbox and route, which presents the certificate it
// was built with; any other goes through the shared one.
func (u *upstreamTrip) send(req *http.Request) (*http.Response, error) {
	if u.inject.TLS == nil {
		return u.h.transport.RoundTrip(req)
	}
	if u.tt == nil {
		if err := u.prepareTLS(req.Context()); err != nil {
			return nil, err
		}
	}
	resp, newConn, err := roundTrip(u.tt.tr, req)
	if err != nil && tlsHandshakeFailed(err, newConn) {
		return nil, &tlsFailure{check: "tls.handshake", cause: err}
	}
	return resp, err
}

// roundTrip sends req on tr and reports whether the request rode a new
// connection. A request that failed before it got a connection counts as on a
// new one, since the failure was in setting one up.
func roundTrip(tr http.RoundTripper, req *http.Request) (resp *http.Response, newConn bool, err error) {
	var reused atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused.Store(i.Reused) }}
	resp, err = tr.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	return resp, !reused.Load(), err
}

func (u *upstreamTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := u.send(req)
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
	return u.send(retry)
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

// githubAPIPath maps the paths gh sends to a GitHub Enterprise host onto
// api.github.com: /api/v3/<x> to /<x> and /api/graphql to /graphql. The
// decoded and the escaped path must cut the same way, so the one that is
// forwarded is the one that was checked.
func githubAPIPath(p routePath) (routePath, bool) {
	cut := func(path string) (string, bool) {
		switch {
		case path == "/api/graphql":
			return "/graphql", true
		case path == "/api/v3":
			return "/", true
		case strings.HasPrefix(path, "/api/v3/"):
			return strings.TrimPrefix(path, "/api/v3"), true
		}
		return "", false
	}
	rest, ok := cut(p.rest)
	if !ok {
		return p, false
	}
	escaped, ok := cut(p.escapedRest)
	if !ok {
		return p, false
	}
	p.rest, p.escapedRest = rest, escaped
	return p, true
}

// hostRoute reads the route out of a Host of the form <route>.brig.internal.
// A port, case and one trailing dot are dropped; bare brig.internal and a
// name with more than one label are not a host route.
func hostRoute(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	name, ok := strings.CutSuffix(host, v1alpha1.HostName(""))
	if !ok || strings.Contains(name, ".") || v1alpha1.CheckRouteName(name) != nil {
		return "", false
	}
	return name, true
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
//
// An escaped slash is taken in the segments of escapedRest that objects
// names: a Cloud Storage object name, which the upstream does not split
// (see storageObjects). The decoded name is still checked as segments, so a
// name with a dot or empty piece is refused, but for the empty piece a
// trailing slash leaves: a folder placeholder such as dir/ is a name, and a
// copy, rewrite or move takes it as a source, before the verb, as well as at
// the end. The escapes are looked for segment by segment, since no escape
// spans a slash.
func canonical(rest, escapedRest string, repos bool, objects map[int]bool) bool {
	// trailing holds the decoded segments that are the empty last piece of
	// an object name: each escaped segment decodes to one segment more
	// than it has %2f.
	trailing := map[int]bool{}
	at := 0
	for i, seg := range strings.Split(strings.TrimPrefix(strings.ToLower(escapedRest), "/"), "/") {
		for _, esc := range []string{"%2f", "%5c", "%2e", "%252f", "%255c", "%252e"} {
			if strings.Contains(seg, esc) && (esc != "%2f" || !objects[i]) {
				return false
			}
		}
		pieces := strings.Count(seg, "%2f") + 1
		if objects[i] && pieces > 1 && strings.HasSuffix(seg, "%2f") {
			trailing[at+pieces-1] = true
		}
		at += pieces
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
		case seg == "" && i != len(segs)-1 && !trailing[i]:
			return false
		case seg != "" && strings.Trim(seg, dots) == "":
			return false
		}
	}
	return true
}

// refuseToken answers 401 for a session token that is missing, forged or
// expired. A route that carries the token as a basic-auth password also
// sends a Basic challenge: git's libcurl offers credentials only after one,
// so without it the client never sends the token and fails with
// "Authentication failed". A route that carries the token in a header gets
// no challenge, since its client sends the header up front.
func refuseToken(w http.ResponseWriter, rt *route) {
	if rt.spec.Inject != nil && rt.spec.Inject.BasicAuth != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="brig"`)
	}
	http.Error(w, "brig broker: the session token was refused", http.StatusUnauthorized)
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
	// vended is the AccessKeyId a route that vends answered with. It is an
	// identifier, never the secret key or the session token.
	vended string
	// cert and certNotAfter name the client certificate a request presented:
	// a short digest of the leaf and its expiry. Never the certificate.
	cert, certNotAfter string
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
	if a.vended != "" {
		attrs = append(attrs, slog.String("vended", a.vended))
	}
	if a.cert != "" {
		attrs = append(attrs, slog.String("cert", a.cert), slog.String("cert.notAfter", a.certNotAfter))
	}
	h.audit.LogAttrs(ctx, slog.LevelInfo, "broker", attrs...)
}
