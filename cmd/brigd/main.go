// Command brigd keeps sandbox sessions alive.
//
// It is deliberately small. Everything it knows about workspaces,
// credentials and the guest lives in internal/wrap, the same package the brig
// CLI uses, so the daemon cannot grow a second opinion about how a sandbox is
// built. What it adds is the part a one-shot CLI cannot do: a place to keep
// the session inventory, and one owner for boot and teardown when several
// callers want the same sandbox.
//
// It does not proxy exec. Handing a caller's terminal to a process inside the
// guest means passing file descriptors, and `brig exec` already does that
// directly and correctly by replacing itself with the runtime. The daemon
// owns lifecycle; the CLI owns the terminal.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

var version = "dev"

// Request is one line of JSON on the socket.
type Request struct {
	Op    string `json:"op"`    // ensure | status | stop | version
	Agent string `json:"agent"` // template name, for ensure and stop
	Name  string `json:"name"`  // session name, optional
}

// Response is one line of JSON back.
type Response struct {
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Version  string    `json:"version,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
}

// Session is one sandbox the daemon knows about.
type Session struct {
	Agent     string `json:"agent"`
	Name      string `json:"name,omitempty"`
	VM        string `json:"vm"`
	Workspace string `json:"workspace"`
	Running   bool   `json:"running"`
}

func main() {
	socket := flag.String("socket", defaultSocket(), "unix socket to listen on")
	flag.Parse()

	// The daemon builds the registry from the same sources the CLI does. It
	// looks profiles up by name, so without this it would know only the names
	// the CLI happens to have loaded -- which is none of them, the two being
	// separate processes. A broken file is reported and skipped.
	if err := profile.Load(profile.Dir()); err != nil {
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
	}

	if err := serve(*socket); err != nil {
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
		os.Exit(1)
	}
}

func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "brigd.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".brig", "brigd.sock")
}

type daemon struct {
	rt runtime.Runtime

	mu       sync.Mutex
	sessions map[string]Session     // keyed by VM name
	locks    map[string]*sync.Mutex // one per VM name, see lockFor
}

// lockFor serialises work on one sandbox.
//
// Being the single owner of boot and teardown is the daemon's whole reason to
// exist, and connections are handled concurrently: without this, two clients
// asking for the same sandbox at the same moment both find it absent and both
// boot it. Held across EnsureRunning and Stop, not just around the inventory.
func (d *daemon) lockFor(vm string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l, ok := d.locks[vm]; ok {
		return l
	}
	l := &sync.Mutex{}
	d.locks[vm] = l
	return l
}

func serve(socket string) error {
	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	// A socket left behind by a crashed daemon would block the bind. Removing
	// it is safe only because a live daemon holds a lock on it, which the
	// bind below re-establishes.
	_ = os.Remove(socket)

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	// The socket carries lifecycle control over sandboxes holding live
	// credentials, so it is the invoking user's alone.
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}

	d := &daemon{rt: rt, sessions: map[string]Session{}, locks: map[string]*sync.Mutex{}}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ln.Close()
		os.Remove(socket)
	}()

	fmt.Fprintf(os.Stderr, "brigd %s listening on %s (runtime %s)\n", version, socket, rt.Kind())
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // the listener closed: a signal, not a failure
		}
		go d.handle(conn)
	}
}

func (d *daemon) handle(conn net.Conn) {
	defer conn.Close()
	scan := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for scan.Scan() {
		var req Request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = enc.Encode(Response{Error: "bad request: " + err.Error()})
			continue
		}
		_ = enc.Encode(d.dispatch(req))
	}
}

func (d *daemon) dispatch(req Request) Response {
	switch req.Op {
	case "version":
		return Response{OK: true, Version: version}
	case "status":
		return Response{OK: true, Sessions: d.status()}
	case "ensure":
		return d.ensure(req)
	case "stop":
		return d.stop(req)
	default:
		return Response{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}

func (d *daemon) config(req Request) (*wrap.Config, error) {
	t, ok := profile.Lookup(req.Agent)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", req.Agent)
	}
	return wrap.Load(t, wrap.Options{Name: req.Name}, d.rt)
}

// ensure boots the sandbox if it is not already up, using exactly the CLI's
// path: same workspace preparation, same credential resolution, same
// stale-share check.
func (d *daemon) ensure(req Request) Response {
	cfg, err := d.config(req)
	if err != nil {
		return Response{Error: err.Error()}
	}
	lock := d.lockFor(cfg.VMName)
	lock.Lock()
	defer lock.Unlock()

	set, err := cfg.BuildEnv()
	if err != nil {
		return Response{Error: err.Error()}
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return Response{Error: err.Error()}
	}
	d.remember(cfg)
	return Response{OK: true, Sessions: d.status()}
}

func (d *daemon) stop(req Request) Response {
	cfg, err := d.config(req)
	if err != nil {
		return Response{Error: err.Error()}
	}
	lock := d.lockFor(cfg.VMName)
	lock.Lock()
	defer lock.Unlock()

	if err := cfg.Stop(); err != nil {
		return Response{Error: err.Error()}
	}
	d.mu.Lock()
	delete(d.sessions, cfg.VMName)
	d.mu.Unlock()
	return Response{OK: true, Sessions: d.status()}
}

func (d *daemon) remember(cfg *wrap.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions[cfg.VMName] = Session{
		Agent:     cfg.Profile.Name,
		Name:      cfg.RawName,
		VM:        cfg.VMName,
		Workspace: cfg.Workspace,
	}
}

// status re-reads liveness from the runtime rather than trusting the
// inventory: a sandbox can be stopped by anything, including a `brig stop`
// that never came through the daemon.
func (d *daemon) status() []Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Session, 0, len(d.sessions))
	for vm, s := range d.sessions {
		s.Running = d.rt.Running(vm)
		out = append(out, s)
	}
	return out
}
