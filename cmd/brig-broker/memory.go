package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/sessiontoken"
)

// Memory has its own key, sandbox keys, endpoints and state. It shares only
// the process and the control socket with the routes, so it can move to its
// own binary without the host key that signs the route tokens.
const (
	// memoryTokenType is the typ claim of a memory token. A route checks
	// for brig-route, so a memory token is refused there, and the reverse.
	memoryTokenType = "brig-memory"
	// memoryLabel is the key-derivation label of a memory token.
	memoryLabel = "brig-memory"
	// memoryNonceSize is the length of the registration nonce, as for a
	// route sandbox.
	memoryNonceSize = 16
)

// The bounds of the memory requests the broker serves at once. Each request
// reads store files into the heap of the process that holds the credentials,
// so a guest that loops over memory_search must not grow it without limit.
// Past a bound the request gets 503 at once and is not queued, so it holds no
// connection slot.
const (
	maxMemoryPerSandbox = 4
	maxMemoryInFlight   = 16
	// memoryCallTimeout bounds one store call, so a slow disk cannot hold
	// a slot for ever.
	memoryCallTimeout = 10 * time.Second
)

// memoryReadTimeout bounds the read of one memory request's body, which
// happens before a slot is taken: a guest that trickles its body must not
// hold a connection open for ever. A variable so a test can shorten it.
var memoryReadTimeout = memoryCallTimeout

// memoryGate counts the memory requests in flight, per sandbox and in all.
type memoryGate struct {
	mu    sync.Mutex
	total int
	per   map[string]int
}

// acquire takes a slot for sandbox id, and reports false when either bound
// is reached.
func (g *memoryGate) acquire(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= maxMemoryInFlight || g.per[id] >= maxMemoryPerSandbox {
		return false
	}
	if g.per == nil {
		g.per = map[string]int{}
	}
	g.total++
	g.per[id]++
	return true
}

// release gives back a slot acquire took.
func (g *memoryGate) release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.per[id] == 0 {
		return
	}
	g.total--
	if g.per[id]--; g.per[id] == 0 {
		delete(g.per, id)
	}
}

// memoryReg is the memory registration of one sandbox.
type memoryReg struct {
	// store is the store key the sandbox reads and writes.
	store string
	// nonce is drawn at the first registration and kept while the store is
	// the same, so a token minted before a second registration stays valid.
	nonce []byte
	// key is the HMAC key of the sandbox's memory tokens, derived from the
	// memory key, the sandbox id and nonce.
	key []byte
	// internal is the address the sandbox's guest resolves brig.internal
	// to, or the zero Addr for a sandbox that does not reach it.
	internal netip.Addr
}

// validMemoryKey accepts a store name a caller picks, or a project key.
// Anything else is refused before a path is built from it.
func validMemoryKey(key string) error {
	if rest, ok := strings.CutPrefix(key, "project-"); ok {
		if len(rest) == 16 && strings.Trim(rest, "0123456789abcdef") == "" {
			return nil
		}
		return fmt.Errorf("%q is not a project key: it must be project- and 16 lowercase hex digits", key)
	}
	return memory.ValidStoreName(key)
}

// addMemory registers the memory of a sandbox. The store is opened once, so
// a store that cannot open fails the run at its start and not at the agent's
// first call.
func (s *server) addMemory(w http.ResponseWriter, r *http.Request) {
	var req injector.MemorySandbox
	if !decode(w, r, &req) {
		return
	}
	if err := broker.ValidID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validMemoryKey(req.Key); err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
		return
	}
	// A project store comes with the folder its key was made from, and a
	// named store with none, so the folder recorded for `brig memory ls`
	// is always the store's own.
	if isProject := strings.HasPrefix(req.Key, "project-"); isProject != (req.Project != "") {
		http.Error(w, fmt.Sprintf("sandbox %s: a project store needs its folder, and a named store has none",
			req.ID), http.StatusBadRequest)
		return
	} else if isProject && (!filepath.IsAbs(req.Project) || memory.ProjectKey(req.Project) != req.Key) {
		http.Error(w, fmt.Sprintf("sandbox %s: %s is not the folder of %s", req.ID, req.Project, req.Key),
			http.StatusBadRequest)
		return
	}
	internal, err := brigInternalAddr(req.BrigInternal)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
		return
	}
	if s.cfg.memoryDir == "" {
		http.Error(w, "this broker has no memory directory", http.StatusInternalServerError)
		return
	}
	// The store is opened without s.mu: a slow disk would hold every
	// control call behind it. The control socket is brig's alone, so the
	// error, with its host path, goes back to brig and never to a guest.
	f, err := memory.OpenFiles(s.cfg.memoryDir, req.Key)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusInternalServerError)
		return
	}
	if req.Project != "" {
		err = f.SetProject(req.Project)
	}
	_ = f.Close()
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	if old, ok := s.memories[req.ID]; ok {
		// The run path refuses a second store under one id before it gets
		// here. A sandbox that changed stores would read the other one's
		// memories with a token it already holds.
		if old.store != req.Key {
			http.Error(w, fmt.Sprintf("sandbox %s is registered for memory with another store", req.ID),
				http.StatusConflict)
			return
		}
		if old.internal != internal {
			http.Error(w, fmt.Sprintf("sandbox %s is registered for memory at brig.internal %s, not %s",
				req.ID, addrName(old.internal), addrName(internal)), http.StatusConflict)
			return
		}
		reply(w, s.memoryAdded(req.ID))
		return
	}
	// The routes and the memory of a sandbox share its divert socket, which
	// serves one brig.internal address. Routes that do not reach
	// brig.internal are served on the loopback port, which a sandbox with a
	// brig.internal socket is refused.
	l := s.diverts[req.ID]
	if internal.IsValid() {
		if _, ok := s.sandboxes[req.ID]; ok {
			var routes netip.Addr
			if l != nil {
				routes = l.internal
			}
			if routes != internal {
				http.Error(w, fmt.Sprintf("sandbox %s: its memory reaches brig.internal at %s, and its routes at %s; "+
					"both must name the same address", req.ID, internal, addrName(routes)), http.StatusConflict)
				return
			}
		}
	}
	nonce := make([]byte, memoryNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key, err := sessiontoken.SandboxKey(s.memoryKey, memoryLabel, req.ID, nonce)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if internal.IsValid() {
		if l == nil {
			if l, err = s.openDivert(req.ID, internal, false, nil, nil); err != nil {
				clear(key)
				http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusInternalServerError)
				return
			}
			s.diverts[req.ID] = l
		}
		l.memory = true
	}
	s.memories[req.ID] = memoryReg{store: req.Key, nonce: nonce, key: key, internal: internal}
	reply(w, s.memoryAdded(req.ID))
}

