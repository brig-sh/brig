package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/mcpproxy"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/sessiontoken"
)

// The MCP servers have their own key, sandbox keys, endpoints and state, as
// memory does. They share the process, the control socket and the divert
// socket with the routes and memory, so they can move to their own binary
// without the keys of either.
const (
	// mcpTokenType is the typ claim of an MCP token. A route checks for
	// brig-route and memory for brig-memory, so an MCP token is refused
	// there, and theirs here.
	mcpTokenType = "brig-mcp"
	// mcpLabel is the key-derivation label of an MCP token.
	mcpLabel = "brig-mcp"
	// mcpNonceSize is the length of the registration nonce, as for memory.
	mcpNonceSize = 16
)

// maxMCPPerSandbox bounds the MCP calls of one sandbox in flight at once.
// A slot is held while the answer streams, for as long as a tool runs, so
// there is no bound over all sandboxes: a few busy sandboxes would hold it
// and refuse every other. A call past the bound gets 503 at once and is not
// queued.
const maxMCPPerSandbox = 8

// mcpCallTimeout bounds one MCP call, the streamed answer included, so a
// tool that never finishes, or a guest that stops reading, cannot hold a
// slot and a broker goroutine for ever. A variable so a test can shorten it.
var mcpCallTimeout = 10 * time.Minute

// beforeMCPRead runs as serveMCP starts to read a request's body. A test
// waits on it to know the request is past the token check.
var beforeMCPRead = func() {}

// mcpGate counts the MCP calls in flight per sandbox. Unlike memoryGate it
// has no total.
type mcpGate struct {
	mu  sync.Mutex
	per map[string]int
}

// acquire takes a slot for sandbox id, and reports false at the bound.
func (g *mcpGate) acquire(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.per[id] >= maxMCPPerSandbox {
		return false
	}
	if g.per == nil {
		g.per = map[string]int{}
	}
	g.per[id]++
	return true
}

// release gives back a slot acquire took.
func (g *mcpGate) release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.per[id] == 0 {
		return
	}
	if g.per[id]--; g.per[id] == 0 {
		delete(g.per, id)
	}
}

// mcpReg is the MCP registration of one sandbox.
type mcpReg struct {
	// servers are the sandbox's servers, expanded, by name.
	servers map[string]v1alpha1.MCPServer
	// nonce is drawn at the first registration and kept while the servers
	// are the same, so a token minted before a second registration stays
	// valid.
	nonce []byte
	// key is the HMAC key of the sandbox's MCP tokens, derived from the MCP
	// key, the sandbox id and nonce.
	key []byte
	// internal is the address the sandbox's guest resolves brig.internal
	// to. It is always valid: MCP is served there alone.
	internal netip.Addr
	// workload is who the credentials are resolved for.
	workload secret.Workload
}

// newMCPProxy is the proxy of every MCP server of the broker. transport is
// the test's upstream, or nil for the proxy's own. The proxy never follows a
// redirect: it sends one round trip, and a redirect would carry the
// credential to wherever the upstream named.
//
// The transport has no ResponseHeaderTimeout, as for routes: a tools/call
// answered as application/json sends its headers only when the tool is done.
// mcpCallTimeout bounds a stalled upstream instead.
func (s *server) newMCPProxy(transport http.RoundTripper) *mcpproxy.Proxy {
	if transport == nil {
		transport = mcpproxy.DefaultTransport()
	}
	return &mcpproxy.Proxy{
		Credentials: s.creds,
		Transport:   transport,
		Audit:       s.writeMCPRecord,
		// The guest is told the server is unreachable, and the reason, which
		// can name host addresses, goes to the broker log alone.
		Log: func(server string, err error) {
			s.logger.Printf("mcp server %s is unreachable: %v", clip(server, maxAuditServer), err)
		},
	}
}

