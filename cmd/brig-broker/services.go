package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/sessiontoken"
)

// This file is the registry of the guest services: what the broker serves a
// sandbox at brig.internal/<sandbox>/<kind> besides its routes. Each service
// signs its own tokens, with a key of its own, so a token of one service, or
// of a route, is refused by every other.

// serviceNonceSize is the length of the nonce drawn per registration.
const serviceNonceSize = 32

// errUnregistered is verifyService's refusal of a sandbox the service does
// not know, which is answered 404 and not 401: a guest must not tell it from
// a path of another sandbox.
var errUnregistered = errors.New("not registered")

// guestService is what a feature gives the broker to be served at
// brig.internal/<sandbox>/<kind>.
type guestService struct {
	kind      string // the path segment, and the name in status and refusals
	label     string // sessiontoken.SandboxKey label
	tokenType string // the typ its tokens carry
	hostKey   []byte // from loadOrCreateKey
	// same reports whether a new registration's data equals the old one,
	// so the nonce and the tokens minted under it survive.
	same func(prev, next any) bool
	// serve answers one request that passed every shared check.
	serve func(w http.ResponseWriter, r *http.Request, id, rest string, data any)
	// refused, if set, is told of each request a shared check refused, so
	// a probe or a replayed token leaves a trace in the service's audit.
	// arrived is the sandbox whose socket the request came in on, or "",
	// and id the one its path names. check is upgrade, arrival, sandbox or
	// token, and status what the guest was answered.
	// It is not handed the request, which carries the guest's token.
	refused func(arrived, id, rest, check string, status int)
}

// serviceReg is one sandbox's registration for a guest service.
type serviceReg struct {
	// internal is the sandbox's brig.internal address, or the zero Addr.
	internal netip.Addr
	// nonce is drawn per registration, so a sandbox registered again after
	// a removal refuses the tokens of the one before.
	nonce []byte
	// key signs the sandbox's tokens for the service, derived from the
	// service's host key, the sandbox id and nonce. It is read only under
	// s.mu: a removal clears it in place, so a reader outside the lock could
	// verify against a zero key. verifyService copies it.
	key  []byte
	data any
	// ctx is done when the registration is dropped. The requests served
	// under it end with it, as route requests end with their routes, since
	// the routes can hold the socket, and the guest's connection, open past
	// the revocation.
	ctx    context.Context
	cancel context.CancelFunc
}