// memoryAdded is the answer to a memory registration of sandbox id. s.mu is
// held. A sandbox whose memory reaches brig.internal is handed its divert
// socket and the CA certificate to trust it by, and never its key.
func (s *server) memoryAdded(id string) injector.AddResponse {
	var resp injector.AddResponse
	if l := s.diverts[id]; l != nil && l.memory {
		resp.DivertSocket = l.path
		resp.CA = string(s.ca.certPEM())
	}
	return resp
}

// removeMemory drops the memory of a sandbox. Its memory tokens fail from
// the next request on, because the next registration draws another nonce.
func (s *server) removeMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	reg, removed := s.memories[id]
	if removed {
		clear(reg.key)
		delete(s.memories, id)
	}
	// A socket the sandbox's routes hold stays for them.
	if l := s.diverts[id]; l != nil && l.memory {
		l.memory = false
		if !l.servesRoutes() {
			l.close()
			delete(s.diverts, id)
		}
	}
	remaining := s.registered()
	s.mu.Unlock()
	reply(w, injector.RemoveResponse{Removed: removed, Remaining: remaining})
}

// mintMemory returns a memory token for a sandbox registered for memory.
func (s *server) mintMemory(w http.ResponseWriter, r *http.Request) {
	var req injector.MemoryMintRequest
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
	reg, ok := s.memories[req.Sandbox]
	if !ok {
		http.Error(w, fmt.Sprintf("sandbox %q is not registered for memory", req.Sandbox), http.StatusNotFound)
		return
	}
	token, err := sessiontoken.Mint(reg.key, sessiontoken.Claims{
		Sandbox: req.Sandbox,
		Session: req.Session,
		Exp:     time.Now().Add(ttl).Unix(),
		Typ:     memoryTokenType,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply(w, injector.MintResponse{Token: token})
}

// registered counts the sandboxes registered for routes, for memory or for
// both: the broker is idle only when it serves neither. s.mu is held.
func (s *server) registered() int {
	n := len(s.sandboxes)
	for id := range s.memories {
		if _, ok := s.sandboxes[id]; !ok {
			n++
		}
	}
	return n
}

// internalMux is the handler of the brig.internal route server: the memory
// of a sandbox at /<id>/memory, and the route handler for everything else.
// The loopback route server has the route handler alone, so memory is never
// reached there.
func (s *server) internalMux() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := memoryTarget(r); ok {
			s.serveMemory(w, r, id)
			return
		}
		s.handler.ServeHTTP(w, r)
	})
}

// memoryTarget returns the sandbox of a memory request. Memory is served at
// brig.internal itself. A host route's name is <route>.brig.internal and its
// path is the upstream's, so a /<x>/memory path there belongs to the route.
func memoryTarget(r *http.Request) (string, bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), "."+injector.BrigInternal) {
		return "", false
	}
	return memoryPath(r.URL.Path)
}

// memoryPath returns the sandbox of the path /<id>/memory. Nothing below it
// is memory.
func memoryPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return "", false
	}
	id, tail, ok := strings.Cut(rest, "/")
	return id, ok && id != "" && tail == "memory"
}

// The bounds of the guest's words on a memory audit line.
const (
	maxAuditRPCMethod = 64
	maxAuditTool      = 64
	maxAuditPath      = 64
)