// expandRegistered expands a server a registration carries. brig sends it
// expanded, and a preset server then carries the preset's url and inject,
// which ExpandMCP refuses as the policy's own. They are dropped and the
// preset applied again; a url or inject that is not the preset's is refused,
// so a registration cannot send a preset's credential anywhere else.
func expandRegistered(s v1alpha1.MCPServer) (v1alpha1.MCPServer, error) {
	if s.Preset == "" {
		return policy.ExpandMCP(s)
	}
	bare := *s.DeepCopy()
	bare.URL, bare.Inject = "", nil
	e, err := policy.ExpandMCP(bare)
	if err != nil {
		return v1alpha1.MCPServer{}, err
	}
	if (s.URL != "" && s.URL != e.URL) || (s.Inject != nil && !reflect.DeepEqual(s.Inject, e.Inject)) {
		return v1alpha1.MCPServer{}, fmt.Errorf("mcp %s: the %s preset sets url and inject, and the registration "+
			"names others", s.Name, s.Preset)
	}
	return e, nil
}

// addMCP registers the MCP servers of a sandbox. Each server is expanded
// again here, as routes are: the broker does not rely on brig's policy
// check.
func (s *server) addMCP(w http.ResponseWriter, r *http.Request) {
	var req injector.MCPSandbox
	if !decode(w, r, &req) {
		return
	}
	if err := broker.ValidID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	internal, err := brigInternalAddr(req.BrigInternal)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
		return
	}
	// The loopback port names no sandbox and the link-local address is
	// clear text, so MCP is served on brig.internal or not at all.
	if !internal.IsValid() {
		http.Error(w, fmt.Sprintf("sandbox %s: mcp servers are served on brig.internal alone, and the sandbox "+
			"does not reach it", req.ID), http.StatusBadRequest)
		return
	}
	if len(req.Servers) == 0 {
		http.Error(w, fmt.Sprintf("sandbox %s: no mcp servers to register", req.ID), http.StatusBadRequest)
		return
	}
	servers := map[string]v1alpha1.MCPServer{}
	for _, sv := range req.Servers {
		e, err := expandRegistered(sv.Server)
		if err != nil {
			http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
			return
		}
		if _, dup := servers[e.Name]; dup {
			http.Error(w, fmt.Sprintf("sandbox %s: mcp server %s is registered twice", req.ID, e.Name),
				http.StatusBadRequest)
			return
		}
		servers[e.Name] = e
	}
	workload := secret.Workload{Sandbox: req.ID, Profile: req.Profile}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	old, registered := s.mcps[req.ID]
	if registered && old.internal != internal {
		http.Error(w, fmt.Sprintf("sandbox %s is registered for mcp at brig.internal %s, not %s",
			req.ID, old.internal, internal), http.StatusConflict)
		return
	}
	if registered && old.workload == workload && reflect.DeepEqual(old.servers, servers) {
		reply(w, s.mcpAdded(req.ID))
		return
	}
	// The routes, the memory and the MCP servers of a sandbox share its
	// divert socket, which serves one brig.internal address. Routes that do
	// not reach brig.internal are served on the loopback port, which a
	// sandbox with a brig.internal socket is refused.
	l := s.diverts[req.ID]
	if _, ok := s.sandboxes[req.ID]; ok {
		var routes netip.Addr
		if l != nil {
			routes = l.internal
		}
		if routes != internal {
			http.Error(w, fmt.Sprintf("sandbox %s: its mcp servers reach brig.internal at %s, and its routes at %s; "+
				"both must name the same address", req.ID, internal, addrName(routes)), http.StatusConflict)
			return
		}
	}
	if m, ok := s.memories[req.ID]; ok && m.internal.IsValid() && m.internal != internal {
		http.Error(w, fmt.Sprintf("sandbox %s: its mcp servers reach brig.internal at %s, and its memory at %s; "+
			"both must name the same address", req.ID, internal, m.internal), http.StatusConflict)
		return
	}
	// Another set of servers is another registration, under another nonce,
	// as another set of routes is: a token minted for the old set does not
	// reach the new one.
	nonce := make([]byte, mcpNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key, err := sessiontoken.SandboxKey(s.mcpKey, mcpLabel, req.ID, nonce)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if l == nil {
		if l, err = s.openDivert(req.ID, internal, false, nil, nil); err != nil {
			clear(key)
			http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusInternalServerError)
			return
		}
		s.diverts[req.ID] = l
	}
	l.mcp = true
	if registered {
		clear(old.key)
	}
	s.mcps[req.ID] = mcpReg{servers: servers, nonce: nonce, key: key, internal: internal, workload: workload}
	reply(w, s.mcpAdded(req.ID))
}