// addService makes svc one the broker serves. A second service of one kind
// is a programming error, as is a kind that is not one path segment or that
// a path of the broker takes: the routes are at /<sandbox>/r/<route>, and the
// control socket removes a sandbox's services at /services/sandboxes/<id>.
func (s *server) addService(svc *guestService) {
	if svc.kind == "" || strings.Contains(svc.kind, "/") || svc.kind == "r" || svc.kind == "services" {
		panic(fmt.Sprintf("guest service kind %q is not one path segment of its own", svc.kind))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.services[svc.kind]; ok {
		panic("guest service " + svc.kind + " added twice")
	}
	s.services[svc.kind] = svc
	s.serviceRegs[svc.kind] = map[string]*serviceReg{}
}

// registerService registers sandbox id for the service of kind, at the
// brig.internal address internal. The same registration again changes
// nothing, and the tokens minted for it stay valid. The error is the
// refusal's text, with its HTTP status.
func (s *server) registerService(kind, id string, internal netip.Addr, data any) (injector.AddResponse, int, error) {
	if err := broker.ValidID(id); err != nil {
		return injector.AddResponse{}, http.StatusBadRequest, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return injector.AddResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s", injector.StoppingMessage)
	}
	svc, ok := s.services[kind]
	if !ok {
		return injector.AddResponse{}, http.StatusNotFound, fmt.Errorf("this broker serves no %s", kind)
	}
	if old, ok := s.serviceRegs[kind][id]; ok {
		// A sandbox that changed its data would reach the other data with a
		// token it already holds.
		if old.internal != internal {
			return injector.AddResponse{}, http.StatusConflict, fmt.Errorf(
				"sandbox %s is registered for %s at brig.internal %s, not %s",
				id, kind, addrName(old.internal), addrName(internal))
		}
		if !svc.same(old.data, data) {
			return injector.AddResponse{}, http.StatusConflict, fmt.Errorf(
				"sandbox %s is registered for %s with other settings", id, kind)
		}
		return s.serviceAdded(id), http.StatusOK, nil
	}
	// The routes and the guest services of a sandbox share its divert
	// socket, which serves one brig.internal address. Routes that do not
	// reach brig.internal are served on the loopback port, which a sandbox
	// with a brig.internal socket is refused, so they hold the address
	// "none" even without a socket. A socket here is held, since every
	// socket nothing holds is closed and dropped from s.diverts: an unheld
	// one at another address would be reused below at the wrong address.
	l := s.diverts[id]
	holder, at := "", netip.Addr{}
	if l != nil && l.internal != internal {
		if h := l.holders(); len(h) > 0 {
			holder, at = h[0], l.internal
		}
	} else if _, routes := s.sandboxes[id]; routes && l == nil && internal.IsValid() {
		holder = "routes"
	}
	if holder != "" {
		who := "the routes reach"
		if holder != "routes" {
			who = "its " + holder + " reaches"
		}
		return injector.AddResponse{}, http.StatusConflict, fmt.Errorf(
			"sandbox %s: %s brig.internal at %s, and its %s at %s; both must name the same address",
			id, who, addrName(at), kind, addrName(internal))
	}
	nonce := make([]byte, serviceNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return injector.AddResponse{}, http.StatusInternalServerError, err
	}
	key, err := sessiontoken.SandboxKey(svc.hostKey, svc.label, id, nonce)
	if err != nil {
		return injector.AddResponse{}, http.StatusInternalServerError, err
	}
	// Every service holds the socket, with or without an address, so the
	// socket outlives a change of the routes that share it.
	if l == nil {
		if l, err = s.openDivert(id, internal, false, nil, nil); err != nil {
			clear(key)
			return injector.AddResponse{}, http.StatusInternalServerError, fmt.Errorf("sandbox %s: %w", id, err)
		}
		s.diverts[id] = l
	}
	l.hold(kind)
	ctx, cancel := context.WithCancel(context.Background())
	s.serviceRegs[kind][id] = &serviceReg{internal: internal, nonce: nonce, key: key, data: data, ctx: ctx, cancel: cancel}
	return s.serviceAdded(id), http.StatusOK, nil
}

// serviceAdded is the answer to a service registration of sandbox id. s.mu
// is held. A sandbox that reaches brig.internal is handed the CA certificate
// to trust it by, and never its key.
func (s *server) serviceAdded(id string) injector.AddResponse {
	var resp injector.AddResponse
	if l := s.diverts[id]; l != nil {
		resp.DivertSocket = l.path
		if l.internal.IsValid() {
			resp.CA = string(s.ca.certPEM())
		}
	}
	return resp
}

// unregisterService drops the registration of sandbox id for the service of
// kind, and reports whether there was one. Its tokens fail from the next
// request on, since the key is gone and the next registration draws another
// nonce, and its requests in flight end. The divert socket closes when
// nothing else holds it.
func (s *server) unregisterService(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unregisterServiceLocked(kind, id)
}

// unregisterServiceLocked is unregisterService with s.mu held.
func (s *server) unregisterServiceLocked(kind, id string) bool {
	reg, ok := s.serviceRegs[kind][id]
	if !ok {
		return false
	}
	clear(reg.key)
	reg.cancel()
	delete(s.serviceRegs[kind], id)
	if l := s.diverts[id]; l != nil {
		l.release(kind)
		if !l.held() {
			l.close()
			delete(s.diverts, id)
		}
	}
	return true
}

// mintService returns a token of the service of kind for sandbox id, valid
// for ttl. A ttl that is not positive, or is longer than injector.TokenTTL,
// is injector.TokenTTL, as for a route's token.
func (s *server) mintService(kind, id, session string, ttl time.Duration) (string, error) {
	if err := broker.ValidID(id); err != nil {
		return "", err
	}
	if ttl <= 0 || ttl > injector.TokenTTL {
		ttl = injector.TokenTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[kind]
	if !ok {
		return "", fmt.Errorf("this broker serves no %s", kind)
	}
	reg, ok := s.serviceRegs[kind][id]
	if !ok {
		return "", fmt.Errorf("sandbox %s is not registered for %s", id, kind)
	}
	return sessiontoken.Mint(reg.key, sessiontoken.Claims{
		Sandbox: id,
		Session: session,
		Exp:     time.Now().Add(ttl).Unix(),
		Typ:     svc.tokenType,
	})
}

// verifyService checks token as one of the service of kind for sandbox id,
// and returns the registration's data and the context that ends with it.
// The key is copied under s.mu and the copy cleared after, so a removal that
// runs meanwhile never leaves this verifying against a cleared key.
func (s *server) verifyService(kind, id, token string, now time.Time) (any, context.Context, error) {
	s.mu.Lock()
	svc, ok := s.services[kind]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("this broker serves no %s", kind)
	}
	reg, ok := s.serviceRegs[kind][id]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("sandbox %s is %w for %s", id, errUnregistered, kind)
	}
	key, data, ctx := slices.Clone(reg.key), reg.data, reg.ctx
	s.mu.Unlock()
	defer clear(key)
	if _, err := sessiontoken.Verify(key, token, id, "", svc.tokenType, now); err != nil {
		return nil, nil, err
	}
	return data, ctx, nil
}

