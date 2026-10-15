// Package broker is the credential broker of brokered routes: it verifies the
// session token a guest sends, applies the route's request.allow, injects the
// credential and forwards the request to the route's upstream.
//
// A guest holds a session token per route and no credential. A request names
// its sandbox and route in the path, /<sandbox-id>/r/<route>/<rest>, and the
// token proves both. The credential is resolved on the host through a
// secret.Broker for every request that passes, and never reaches the guest.
// docs/broker.md has the request pipeline, the error table, the token format
// and the audit line.
//
// The package serves HTTP. Terminating TLS for brig.internal, binding the
// sandbox gateway addresses and minting a token per exec belong to the
// process that hosts the Handler. That process also bounds what a guest can
// hold open: it sets ReadHeaderTimeout and IdleTimeout on its http.Server and
// limits the connections per sandbox. The upstream transport has no
// ResponseHeaderTimeout, because a model can take minutes before the first
// byte of an event stream.
package broker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/secret"
)

// Renewer is a secret.Broker that renews a credential the upstream refused.
// *renew.Cache implements it.
//
// It is optional. With it, an upstream 401 on a route with a credential
// lifecycle is answered by one renewal and one retry. Without it, the 401
// goes back to the guest.
type Renewer interface {
	// ForceRenew renews name once after the upstream refused stale, and
	// returns the document that replaces it.
	ForceRenew(ctx context.Context, name string, w secret.Workload, stale []byte) ([]byte, error)
}

// Config is what a Handler is built from.
type Config struct {
	// HostKey is required, and is at least 32 bytes. Each sandbox key is
	// derived from it, and it is kept in memory only.
	HostKey []byte
	// Credentials is required. It resolves the secret of a route for every
	// request that passes the checks.
	Credentials secret.Broker
	// Audit is optional. It takes one line per request. Without it, the
	// lines are dropped.
	Audit *slog.Logger
	// Now is optional, and defaults to time.Now.
	Now func() time.Time
	// Transport is optional. It carries the upstream requests, and defaults
	// to one that verifies upstream TLS with the system roots and uses no
	// proxy.
	Transport http.RoundTripper
	// ArrivalCheck is optional. It reports whether a request for sandbox
	// that arrived from remoteAddr came in on that sandbox's gateway. It
	// defaults to accepting every address, which fits a runtime where all
	// sandboxes share one gateway.
	ArrivalCheck func(sandbox, remoteAddr string) bool
	// Sandboxes is optional. Each one is added as by AddSandbox.
	Sandboxes []Sandbox
	// ReplayBudget is optional, and defaults to 64 MiB. It bounds the bytes
	// of request bodies held at once, across all requests, for a retry after
	// an upstream 401.
	ReplayBudget int64
}

// DefaultReplayBudget is the default of Config.ReplayBudget.
const DefaultReplayBudget = 64 << 20

// renewInterval is the shortest time between two renewals of one secret that
// upstream 401s force. Each renewal rotates the refresh token, and one that
// fails at the provider leaves the secret spent for every sandbox that shares
// it. An upstream that keeps answering 401 would otherwise force one per
// request. A document near its expiry needs no forced renewal: Resolve renews
// it ahead of time.
const renewInterval = time.Minute

// Sandbox is one sandbox that the broker serves, with its routes.
type Sandbox struct {
	// ID is required. It is the runtime's instance name, the first segment of
	// the request path.
	ID string
	// Profile is the canonical profile name of the sandbox, carried to the
	// secret.Broker and the audit line.
	Profile string
	// Routes is the route table of the sandbox. Route names are unique.
	Routes []Route
}

// Route is one route of a sandbox.
//
// Route must be a route that policy.ExpandRoute returned, and the broker does
// not validate it again. It depends on what Route.Validate guarantees: the
// upstream is a canonical https:// origin, a credential is named, inject
// carries the credential in headers.set or basicAuth, Authorization is in
// headers.remove, no header list names a header the broker owns, and
// request.allow.repos appears only on a github-token route.
type Route struct {
	// Policy is the name of the policy that declares the route. A refusal
	// names it.
	Policy string
	// Route is the expanded route.
	Route v1alpha1.Route
}

// Handler is the broker, an http.Handler for the paths of every sandbox it
// serves. It is safe for concurrent use.
type Handler struct {
	hostKey   []byte
	creds     secret.Broker
	renewer   Renewer
	audit     *slog.Logger
	now       func() time.Time
	transport http.RoundTripper
	arrival   func(sandbox, remoteAddr string) bool
	replay    *budget

	mu        sync.RWMutex
	sandboxes map[string]*sandbox

	renewMu sync.Mutex
	// forced is when an upstream 401 last forced a renewal, per secret.
	forced map[string]time.Time
}

// sandbox is a registered sandbox, ready to serve.
type sandbox struct {
	id      string
	profile string
	key     []byte
	routes  map[string]*route
}