// mcpAdded is the answer to an MCP registration of sandbox id. s.mu is held.
// The sandbox is handed its divert socket and the CA certificate to trust it
// by, and never its key.
func (s *server) mcpAdded(id string) injector.AddResponse {
	var resp injector.AddResponse
	if l := s.diverts[id]; l != nil && l.mcp {
		resp.DivertSocket = l.path
		resp.CA = string(s.ca.certPEM())
	}
	return resp
}

// removeMCP drops the MCP servers of a sandbox. Its MCP tokens fail from the
// next request on, because the next registration draws another nonce.
func (s *server) removeMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	reg, removed := s.mcps[id]
	if removed {
		clear(reg.key)
		delete(s.mcps, id)
	}
	// A socket the sandbox's routes or memory hold stays for them.
	if l := s.diverts[id]; l != nil && l.mcp {
		l.mcp = false
		if !l.servesRoutes() && !l.memory {
			l.close()
			delete(s.diverts, id)
		}
	}
	remaining := s.registered()
	s.mu.Unlock()
	reply(w, injector.RemoveResponse{Removed: removed, Remaining: remaining})
}

// mintMCP returns an MCP token for a sandbox registered for MCP servers.
func (s *server) mintMCP(w http.ResponseWriter, r *http.Request) {
	var req injector.MCPMintRequest
	if !decode(w, r, &req) {
		return
	}
	if err := broker.ValidID(req.Sandbox); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > injector.TokenTTL {
		ttl = injector.TokenTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	reg, ok := s.mcps[req.Sandbox]
	if !ok {
		http.Error(w, fmt.Sprintf("sandbox %q is not registered for mcp", req.Sandbox), http.StatusNotFound)
		return
	}
	token, err := sessiontoken.Mint(reg.key, sessiontoken.Claims{
		Sandbox: req.Sandbox,
		Session: req.Session,
		Exp:     time.Now().Add(ttl).Unix(),
		Typ:     mcpTokenType,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply(w, injector.MintResponse{Token: token})
}

// mcpPath returns the sandbox and the server name of a path under
// /<id>/mcp/. Everything under it is MCP's, so a name with a slash is an
// unknown server and never falls through to the route handler.
func mcpPath(path string) (id, name string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return "", "", false
	}
	id, rest, ok = strings.Cut(rest, "/")
	if !ok || id == "" {
		return "", "", false
	}
	name, ok = strings.CutPrefix(rest, "mcp/")
	return id, name, ok
}

// maxAuditServer bounds the server name on an MCP audit line. The name of a
// refused request comes from the guest's path.
const maxAuditServer = 64

// mcpAudit is the audit line of one MCP request refused before it reached
// the proxy. It names the sandbox the request arrived on, the sandbox and
// server its path names, clipped, and why it was refused.
type mcpAudit struct {
	sandbox string
	path    string
	server  string
	// check names why the request was refused: arrival, sandbox, token,
	// server, read or busy.
	check string
}

// serveMCP serves the MCP server name of sandbox id. The request must have
// arrived over brig.internal on the sandbox's own divert socket, and carry an
// MCP token of the sandbox; the checks and their answers are memory's. Then
// the proxy answers it, and writes its own audit line.
//
// Nothing here recovers a panic: the proxy aborts a stream it can no longer
// judge with http.ErrAbortHandler, and net/http must see it to break the
// guest's connection rather than end the stream cleanly.
func (s *server) serveMCP(w http.ResponseWriter, r *http.Request, id, name string) {
	start := time.Now()
	arrived, ok := s.arrival(id, r.RemoteAddr)
	a := &mcpAudit{sandbox: arrived, path: clip(id, maxAuditPath), server: clip(name, maxAuditServer)}
	refuse := func(check string, status int) {
		a.check = check
		s.writeMCPAudit(a, status, time.Since(start))
	}

	// MCP is served on brig.internal alone. The link-local address is clear
	// text, and the loopback port names no sandbox.
	if arr, _ := r.Context().Value(arrivalKey{}).(arrival); arr != arrivalBrigInternal || !ok || arrived != id {
		http.NotFound(w, r)
		refuse("arrival", http.StatusNotFound)
		return
	}
	// The key is copied under s.mu: a removal clears it in place.
	s.mu.Lock()
	reg, ok := s.mcps[id]
	key, servers, workload := slices.Clone(reg.key), reg.servers, reg.workload
	s.mu.Unlock()
	defer clear(key)
	if !ok {
		http.NotFound(w, r)
		refuse("sandbox", http.StatusNotFound)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = ""
	}
	if _, err := sessiontoken.Verify(key, token, id, "", mcpTokenType, time.Now()); token == "" || err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "invalid mcp token", http.StatusUnauthorized)
		refuse("token", http.StatusUnauthorized)
		return
	}
	// The name is looked up after the token, so a guest without one cannot
	// learn which servers the sandbox has.
	srv, ok := servers[name]
	if !ok {
		http.NotFound(w, r)
		refuse("server", http.StatusNotFound)
		return
	}
	// The body is read before a slot is taken, under memory's deadline, so
	// a guest that trickles its body holds no slot; the deadline is cleared
	// after a read within the bound, so the next request on a kept-alive
	// connection, and a long upstream answer, are not cut by it. One byte
	// past the bound lets the proxy answer 413.
	beforeMCPRead()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(memoryReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpproxy.MaxRequest+1))
	if err != nil {
		// The deadline stays: the server would otherwise wait on the rest
		// of the body before it sends this answer.
		http.Error(w, "cannot read request", http.StatusBadRequest)
		refuse("read", http.StatusBadRequest)
		return
	}
	if len(body) <= mcpproxy.MaxRequest {
		_ = rc.SetReadDeadline(time.Time{})
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	// The slot is held while the proxy streams the answer, and given back as
	// an aborted stream unwinds.
	if !s.mcpGate.acquire(id) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many mcp requests at once; retry", http.StatusServiceUnavailable)
		refuse("busy", http.StatusServiceUnavailable)
		return
	}
	defer s.mcpGate.release(id)
	ctx, cancel := context.WithTimeout(r.Context(), mcpCallTimeout)
	defer cancel()
	// The context stops the upstream, and not a write to a guest that
	// stopped reading: once the socket is full that write blocks until the
	// guest closes. The write deadline cuts it at the same time; closing
	// the connection then gives crypto/tls's close_notify up to 5 s more.
	// Its error is ignored, as for the read deadline: the brig.internal
	// server's connections support it.
	_ = rc.SetWriteDeadline(time.Now().Add(mcpCallTimeout))
	s.mcpProxy.Serve(w, r.WithContext(ctx), mcpproxy.Target{Sandbox: workload, Server: srv})
}

