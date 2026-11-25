package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/mcpproxy"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/secret"
)

// The MCP servers are the guest service of kind mcp, served at
// brig.internal/<sandbox>/mcp/<server>. The registry in services.go checks
// where a request arrived and its token, with a key of MCP's own, so a route
// token is refused here and an MCP token on a route. What is MCP's alone is
// below: the registration, the gate, the deadlines and the proxy.
const (
	mcpKind = "mcp"
	// mcpTokenType is the typ claim of an MCP token. A route checks for
	// brig-route, so an MCP token is refused there, and a route's here.
	mcpTokenType = "brig-mcp"
	// mcpLabel is the key-derivation label of an MCP token.
	mcpLabel = "brig-mcp"
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

// mcpReadTimeout bounds the read of one MCP request's body, which happens
// before a slot is taken: a guest that trickles its body must not hold a
// connection open for ever. A variable so a test can shorten it.
var mcpReadTimeout = 10 * time.Second

// beforeMCPRead runs as serveMCP starts to read a request's body. A test
// waits on it to know the request is past the token check.
var beforeMCPRead = func() {}

// mcpGate counts the MCP calls in flight per sandbox. It has no total.
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

// mcpServers is the data of one sandbox's MCP registration.
type mcpServers struct {
	// workload is who the credentials are resolved for.
	workload secret.Workload
	// servers are the sandbox's servers, expanded, by name.
	servers map[string]v1alpha1.MCPServer
}

// mcpService is the guest service of the MCP servers, signing with hostKey,
// the MCP key.
func (s *server) mcpService(hostKey []byte) *guestService {
	return &guestService{
		kind:      mcpKind,
		label:     mcpLabel,
		tokenType: mcpTokenType,
		hostKey:   hostKey,
		same:      func(prev, next any) bool { return reflect.DeepEqual(prev, next) },
		serve:     s.serveMCP,
		// A probe of another sandbox, a request off brig.internal and a bad
		// or replayed token each leave a line, as a refusal of serveMCP does.
		refused: func(arrived, id, rest, check string, status int) {
			s.writeMCPAudit(arrived, id, rest, check, status)
		},
	}
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
	data := mcpServers{workload: secret.Workload{Sandbox: req.ID, Profile: req.Profile}, servers: servers}
	resp, status, err := s.registerService(mcpKind, req.ID, internal, data)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	reply(w, resp)
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
	s.mu.Lock()
	if s.refuseStopping(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	token, err := s.mintService(mcpKind, req.Sandbox, req.Session, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	reply(w, injector.MintResponse{Token: token})
}

// The bounds of the guest's words on an MCP audit line.
const (
	maxAuditServer    = 64
	maxAuditPath      = 64
	maxAuditRPCMethod = 64
	maxAuditTool      = 64
)

// clip cuts s to n bytes.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// serveMCP serves the MCP server name of sandbox id, whose request has
// passed the registry's checks: it arrived on the sandbox's own socket over
// brig.internal, and carries an MCP token of the sandbox. data is the
// sandbox's mcpServers. The proxy answers it, and writes its own audit line.
//
// Nothing here recovers a panic: the proxy aborts a stream it can no longer
// judge with http.ErrAbortHandler, and net/http must see it to break the
// guest's connection rather than end the stream cleanly.
func (s *server) serveMCP(w http.ResponseWriter, r *http.Request, id, name string, data any) {
	start := time.Now()
	reg := data.(mcpServers)
	refuse := func(check string, status int) {
		s.writeMCPAudit(id, id, name, check, status, slog.Duration("dur", time.Since(start)))
	}
	// The name is looked up after the token, so a guest without one cannot
	// learn which servers the sandbox has. Everything under /<id>/mcp/ is
	// MCP's, so a name with a slash is an unknown server.
	srv, ok := reg.servers[name]
	if !ok {
		http.NotFound(w, r)
		refuse("server", http.StatusNotFound)
		return
	}
	// The body is read before a slot is taken, under mcpReadTimeout, so a
	// guest that trickles its body holds no slot; the deadline is cleared
	// after a read within the bound, so the next request on a kept-alive
	// connection, and a long upstream answer, are not cut by it. One byte
	// past the bound lets the proxy answer 413.
	beforeMCPRead()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(mcpReadTimeout))
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
	s.mcpProxy.Serve(w, r.WithContext(ctx), mcpproxy.Target{Sandbox: reg.workload, Server: srv})
}

// writeMCPAudit writes the line of one MCP request refused before the proxy.
// sandbox is the sandbox whose socket it arrived on, or "", and id the one
// its path names, clipped: the path comes from the guest. server is the name
// the path gave, clipped, and check names why it was refused: upgrade,
// arrival, sandbox or token by the registry, server, read or busy here. The
// token and the body are never on it.
func (s *server) writeMCPAudit(sandbox, id, server, check string, status int, extra ...slog.Attr) {
	attrs := []slog.Attr{slog.String("sandbox", sandbox), slog.String("path", clip(id, maxAuditPath)),
		slog.String("kind", mcpKind), slog.String("server", clip(server, maxAuditServer)),
		slog.Int("status", status)}
	attrs = append(attrs, extra...)
	attrs = append(attrs, slog.String("check", check))
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "broker", attrs...)
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
		slog.String("kind", mcpKind), slog.String("server", clip(rec.Server, maxAuditServer)),
		slog.String("method", clip(rec.Method, maxAuditRPCMethod))}
	if rec.Tool != "" {
		attrs = append(attrs, slog.String("tool", clip(rec.Tool, maxAuditTool)))
	}
	attrs = append(attrs, slog.String("verdict", string(rec.Verdict)), slog.Int("status", rec.Status),
		slog.Duration("dur", rec.Duration))
	s.auditLog.LogAttrs(context.Background(), slog.LevelInfo, "broker", attrs...)
}
