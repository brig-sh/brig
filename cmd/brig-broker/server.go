package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/renew"
	"github.com/brig-sh/brig/internal/routemode"
	"github.com/brig-sh/brig/internal/secret"
)

// The bounds the broker sets on what a guest can hold open. A request header
// that takes longer than readHeaderTimeout, or a connection idle longer than
// idleTimeout, is closed. The upstream response has no such bound: a model
// can take minutes before the first byte of an event stream.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 90 * time.Second
	// maxHeaderBytes bounds the request line and the headers. A larger one
	// is answered 431 by the server, before the handler or the audit log.
	maxHeaderBytes = 16 << 10
	// maxLog is the size at which the audit log and the broker log are
	// rotated. One previous file is kept, so each takes at most twice this.
	maxLog = 64 << 20
	// sockaddrUnMax is what a unix socket path fits in on macOS, counting
	// the trailing NUL.
	sockaddrUnMax = 103
	// maxConns bounds the connections open at once on the route port. A
	// loopback connection names no sandbox until its request is read, so
	// the bound is for the host, not per sandbox.
	maxConns = 256
	// maxControlBody bounds a control request. The largest is a sandbox
	// with its routes.
	maxControlBody = 1 << 20
)

// errAlreadyRunning is a broker started while another holds the lock.
var errAlreadyRunning = errors.New("another broker is running")

// serverConfig is what a server is built from. Tests replace the store, the
// upstream transport and the lifecycles.
type serverConfig struct {
	dir string
	// openStore opens brig's secret store, the keychain on macOS.
	openStore func() (secret.Store, error)
	// transport carries the upstream requests. nil is the broker's own,
	// with the system roots.
	transport http.RoundTripper
	// renewerFor returns the Renewer of a lifecycle. nil is lifecycleRenewer.
	renewerFor func(v1alpha1.Lifecycle) (secret.Renewer, error)
	// logTo takes the broker's own lines. nil is broker.log, rotated at
	// logMax.
	logTo io.Writer
	// logMax is the size at which the logs rotate. Zero is maxLog.
	logMax int64
	// resolve resolves a host name for a divert handler. nil is the host's
	// resolver.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// dial dials an address from the host for a divert handler. nil dials
	// TCP.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// lifecycleRenewer returns the Renewer that keeps a credential of lifecycle
// l current.
func lifecycleRenewer(l v1alpha1.Lifecycle) (secret.Renewer, error) {
	switch l {
	case v1alpha1.LifecycleAnthropicOAuth:
		return renew.AnthropicOAuth{}, nil
	}
	return nil, fmt.Errorf("lifecycle %q is not one this broker runs", l)
}

// server is one broker: the route handler on the loopback port and the
// control API on the unix socket.
type server struct {
	cfg     serverConfig
	handler *broker.Handler
	creds   *credentials
	ca      *authority
	lock    *os.File
	audit   *rotatingFile
	// auditLog writes the audit lines of the handler and of the divert
	// sockets.
	auditLog *slog.Logger
	log      *rotatingFile
	routes   net.Listener
	control  net.Listener
	// internal takes the brig.internal connections of every divert socket,
	// through TLS, for the route server that serves them.
	internal *connListener
	port     int
	// storeErr is why brig's secret store did not open at start.
	storeErr error

	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, addr string) (net.Conn, error)

	mu        sync.Mutex
	sandboxes map[string]string // id -> the registration, as canonical JSON
	// diverts are the divert sockets, by sandbox id.
	diverts  map[string]*divertSandbox
	stopping bool
	stop     chan struct{}
}