// writeMCPAudit writes the line of one MCP request refused before the proxy.
func (s *server) writeMCPAudit(a *mcpAudit, status int, dur time.Duration) {
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "broker",
		slog.String("sandbox", a.sandbox), slog.String("path", a.path), slog.String("kind", "mcp"),
		slog.String("server", a.server), slog.Int("status", status), slog.Duration("dur", dur),
		slog.String("check", a.check))
}

// writeMCPRecord writes the line of one exchange the proxy answered. It
// never holds arguments, results, header values or the credential.
//
// The method and the tool come from the guest, and the server from the
// policy. The text handler quotes any value with a space, a quote, an equals
// sign or a control character, so none of them can end the line or add a
// field to it; each is clipped, so none can grow it.
func (s *server) writeMCPRecord(rec mcpproxy.Record) {
	attrs := []slog.Attr{slog.String("sandbox", rec.Sandbox), slog.String("path", clip(rec.Sandbox, maxAuditPath)),
		slog.String("kind", "mcp"), slog.String("server", clip(rec.Server, maxAuditServer)),
		slog.String("method", clip(rec.Method, maxAuditRPCMethod))}
	if rec.Tool != "" {
		attrs = append(attrs, slog.String("tool", clip(rec.Tool, maxAuditTool)))
	}
	attrs = append(attrs, slog.String("verdict", string(rec.Verdict)), slog.Int("status", rec.Status),
		slog.Duration("dur", rec.Duration))
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "broker", attrs...)
}