// internalMux is the handler of the brig.internal route server: the guest
// services at /<sandbox>/<kind>, and the route handler for everything else.
// The loopback route server has the route handler alone, so no service is
// reached there.
func (s *server) internalMux() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, kind, rest, ok := serviceTarget(r)
		var svc *guestService
		if ok {
			s.mu.Lock()
			svc = s.services[kind]
			s.mu.Unlock()
		}
		// A kind no service has is a path of the routes, as before services.
		if svc == nil {
			r, done := routesRequest(r)
			defer done()
			s.handler.ServeHTTP(w, r)
			return
		}
		s.serveService(w, r, svc, id, rest)
	})
}

// routesRequest is r with a context that ends when the routes of the socket r
// arrived on are replaced or removed, and its release. A guest service can
// hold the socket, and the guest's connection, open past the routes, so
// without it a route request in flight would outlive their revocation.
func routesRequest(r *http.Request) (*http.Request, func()) {
	l, ok := r.Context().Value(divertSocketKey{}).(*divertSandbox)
	if !ok {
		return r, func() {}
	}
	l.mu.Lock()
	routesCtx := l.routesCtx
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(routesCtx, cancel)
	return r.WithContext(ctx), func() { stop(); cancel() }
}

// serveService answers one request for svc after the checks every service
// shares. The arrival is checked before the registration is looked up, so a
// request on another sandbox's socket learns nothing of that sandbox.
func (s *server) serveService(w http.ResponseWriter, r *http.Request, svc *guestService, id, rest string) {
	// arrived only names the request's socket, for the refusal's trace: it
	// reads the remote address and looks nothing up for the guest.
	arrived, ok := s.arrival(id, r.RemoteAddr)
	refuse := func(check string, status int) {
		if svc.refused != nil {
			svc.refused(arrived, id, rest, check, status)
		}
	}
	// An upgrade or a CONNECT would hand the service a raw pipe, as it would
	// a route, so both are refused as the route handler refuses them: before
	// anything else, and before the token is read.
	if r.Method == http.MethodConnect || broker.Upgrade(r.Header) {
		http.Error(w, svc.kind+" does not take a protocol upgrade or CONNECT", http.StatusBadRequest)
		refuse("upgrade", http.StatusBadRequest)
		return
	}
	// A service is served on brig.internal alone. The link-local address is
	// clear text, and the loopback port names no sandbox.
	if a, _ := r.Context().Value(arrivalKey{}).(arrival); a != arrivalBrigInternal || !ok || arrived != id {
		http.NotFound(w, r)
		refuse("arrival", http.StatusNotFound)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = ""
	}
	data, regCtx, err := s.verifyService(svc.kind, id, token, time.Now())
	if errors.Is(err, errUnregistered) {
		// The answer to another sandbox's path, so the two read the same.
		http.NotFound(w, r)
		refuse("sandbox", http.StatusNotFound)
		return
	}
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "brig broker: the "+svc.kind+" token was refused", http.StatusUnauthorized)
		refuse("token", http.StatusUnauthorized)
		return
	}
	// A registration dropped while this runs ends it, as routesRequest ends
	// a route request with its routes.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(regCtx, cancel)
	defer stop()
	svc.serve(w, r.WithContext(ctx), id, rest, data)
}