// newServer takes the lock, the keys and both listeners. It returns
// errAlreadyRunning when another broker serves dir.
func newServer(cfg serverConfig) (_ *server, err error) {
	if err := os.MkdirAll(cfg.dir, 0o700); err != nil {
		return nil, err
	}
	if err := injector.CheckDir(cfg.dir); err != nil {
		return nil, err
	}
	s := &server{cfg: cfg, sandboxes: map[string]string{}, diverts: map[string]*divertSandbox{},
		stop: make(chan struct{}), internal: newConnListener(), resolve: cfg.resolve, dial: cfg.dial}
	if s.resolve == nil {
		s.resolve = lookupHost
	}
	if s.dial == nil {
		s.dial = dialUpstream
	}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	if s.lock, err = takeLock(filepath.Join(cfg.dir, injector.LockName)); err != nil {
		return nil, err
	}
	hostKey, err := loadOrCreateHostKey(filepath.Join(cfg.dir, injector.HostKeyName))
	if err != nil {
		return nil, err
	}
	if s.ca, err = loadOrCreateCA(cfg.dir); err != nil {
		return nil, err
	}
	logMax := cfg.logMax
	if logMax == 0 {
		logMax = maxLog
	}
	if s.audit, err = openRotating(filepath.Join(cfg.dir, injector.AuditName), logMax); err != nil {
		return nil, err
	}
	if cfg.logTo == nil {
		if s.log, err = openRotating(filepath.Join(cfg.dir, injector.LogName), logMax); err != nil {
			return nil, err
		}
	}
	// A store that does not open fails the routes that read it, not the
	// broker: a host with no keyring still registers sandboxes and mints
	// their tokens.
	store, err := cfg.openStore()
	if err != nil {
		s.storeErr = err
		store = newLazyStore(cfg.openStore)
	}
	renewerFor := cfg.renewerFor
	if renewerFor == nil {
		renewerFor = lifecycleRenewer
	}
	s.creds = newCredentials(store, renewerFor)
	s.auditLog = slog.New(slog.NewTextHandler(s.audit, nil))
	s.handler, err = broker.New(broker.Config{
		HostKey:      hostKey,
		Credentials:  s.creds,
		Audit:        s.auditLog,
		Transport:    cfg.transport,
		ArrivalCheck: s.arrival,
	})
	clear(hostKey)
	if err != nil {
		return nil, err
	}
	ln, err := listenRoutes(cfg.dir)
	if err != nil {
		return nil, err
	}
	s.routes = newLimitListener(ln, maxConns)
	s.port = ln.Addr().(*net.TCPAddr).Port
	// The lock is held, so a socket file here was left by a broker that did
	// not exit cleanly.
	sock := filepath.Join(cfg.dir, injector.SocketName)
	if len(sock) > sockaddrUnMax {
		return nil, fmt.Errorf("the control socket %s is %d bytes, and a unix socket path is at most %d; "+
			"point BRIG_STATE_DIR at a shorter directory", sock, len(sock), sockaddrUnMax)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.control, err = net.Listen("unix", sock); err != nil {
		return nil, fmt.Errorf("cannot open the control socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		return nil, err
	}
	if err := s.writePID(); err != nil {
		return nil, err
	}
	return s, nil
}

// serve serves routes and control requests until ctx ends or stop is asked
// for, then closes both listeners and removes the socket and the pid file.
func (s *server) serve(ctx context.Context) error {
	var out io.Writer = s.log
	if s.cfg.logTo != nil {
		out = s.cfg.logTo
	}
	logger := log.New(out, "brig-broker: ", log.LstdFlags)
	routes := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          logger,
	}
	// brig.internal is the same handler, on the connections the divert
	// sockets hand over. Each request's remote address names the socket it
	// arrived on. See briginternal.go.
	internal := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          logger,
	}
	control := &http.Server{
		Handler:           s.controlMux(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          logger,
	}
	errs := make(chan error, 3)
	go func() { errs <- routes.Serve(s.routes) }()
	go func() { errs <- internal.Serve(s.internal) }()
	go func() { errs <- control.Serve(s.control) }()
	logger.Printf("serving routes on 127.0.0.1:%d, control on %s", s.port, s.control.Addr())
	if s.storeErr != nil {
		logger.Printf("brig's secret store did not open, and every route answers 502 until it does: %v", s.storeErr)
	}

	var err error
	select {
	case <-ctx.Done():
	case <-s.stop:
	case err = <-errs:
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = control.Shutdown(shut)
	_ = routes.Shutdown(shut)
	_ = internal.Shutdown(shut)
	logger.Printf("stopped")
	s.close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// close releases what newServer took. The pid file and the socket go only
// while the lock is held, so a broker that lost the race removes nothing of
// the one that won.
func (s *server) close() {
	s.mu.Lock()
	for id, l := range s.diverts {
		l.close()
		delete(s.diverts, id)
	}
	s.mu.Unlock()
	if s.control != nil {
		_ = s.control.Close()
	}
	if s.routes != nil {
		_ = s.routes.Close()
	}
	if s.internal != nil {
		_ = s.internal.Close()
	}
	if s.lock != nil {
		_ = os.Remove(filepath.Join(s.cfg.dir, injector.SocketName))
		_ = os.Remove(filepath.Join(s.cfg.dir, injector.PIDName))
		_ = s.lock.Close()
	}
	if s.audit != nil {
		_ = s.audit.Close()
	}
	if s.log != nil {
		_ = s.log.Close()
	}
}

// writePID records this process and its start time, written whole through a
// rename so brig never reads half a line.
func (s *server) writePID() error {
	start, err := injector.ProcessStart(os.Getpid())
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.dir, injector.PIDName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, injector.PIDLine(os.Getpid(), start)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// controlMux is the control API. docs/broker.md lists it.
func (s *server) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", s.addSandbox)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.removeSandbox)
	mux.HandleFunc("POST /tokens", s.mint)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /stop", s.stopRequest)
	return mux
}

// decode reads a JSON body strictly into v.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "malformed request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// refuseStopping answers a request a stopping broker cannot take, and
// reports whether it did. brig waits for the broker to exit and starts
// another.
func (s *server) refuseStopping(w http.ResponseWriter) bool {
	if s.stopping {
		http.Error(w, injector.StoppingMessage, http.StatusServiceUnavailable)
		return true
	}
	return false
}

// addSandbox registers a sandbox's routes. Each route is expanded again, so
// a credential lifecycle comes only from a preset brig ships. The same
// registration again changes nothing, and the tokens minted for it stay
// valid.
func (s *server) addSandbox(w http.ResponseWriter, r *http.Request) {
	var req injector.Sandbox
	if !decode(w, r, &req) {
		return
	}
	if err := broker.ValidID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sb := broker.Sandbox{ID: req.ID, Profile: req.Profile}
	internal, err := brigInternalAddr(req.BrigInternal)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
		return
	}
	// The settings of each route mode besides endpoint go to that mode's
	// divert handler, which checks them before anything is registered.
	for _, m := range slices.Sorted(maps.Keys(req.Modes)) {
		dm, ok := divertModes[m]
		if !ok {
			http.Error(w, fmt.Sprintf("sandbox %s: %s", req.ID, routemode.NotInBuild(m)), http.StatusBadRequest)
			return
		}
		if err := dm.check(req.ID, req.Modes[m]); err != nil {
			http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
			return
		}
		sb.Modes = append(sb.Modes, m)
	}
	for _, rt := range req.Routes {
		expanded, err := policy.ExpandRoute(rt.Route)
		if err != nil {
			http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusBadRequest)
			return
		}
		sb.Routes = append(sb.Routes, broker.Route{Policy: rt.Policy, Route: expanded})
	}
	key, err := json.Marshal(struct {
		broker.Sandbox
		BrigInternal string
		Settings     map[v1alpha1.RouteMode]json.RawMessage
	}{sb, req.BrigInternal, req.Modes})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseStopping(w) {
		return
	}
	if s.sandboxes[req.ID] == string(key) {
		reply(w, s.added(req.ID))
		return
	}
	// A secret tracked for this registration alone is dropped again when
	// the registration fails, so no cache is left for a route nobody serves.
	var tracked []string
	untrack := func() {
		for _, name := range tracked {
			s.creds.untrack(name)
		}
	}
	for _, rt := range sb.Routes {
		name := rt.Route.Credential.SecretRef.Name
		created, err := s.creds.track(name, rt.Route.Credential.Lifecycle)
		if err != nil {
			untrack()
			http.Error(w, fmt.Sprintf("sandbox %s: route %s: %v", req.ID, rt.Route.Name, err), http.StatusBadRequest)
			return
		}
		if created {
			tracked = append(tracked, name)
		}
	}
	if err := s.handler.AddSandbox(sb); err != nil {
		untrack()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The socket path is the sandbox's, so the listener of a registration
	// this one replaces goes first.
	if old := s.diverts[req.ID]; old != nil {
		old.close()
		delete(s.diverts, req.ID)
	}
	// One socket for brig.internal and every route mode besides endpoint.
	if internal.IsValid() || len(sb.Modes) > 0 {
		l, err := s.openDivert(req.ID, internal, sb.Modes, req.Modes)
		if err != nil {
			s.handler.RemoveSandbox(req.ID)
			delete(s.sandboxes, req.ID)
			untrack()
			http.Error(w, fmt.Sprintf("sandbox %s: %v", req.ID, err), http.StatusInternalServerError)
			return
		}
		s.diverts[req.ID] = l
	}
	s.sandboxes[req.ID] = string(key)
	for _, name := range tracked {
		s.creds.recover(name)
	}
	reply(w, s.added(req.ID))
}

// added is the answer to a registration of sandbox id. s.mu is held. A
// sandbox that reaches brig.internal is handed the CA certificate to trust
// it by, and never its key.
func (s *server) added(id string) injector.AddResponse {
	l := s.diverts[id]
	if l == nil {
		return injector.AddResponse{}
	}
	resp := injector.AddResponse{DivertSocket: l.path}
	if l.internal.IsValid() {
		resp.CA = string(s.ca.certPEM())
	}
	return resp
}

// brigInternalAddr reads the brig.internal address of a registration: "" for
// none, or one IP address in its canonical spelling, never unspecified,
// loopback or multicast. The broker serves brig.internal on connections to
// it, and never dials it.
func brigInternalAddr(s string) (netip.Addr, error) {
	if s == "" {
		return netip.Addr{}, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || a.String() != s || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("the brig.internal address %q is not one IP address a guest can reach", s)
	}
	return a.Unmap(), nil
}

func (s *server) removeSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := broker.ValidID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, removed := s.sandboxes[id]
	s.handler.RemoveSandbox(id)
	delete(s.sandboxes, id)
	if l := s.diverts[id]; l != nil {
		l.close()
		delete(s.diverts, id)
	}
	remaining := len(s.sandboxes)
	s.mu.Unlock()
	reply(w, injector.RemoveResponse{Removed: removed, Remaining: remaining})
}

func (s *server) mint(w http.ResponseWriter, r *http.Request) {
	var req injector.MintRequest
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
	if s.refuseStopping(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	token, err := s.handler.Mint(req.Sandbox, req.Session, req.Route, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	reply(w, injector.MintResponse{Token: token})
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	pending, failing, recovering := s.creds.report()
	st := injector.Status{PID: os.Getpid(), Port: s.port, Sandboxes: ids,
		Pending: pending, Failing: failing, Recovering: recovering}
	if n := s.handler.Placeholders(); len(n) > 0 {
		st.Placeholder = n
	}
	reply(w, st)
}

// stopRequest stops the broker. With ifIdle it stops only when no sandbox
// is registered, and from then on refuses a new one.
func (s *server) stopRequest(w http.ResponseWriter, r *http.Request) {
	var req injector.StopRequest
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	ok := !req.IfIdle || len(s.sandboxes) == 0
	first := ok && !s.stopping
	if ok {
		s.stopping = true
	}
	s.mu.Unlock()
	reply(w, injector.StopResponse{Stopping: ok})
	if first {
		close(s.stop)
	}
}

// credentials is the secret.Broker the handler reads through. A secret with
// a lifecycle has a renew.Cache of its own, the one owner of its refresh in
// this process. Every other secret is read from the store on each request.
type credentials struct {
	store      secret.Store
	base       secret.Broker
	renewerFor func(v1alpha1.Lifecycle) (secret.Renewer, error)

	mu     sync.Mutex
	caches map[string]*renew.Cache
}

var _ broker.Renewer = (*credentials)(nil)

func newCredentials(store secret.Store, renewerFor func(v1alpha1.Lifecycle) (secret.Renewer, error)) *credentials {
	return &credentials{
		store:      store,
		base:       secret.StoreBroker(store),
		renewerFor: renewerFor,
		caches:     map[string]*renew.Cache{},
	}
}

// track gives the secret name the cache of lifecycle l, and reports whether
// it created one. A secret already tracked keeps its cache, so two sandboxes
// that share a credential share its refresh.
func (c *credentials) track(name string, l v1alpha1.Lifecycle) (bool, error) {
	if l == "" {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.caches[name]; ok {
		return false, nil
	}
	r, err := c.renewerFor(l)
	if err != nil {
		return false, err
	}
	c.caches[name] = renew.NewCache(c.base, c.store, map[string]secret.Renewer{name: r})
	return true, nil
}

// recover settles, in the background, the renewal journal a broker that
// stopped left for the secret name. The first request for it waits on the
// same turn, so it is served only once that is done. Registration does not
// wait: a keychain that hangs would hold it, and the control socket behind
// it, for as long.
func (c *credentials) recover(name string) {
	if cache := c.cache(name); cache != nil {
		go func() { _ = cache.Recover(context.Background(), name) }()
	}
}

// untrack drops the cache of the secret name.
func (c *credentials) untrack(name string) {
	c.mu.Lock()
	delete(c.caches, name)
	c.mu.Unlock()
}

func (c *credentials) cache(name string) *renew.Cache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caches[name]
}

func (c *credentials) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	if cache := c.cache(name); cache != nil {
		return cache.Resolve(ctx, name, w)
	}
	return c.base.Resolve(ctx, name, w)
}

func (c *credentials) ForceRenew(ctx context.Context, name string, w secret.Workload, stale []byte) ([]byte, error) {
	cache := c.cache(name)
	if cache == nil {
		return nil, fmt.Errorf("%q has no lifecycle and cannot be renewed", name)
	}
	return cache.ForceRenew(ctx, name, w, stale)
}

// report returns the secrets with a write-back pending, those whose last
// refresh failed, and those with a renewal journal still to settle. Names
// only.
func (c *credentials) report() (pending, failing, recovering []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cache := range c.caches {
		pending = append(pending, cache.Pending()...)
		failing = append(failing, cache.Failing()...)
		recovering = append(recovering, cache.Recovering()...)
	}
	slices.Sort(pending)
	slices.Sort(failing)
	slices.Sort(recovering)
	return pending, failing, recovering
}

// takeLock opens and locks path for the life of the broker.
func takeLock(path string) (*os.File, error) {
	f, err := openPrivate(path, os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}
		return nil, err
	}
	return f, nil
}

// openPrivate opens a file of the broker's own, creating it 0600. A link is
// refused, and so is a file someone else can read.
func openPrivate(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	if err := checkPrivate(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// checkPrivate refuses an open file that is not a regular file, or that
// someone other than its owner can read or write.
func checkPrivate(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing %s: it is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing %s: its mode is %#o, and only its owner may read it", path, info.Mode().Perm())
	}
	return nil
}

// limitListener holds at most n connections open, and makes the next
// Accept wait for one to close.
type limitListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
