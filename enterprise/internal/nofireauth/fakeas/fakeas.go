// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package fakeas is a fake NOFire AI authorization server. Brig ships against
// it until the brain-go api service has the endpoints: the tests, the smoke
// test and a developer who runs fakenofire by hand all use it. It is strict
// on purpose. It enforces the server side of AUTH001 R2 to R5, so a Brig bug
// fails a test instead of passing against a lenient fake, and its Options
// make it misbehave in each way Brig must refuse.
package fakeas

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	clientID     = "brig"
	metadataPath = "/.well-known/oauth-authorization-server"
	deviceGrant  = "urn:ietf:params:oauth:grant-type:device_code"
	codeLifetime = 60 * time.Second
	// userCodeAlphabet has no vowels and no look-alike characters (R4).
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
)

// Approval says how the fake answers a request to sign in.
type Approval int

const (
	// Auto approves at once, without asking.
	Auto Approval = iota
	// Deny refuses at once.
	Deny
	// Page serves an HTML Allow/Deny form and waits for a person.
	Page
)

// Options configure the fake. The zero value is a well-behaved server.
type Options struct {
	Approval Approval
	// Issuer is the metadata issuer; "" is the server URL plus the path
	// prefix the metadata was asked under.
	Issuer string
	// EndpointBase is the origin of every endpoint; "" is the server URL.
	EndpointBase string
	// NoS256 advertises only the plain PKCE method. NoIssSupport leaves out
	// RFC 9207 support. NoDevice turns the device grant off.
	NoS256, NoIssSupport, NoDevice bool
	// CallbackIss is the iss on the redirect; "" is the issuer, "-" omits it.
	CallbackIss string
	// VerificationURI is the device verification page; "" is <server>/device.
	VerificationURI string
	// VerificationURIComplete is sent as verification_uri_complete; "" omits
	// it. A value that starts with "/" is taken relative to the server URL,
	// which a test does not know before Start.
	VerificationURIComplete string
	// RedirectAll makes every endpoint answer 302 elsewhere.
	RedirectAll bool
	// Polls are RFC 8628 errors returned to a device poll before it
	// succeeds, in order.
	Polls []string
	// DefaultTTL and MaxTTL bound the granted lifetime: 7 and 30 days when
	// zero. The grant is min(ttl, MaxTTL), or DefaultTTL without a ttl.
	DefaultTTL, MaxTTL time.Duration
	// ExpiresIn overrides expires_in; nil reports the granted lifetime.
	ExpiresIn *int
	// RevokeStatus is the status of the revocation endpoint; 0 is 200.
	RevokeStatus int
	// Interval is the device polling interval; 0 is 5 s. A test lowers it so
	// that a poll loop on a fake clock is not answered with slow_down.
	Interval time.Duration
	// Log, if set, is called with "METHOD path" for every request.
	Log func(line string)
}

type grant struct {
	redirect, challenge string
	ttl                 time.Duration
	expires             time.Time
}

type device struct {
	ttl      time.Duration
	interval time.Duration
	lastPoll time.Time
	polls    []string
	state    string // "pending", "approved" or "denied"
	expires  time.Time
}

// Server is a running fake.
type Server struct {
	URL string

	o   Options
	srv *http.Server

	mu                                sync.Mutex
	requests, agents, issued, revoked []string
	ttls                              []int
	tokens                            map[string]bool
	codes                             map[string]*grant
	devices                           map[string]*device
	userCodes                         map[string]string
	// metaIssuer is the issuer the last metadata response named. The
	// authorization endpoint has no path prefix, so the iss on a redirect
	// reuses it: iss must equal the issuer Brig discovered.
	metaIssuer string
}

// Start listens on addr and serves until Close.
func Start(addr string, o Options) (*Server, error) {
	if o.DefaultTTL == 0 {
		o.DefaultTTL = 7 * 24 * time.Hour
	}
	if o.MaxTTL == 0 {
		o.MaxTTL = 30 * 24 * time.Hour
	}
	if o.Interval == 0 {
		o.Interval = 5 * time.Second
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		URL:       "http://" + ln.Addr().String(),
		o:         o,
		tokens:    map[string]bool{},
		codes:     map[string]*grant{},
		devices:   map[string]*device{},
		userCodes: map[string]string{},
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

// Close stops the server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		return s.srv.Close()
	}
	return nil
}