// memoryAudit is the audit line of one memory request. It names the
// JSON-RPC method, the tool and the memory, and never the body, the
// description, the query or the token.
//
// sandbox is the sandbox whose socket the request arrived on, and path the
// one its path names, clipped. The path comes from the guest, so it is not
// used as the sandbox: otherwise one sandbox's guest could write lines in
// another's name.
type memoryAudit struct {
	sandbox string
	path    string
	// check names why the request was refused: arrival, sandbox, token or
	// busy.
	check  string
	method string
	tool   string
	name   string
}

// serveMemory serves the memory of sandbox id. The request must have
// arrived over brig.internal on the sandbox's own divert socket, and carry a
// memory token of the sandbox. Every refusal is one line with no host path:
// the store's errors name host paths, and go to the broker log alone.
func (s *server) serveMemory(w http.ResponseWriter, r *http.Request, id string) {
	start := time.Now()
	arrived, ok := s.arrival(id, r.RemoteAddr)
	a := &memoryAudit{sandbox: arrived, path: clip(id, maxAuditPath)}
	sw := &statusWriter{ResponseWriter: w}
	defer func() { s.writeMemoryAudit(a, sw.code(), time.Since(start)) }()

	// Memory is served on brig.internal alone. The link-local address is
	// clear text, and the loopback port names no sandbox.
	if arr, _ := r.Context().Value(arrivalKey{}).(arrival); arr != arrivalBrigInternal {
		a.check = "arrival"
		http.NotFound(sw, r)
		return
	}
	if !ok || arrived != id {
		a.check = "arrival"
		http.NotFound(sw, r)
		return
	}
	// The key is copied under s.mu: a removal clears it in place.
	s.mu.Lock()
	reg, ok := s.memories[id]
	key, store := slices.Clone(reg.key), reg.store
	s.mu.Unlock()
	defer clear(key)
	if !ok {
		a.check = "sandbox"
		http.NotFound(sw, r)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = ""
	}
	if _, err := sessiontoken.Verify(key, token, id, "", memoryTokenType, time.Now()); token == "" || err != nil {
		a.check = "token"
		sw.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(sw, "invalid memory token", http.StatusUnauthorized)
		return
	}
	// The body is read once, for the audit line and for the store, and
	// before a slot is taken, so a guest that trickles its body holds none.
	// The deadline cuts the trickle off; it is cleared after a read that
	// succeeds, so the next request on a kept-alive connection is not cut
	// by it. One byte past the bound lets memory.Serve answer 413.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(memoryReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, memory.MaxRequest+1))
	if err != nil {
		// The deadline stays: the server would otherwise wait on the rest
		// of the body before it sends this answer.
		http.Error(sw, "cannot read request", http.StatusBadRequest)
		return
	}
	// Only for a body within the bound. An oversized one is answered 413
	// below with its tail unread, and Go drains that tail after the handler:
	// the deadline is what bounds the drain.
	if len(body) <= memory.MaxRequest {
		_ = rc.SetReadDeadline(time.Time{})
	}
	a.method, a.tool, a.name = rpcSummary(body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if !s.memGate.acquire(id) {
		a.check = "busy"
		sw.Header().Set("Retry-After", "1")
		http.Error(sw, "too many memory requests at once; retry", http.StatusServiceUnavailable)
		return
	}
	defer s.memGate.release(id)
	ctx, cancel := context.WithTimeout(r.Context(), memoryCallTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	// The store is opened without s.mu, as at registration, and never
	// created: a store that is gone was removed with `brig memory rm`, and
	// recreating it would let the agent write into it again.
	f, err := memory.OpenExisting(s.cfg.memoryDir, store)
	if err != nil {
		s.logger.Printf("sandbox %s: the memory store did not open: %v", id, err)
		http.Error(sw, "the memory store is unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	memory.Serve(sw, r, f)
}

// rpcSummary reads the JSON-RPC method of body, and for tools/call the tool
// and the memory name. A body it cannot read leaves them empty: memory.Serve
// answers it.
func rpcSummary(body []byte) (method, tool, name string) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", "", ""
	}
	method = clip(req.Method, maxAuditRPCMethod)
	if req.Method != "tools/call" {
		return method, "", ""
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &call) != nil {
		return method, "", ""
	}
	var args struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	// A name memory refuses is not written: only a valid one is known to
	// be a name and not a value pasted in its place.
	if memory.ValidName(args.Name) != nil {
		args.Name = ""
	}
	return method, clip(call.Name, maxAuditTool), args.Name
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// writeMemoryAudit writes the line of one memory request.
func (s *server) writeMemoryAudit(a *memoryAudit, status int, dur time.Duration) {
	attrs := []slog.Attr{slog.String("sandbox", a.sandbox), slog.String("path", a.path),
		slog.String("kind", "memory"), slog.String("method", a.method)}
	if a.tool != "" {
		attrs = append(attrs, slog.String("tool", a.tool))
	}
	if a.name != "" {
		attrs = append(attrs, slog.String("name", a.name))
	}
	attrs = append(attrs, slog.Int("status", status), slog.Duration("dur", dur))
	if a.check != "" {
		attrs = append(attrs, slog.String("check", a.check))
	}
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "broker", attrs...)
}

// statusWriter records the status a handler sent, for the audit line.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// code is the status sent, 200 for a handler that wrote nothing.
func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