// route is a registered route with its upstream parsed.
type route struct {
	policy   string
	spec     v1alpha1.Route
	upstream *url.URL
}

// New returns a Handler built from cfg.
func New(cfg Config) (*Handler, error) {
	if len(cfg.HostKey) < minHostKey {
		return nil, fmt.Errorf("the broker host key is %d bytes; it needs at least %d", len(cfg.HostKey), minHostKey)
	}
	if cfg.Credentials == nil {
		return nil, errors.New("the broker needs a secret.Broker to resolve credentials")
	}
	h := &Handler{
		hostKey:   append([]byte(nil), cfg.HostKey...),
		creds:     cfg.Credentials,
		audit:     cfg.Audit,
		now:       cfg.Now,
		transport: cfg.Transport,
		arrival:   cfg.ArrivalCheck,
		sandboxes: map[string]*sandbox{},
		forced:    map[string]time.Time{},
	}
	if cfg.ReplayBudget < 0 {
		return nil, fmt.Errorf("the broker replay budget is %d bytes; it cannot be negative", cfg.ReplayBudget)
	}
	h.replay = &budget{left: cfg.ReplayBudget}
	if cfg.ReplayBudget == 0 {
		h.replay.left = DefaultReplayBudget
	}
	h.renewer, _ = cfg.Credentials.(Renewer)
	if h.audit == nil {
		h.audit = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.transport == nil {
		h.transport = defaultTransport()
	}
	if h.arrival == nil {
		h.arrival = func(string, string) bool { return true }
	}
	for _, s := range cfg.Sandboxes {
		if err := h.AddSandbox(s); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// defaultTransport returns the upstream transport: system roots, full
// verification, and no proxy from the environment. An HTTPS_PROXY set on the
// host would otherwise see every upstream request the broker makes.
func defaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

// AddSandbox registers s, or replaces the sandbox registered under s.ID.
//
// Each registration takes a new sandbox key. A replaced sandbox refuses the
// tokens minted for the one it replaces, so the caller mints again.
func (h *Handler) AddSandbox(s Sandbox) error {
	if s.ID == "" || strings.ContainsAny(s.ID, "/%") {
		return fmt.Errorf("sandbox id %q is empty or holds a slash or a percent sign", s.ID)
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key, err := sandboxKey(h.hostKey, s.ID, nonce)
	if err != nil {
		return err
	}
	sb := &sandbox{id: s.ID, profile: s.Profile, key: key, routes: map[string]*route{}}
	for _, r := range s.Routes {
		name := r.Route.Name
		if _, dup := sb.routes[name]; dup {
			return fmt.Errorf("sandbox %s: route %s is registered twice", s.ID, name)
		}
		if r.Route.Credential == nil || r.Route.Inject == nil {
			return fmt.Errorf("sandbox %s: route %s is not expanded; pass it through policy.ExpandRoute", s.ID, name)
		}
		u, err := url.Parse(r.Route.Upstream)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("sandbox %s: route %s: upstream %q is not an https:// origin", s.ID, name, r.Route.Upstream)
		}
		sb.routes[name] = &route{policy: r.Policy, spec: *r.Route.DeepCopy(), upstream: u}
	}
	h.mu.Lock()
	h.sandboxes[s.ID] = sb
	h.mu.Unlock()
	return nil
}

// RemoveSandbox stops serving the sandbox id. Its tokens are refused from the
// next request on: with 404 while the id is not registered, and with 401
// after a sandbox is added again under the same id.
func (h *Handler) RemoveSandbox(id string) {
	h.mu.Lock()
	delete(h.sandboxes, id)
	h.mu.Unlock()
}

func (h *Handler) sandbox(id string) *sandbox {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sandboxes[id]
}

// workload returns the identity of a request for the audit line and the
// secret.Broker.
func (sb *sandbox) workload(session string) secret.Workload {
	return secret.Workload{Sandbox: sb.id, Profile: sb.profile, Session: session}
}

// workloadID renders w as the SPIFFE ID that names it on the audit line. The
// default session has no session segment.
func workloadID(w secret.Workload) string {
	id := "spiffe://brig.local/brig/profile/" + w.Profile
	if w.Session != "" {
		id += "/session/" + w.Session
	}
	return id
}

// mayForceRenew reports whether an upstream 401 may force a renewal of secret
// now, and records the attempt when it may.
func (h *Handler) mayForceRenew(secret string) bool {
	now := h.now()
	h.renewMu.Lock()
	defer h.renewMu.Unlock()
	if last, ok := h.forced[secret]; ok && now.Sub(last) < renewInterval {
		return false
	}
	h.forced[secret] = now
	return true
}

// budget is a count of bytes that callers take and give back.
type budget struct {
	mu   sync.Mutex
	left int64
}

// take takes n bytes, and reports whether there were n left.
func (b *budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

func (b *budget) give(n int64) {
	b.mu.Lock()
	b.left += n
	b.mu.Unlock()
}