// serviceTarget splits a request for a guest service at
// brig.internal/<id>/<kind>[/<rest>]. Whether a service of kind exists is
// the caller's to check. A host route's name is <route>.brig.internal and
// its path is the upstream's, so a service-looking path there belongs to the
// route.
func serviceTarget(r *http.Request) (id, kind, rest string, ok bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), "."+injector.BrigInternal) {
		return "", "", "", false
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/")
	if !ok {
		return "", "", "", false
	}
	id, path, ok = strings.Cut(path, "/")
	if !ok || id == "" {
		return "", "", "", false
	}
	kind, rest, _ = strings.Cut(path, "/")
	if kind == "" {
		return "", "", "", false
	}
	return id, kind, rest, true
}

// removeServices drops sandbox id from every guest service.
func (s *server) removeServices(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// One acquisition, so no registration lands between the removal and
	// the count.
	s.mu.Lock()
	removed := false
	for _, kind := range slices.Sorted(maps.Keys(s.services)) {
		if s.unregisterServiceLocked(kind, id) {
			removed = true
		}
	}
	remaining := s.registered()
	s.mu.Unlock()
	reply(w, injector.RemoveResponse{Removed: removed, Remaining: remaining})
}

// removeService drops sandbox id from the guest service of kind. The
// registry answers for every kind, so no feature has a removal path of its
// own to forget. An unknown kind and a sandbox not registered for kind are
// 404 with the messages ForgetService takes as nothing to revoke.
func (s *server) removeService(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// One acquisition, so no registration lands between the removal and
	// the count.
	s.mu.Lock()
	if _, ok := s.services[kind]; !ok {
		s.mu.Unlock()
		http.Error(w, injector.NoServiceMessage(kind), http.StatusNotFound)
		return
	}
	if !s.unregisterServiceLocked(kind, id) {
		s.mu.Unlock()
		http.Error(w, injector.NotRegisteredMessage(kind, id), http.StatusNotFound)
		return
	}
	remaining := s.registered()
	s.mu.Unlock()
	reply(w, injector.RemoveResponse{Removed: true, Remaining: remaining})
}

// registered counts the sandboxes registered for routes, for a guest service
// or for both: the broker is idle only when it serves none. s.mu is held.
func (s *server) registered() int {
	n := len(s.sandboxes)
	seen := map[string]struct{}{}
	for _, regs := range s.serviceRegs {
		for id := range regs {
			if _, ok := s.sandboxes[id]; ok {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				n++
			}
		}
	}
	return n
}

// serviceIDs names the sandboxes of each guest service, sorted, and leaves
// out a service with none. s.mu is held.
func (s *server) serviceIDs() map[string][]string {
	var out map[string][]string
	for kind, regs := range s.serviceRegs {
		if len(regs) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[kind] = slices.Sorted(maps.Keys(regs))
	}
	return out
}