// Issued returns the access tokens issued.
func (s *Server) Issued() []string { return s.copy(&s.issued) }

// Revoked returns the tokens presented to the revocation endpoint.
func (s *Server) Revoked() []string { return s.copy(&s.revoked) }

// Requests returns "METHOD path" of every request, in order.
func (s *Server) Requests() []string { return s.copy(&s.requests) }

// UserAgents returns the User-Agent of every request, in order.
func (s *Server) UserAgents() []string { return s.copy(&s.agents) }

// RequestedTTL returns the ttl values received, in seconds.
func (s *Server) RequestedTTL() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.ttls...)
}

func (s *Server) copy(p *[]string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), *p...)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	line := r.Method + " " + r.URL.Path
	s.mu.Lock()
	s.requests = append(s.requests, line)
	s.agents = append(s.agents, r.UserAgent())
	s.mu.Unlock()
	if s.o.Log != nil {
		s.o.Log(line)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.o.RedirectAll {
		// Back to the fake itself, so a client that follows the redirect
		// shows up as an extra request instead of failing on DNS.
		w.Header().Set("Location", s.URL+metadataPath)
		w.WriteHeader(http.StatusFound)
		return
	}
	switch p := r.URL.Path; {
	case p == metadataPath || strings.HasPrefix(p, metadataPath+"/"):
		s.metadata(w, r, strings.TrimPrefix(p, metadataPath))
	case p == "/oauth/authorize":
		s.authorize(w, r)
	case p == "/oauth/token":
		s.token(w, r)
	case p == "/oauth/device_authorization":
		s.deviceAuthorization(w, r)
	case p == "/oauth/revoke":
		s.revoke(w, r)
	case p == "/device":
		s.devicePage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) issuer(prefix string) string {
	if s.o.Issuer != "" {
		return s.o.Issuer
	}
	return s.URL + prefix
}

func (s *Server) callbackIss() string {
	if s.metaIssuer != "" {
		return s.metaIssuer
	}
	return s.issuer("")
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request, prefix string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.metaIssuer = s.issuer(prefix)
	base := s.o.EndpointBase
	if base == "" {
		base = s.URL
	}
	method := "S256"
	if s.o.NoS256 {
		method = "plain"
	}
	grants := []string{"authorization_code"}
	m := map[string]any{
		"issuer":                                         s.issuer(prefix),
		"authorization_endpoint":                         base + "/oauth/authorize",
		"token_endpoint":                                 base + "/oauth/token",
		"revocation_endpoint":                            base + "/oauth/revoke",
		"code_challenge_methods_supported":               []string{method},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"response_types_supported":                       []string{"code"},
		"authorization_response_iss_parameter_supported": !s.o.NoIssSupport,
	}
	if !s.o.NoDevice {
		m["device_authorization_endpoint"] = base + "/oauth/device_authorization"
		grants = append(grants, deviceGrant)
	}
	m["grant_types_supported"] = grants
	writeJSON(w, http.StatusOK, m)
}

// grantedTTL parses the ttl parameter, which is in seconds, and applies the
// server's default and maximum. The server never refuses a request for asking
// more than the maximum (R6).
func (s *Server) grantedTTL(raw string, record bool) (time.Duration, bool) {
	if raw == "" {
		return s.o.DefaultTTL, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if record {
		s.ttls = append(s.ttls, n)
	}
	return min(time.Duration(n)*time.Second, s.o.MaxTTL), true
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// An error here is a page, never a redirect: the redirect URI is not
	// trusted until it has passed (R3).
	redirect, challenge, state := r.FormValue("redirect_uri"), r.FormValue("code_challenge"), r.FormValue("state")
	switch {
	case r.FormValue("client_id") != clientID:
		errorPage(w, "Unknown client.")
		return
	case r.FormValue("response_type") != "code":
		errorPage(w, "response_type must be code.")
		return
	case !loopbackRedirect(redirect):
		errorPage(w, "The redirect URI must be http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback.")
		return
	case r.FormValue("code_challenge_method") != "S256" || !validChallenge(challenge):
		errorPage(w, "PKCE with the S256 method is required.")
		return
	case state == "":
		errorPage(w, "state is required.")
		return
	}
	ttl, ok := s.grantedTTL(r.FormValue("ttl"), r.Method == http.MethodGet)
	if !ok {
		errorPage(w, "ttl must be a positive number of seconds.")
		return
	}

	// Only a person at the Page form decides; under Auto and Deny a posted
	// decision is ignored.
	decision := ""
	switch {
	case s.o.Approval == Auto:
		decision = "allow"
	case s.o.Approval == Deny:
		decision = "deny"
	case r.Method == http.MethodPost:
		decision = r.FormValue("decision")
	}
	switch decision {
	case "allow":
		code := randomString(24)
		s.codes[code] = &grant{redirect: redirect, challenge: challenge, ttl: ttl, expires: time.Now().Add(codeLifetime)}
		s.callback(w, redirect, url.Values{"code": {code}, "state": {state}})
	case "deny":
		s.callback(w, redirect, url.Values{"error": {"access_denied"}, "state": {state}})
	default:
		hidden := ""
		for _, k := range []string{"client_id", "response_type", "redirect_uri", "code_challenge", "code_challenge_method", "state", "ttl"} {
			hidden += fmt.Sprintf("<input type=hidden name=\"%s\" value=\"%s\">", k, html.EscapeString(r.FormValue(k)))
		}
		page(w, http.StatusOK, "Brig CLI", fmt.Sprintf(
			"<p>Brig CLI wants to sign in to the fake account as fake@example.com for %s.</p>"+
				"<form method=post action=/oauth/authorize>%s"+
				"<button name=decision value=allow>Allow</button> <button name=decision value=deny>Deny</button></form>",
			ttl, hidden))
	}
}

func (s *Server) callback(w http.ResponseWriter, redirect string, q url.Values) {
	if iss := s.o.CallbackIss; iss != "-" {
		if iss == "" {
			iss = s.callbackIss()
		}
		q.Set("iss", iss)
	}
	u, _ := url.Parse(redirect)
	u.RawQuery = q.Encode()
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Only the body counts: a parameter in the query string is not a form.
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request")
		return
	}
	f := r.PostForm
	if f.Get("client_id") != clientID {
		oauthError(w, "invalid_client")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		// Single use: the code is gone after the first attempt, right or
		// wrong, so a retry with a corrected verifier fails too.
		g := s.codes[f.Get("code")]
		delete(s.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		v := f.Get("code_verifier")
		if g == nil || time.Now().After(g.expires) || len(v) < 43 || len(v) > 128 ||
			f.Get("redirect_uri") != g.redirect ||
			subtle.ConstantTimeCompare([]byte(want), []byte(g.challenge)) != 1 {
			oauthError(w, "invalid_grant")
			return
		}
		s.issue(w, g.ttl)
	case deviceGrant:
		s.pollDevice(w, f.Get("device_code"))
	default:
		oauthError(w, "unsupported_grant_type")
	}
}

func (s *Server) pollDevice(w http.ResponseWriter, code string) {
	d := s.devices[code]
	if d == nil {
		oauthError(w, "invalid_grant")
		return
	}
	now := time.Now()
	if now.After(d.expires) {
		delete(s.devices, code)
		oauthError(w, "expired_token")
		return
	}
	// RFC 8628 section 3.5: a client that polls faster than the interval is
	// told to slow down, and the interval grows by 5 s.
	tooFast := !d.lastPoll.IsZero() && now.Sub(d.lastPoll) < d.interval
	d.lastPoll = now
	if tooFast {
		d.interval += 5 * time.Second
		oauthError(w, "slow_down")
		return
	}
	if len(d.polls) > 0 {
		e := d.polls[0]
		d.polls = d.polls[1:]
		oauthError(w, e)
		return
	}
	switch d.state {
	case "approved":
		delete(s.devices, code)
		s.issue(w, d.ttl)
	case "denied":
		delete(s.devices, code)
		oauthError(w, "access_denied")
	default:
		oauthError(w, "authorization_pending")
	}
}

func (s *Server) issue(w http.ResponseWriter, ttl time.Duration) {
	tok := randomString(32)
	s.issued = append(s.issued, tok)
	s.tokens[tok] = true
	expires := int(ttl / time.Second)
	if s.o.ExpiresIn != nil {
		expires = *s.o.ExpiresIn
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": expires})
}

func (s *Server) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if s.o.NoDevice {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != clientID {
		oauthError(w, "invalid_client")
		return
	}
	ttl, ok := s.grantedTTL(r.PostForm.Get("ttl"), true)
	if !ok {
		oauthError(w, "invalid_request")
		return
	}
	d := &device{ttl: ttl, interval: s.o.Interval, polls: append([]string(nil), s.o.Polls...), state: "pending", expires: time.Now().Add(15 * time.Minute)}
	switch s.o.Approval {
	case Auto:
		d.state = "approved"
	case Deny:
		d.state = "denied"
	}
	code := randomString(32)
	userCode := make([]byte, 8)
	for i := range userCode {
		userCode[i] = userCodeAlphabet[randomInt(len(userCodeAlphabet))]
	}
	s.devices[code] = d
	s.userCodes[string(userCode)] = code
	uri := s.o.VerificationURI
	if uri == "" {
		uri = s.URL + "/device"
	}
	resp := map[string]any{
		"device_code":      code,
		"user_code":        string(userCode),
		"verification_uri": uri,
		"expires_in":       900,
		"interval":         int((s.o.Interval + time.Second - 1) / time.Second),
	}
	if c := s.o.VerificationURIComplete; c != "" {
		if strings.HasPrefix(c, "/") {
			c = s.URL + c
		}
		resp["verification_uri_complete"] = c
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) devicePage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && s.o.Approval == Page {
		code := strings.ToUpper(strings.ReplaceAll(r.FormValue("user_code"), "-", ""))
		d := s.devices[s.userCodes[code]]
		if d == nil {
			errorPage(w, "That code is not valid.")
			return
		}
		if r.FormValue("decision") == "allow" {
			d.state = "approved"
			page(w, http.StatusOK, "Brig CLI", "<p>Approved. You can close this page.</p>")
		} else {
			d.state = "denied"
			page(w, http.StatusOK, "Brig CLI", "<p>Denied. You can close this page.</p>")
		}
		return
	}
	page(w, http.StatusOK, "Brig CLI", fmt.Sprintf(
		"<p>Brig CLI wants to sign in to the fake account. Only continue if you just ran <code>brig login</code> yourself.</p>"+
			"<form method=post action=/device><label>Code <input name=user_code value=\"%s\"></label> "+
			"<button name=decision value=allow>Allow</button> <button name=decision value=deny>Deny</button></form>",
		html.EscapeString(r.FormValue("user_code"))))
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.o.RevokeStatus != 0 {
		w.WriteHeader(s.o.RevokeStatus)
		return
	}
	// RFC 7009: answer 200 for a token the server does not know, too.
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != clientID {
		oauthError(w, "invalid_client")
		return
	}
	tok := r.PostForm.Get("token")
	s.revoked = append(s.revoked, tok)
	delete(s.tokens, tok)
	w.WriteHeader(http.StatusOK)
}

// loopbackRedirect accepts only the RFC 8252 section 7.3 forms of R2.
func loopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" ||
		u.Path != "/callback" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "::1" {
		return false
	}
	p, err := strconv.Atoi(u.Port())
	return err == nil && p > 0 && p < 65536
}

func validChallenge(c string) bool {
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size && len(c) == 43
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func errorPage(w http.ResponseWriter, msg string) {
	page(w, http.StatusBadRequest, "Brig CLI: error", "<p>"+html.EscapeString(msg)+"</p>")
}

// page writes a static page with no script and no external resource.
func page(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><h1>%s</h1>%s", title, title, body)
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomInt(n int) int {
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		// Reject the tail so the modulo is unbiased.
		if int(b[0]) < 256/n*n {
			return int(b[0]) % n
		}
	}
}
