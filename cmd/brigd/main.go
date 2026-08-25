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
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

var version = "dev"

// maxRequestBytes is the longest request line brigd reads, newline excluded.
//
// A request is an op, a profile name and a session name, so a megabyte is far
// more than one can honestly need; the limit is there so a client that never
// sends a newline cannot make the daemon buffer without bound. It is a raised
// bufio.Scanner default rather than an invented number, and it is a documented
// part of the protocol precisely because exceeding it used to be invisible:
// the scan ended, the connection closed, and neither the client nor the log
// was told anything.
const maxRequestBytes = 1 << 20

// idleTimeout is how long a connection may sit without sending a request
// before the daemon closes it.
//
// It bounds the silence between requests, not the work: the deadline is reset
// before each read, so an ensure that spends a minute booting a sandbox is not
// racing it. A client that opens a connection and then says nothing costs a
// goroutine and a descriptor until this runs out.
const idleTimeout = 5 * time.Minute

// Request is one line of JSON on the socket.
type Request struct {
	Op    string `json:"op"`    // ensure | status | stop | version
	Agent string `json:"agent"` // template name, for ensure and stop
	Name  string `json:"name"`  // session name, optional
}

// Response is one line of JSON back.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Warnings is what the run said about itself: a credential that was not
	// forwarded and why, an expiring secret, an image that could not be
	// verified. The CLI prints these on the terminal of the person who typed
	// the command; a daemon has no such terminal, and its own is nobody's, so
	// they travel back to the client that asked instead. One line each, in the
	// order they were said.
	Warnings []string  `json:"warnings,omitempty"`
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

// lockSocket takes the exclusive lock that makes this process the daemon for
// one socket path, and returns the file holding it.
//
// The lock lives on a sidecar file rather than on the socket, because a unix
// socket cannot carry one: the kernel drops the socket's inode from the
// filesystem the moment somebody unlinks the path, and the listener keeps
// working, so there is nothing there for a second daemon to test. flock on a
// plain file is dropped when the process dies however it dies, which is what
// makes a crashed daemon leave nothing behind to clean up and no stale pid to
// second-guess.
//
// The pid goes into the file as well. Nothing reads it to decide anything --
// the lock decides -- but a refusal that names the process in the way is the
// difference between "it is already running" and a hunt through ps.
//
// The file is deliberately not removed on shutdown. Unlinking it would let a
// daemon starting at that moment hold a lock on a file that no longer has a
// name, while a third one creates a new file at the same path and locks that:
// two daemons, two locks, one socket, which is the bug this exists to stop.
func lockSocket(socket string) (*os.File, error) {
	path := socket + ".lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(path)
		_ = f.Close()
		return nil, fmt.Errorf("another brigd%s is already serving %s. Stop it, or "+
			"start this one on a socket of its own with --socket", holder, socket)
	}
	// Truncated before the pid is written, so a shorter pid than the last one
	// cannot leave the tail of the old one behind for the next reader.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// lockHolder names the process holding the lock, as " (pid 123)", or says
// nothing at all when the file has no readable pid in it. It is only ever part
// of a message, so an unreadable file is not worth an error of its own.
func lockHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(b))
	if pid == "" {
		return ""
	}
	return " (pid " + pid + ")"
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
	// One daemon per socket path, and the lock is what decides it. Binding a
	// unix socket takes no lock of any kind, so nothing in the listen below
	// keeps a second daemon out: both stay up, both report listening, and the
	// second one answers -- which leaves the first holding an inventory that no
	// longer matches reality and a per-VM mutex that serialises nothing,
	// because the two processes do not share it.
	lock, err := lockSocket(socket)
	if err != nil {
		return err
	}
	defer lock.Close()

	// Only now that the lock is held. A socket left behind by a crashed daemon
	// would block the bind and has to go; one that a live daemon is serving
	// must not, and holding the lock is the difference between the two.
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
	// One byte more than the limit, for the newline. The scanner's cap is on
	// what it buffers rather than on the token it hands back, and a request of
	// exactly the limit is only buffered whole if its terminator fits too.
	scan.Buffer(make([]byte, 0, 64*1024), maxRequestBytes+1)
	enc := json.NewEncoder(conn)
	for {
		// Set before every read and not once for the connection: it bounds how
		// long a client may hold a handler goroutine open saying nothing, and
		// the time a request spends being served is not the client's silence.
		// A connection that goes quiet is closed rather than kept forever.
		if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return
		}
		if !scan.Scan() {
			break
		}
		var req Request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = enc.Encode(Response{Error: "bad request: " + err.Error()})
			continue
		}
		_ = enc.Encode(d.dispatch(req))
	}
	// A scan that stops on an error rather than on end of input is the one case
	// a client cannot see for itself: it wrote a request and the connection
	// closed, with nothing said here and nothing said in the daemon's log
	// either. Say both.
	//
	// The connection is not read any further after this. What follows an
	// over-length request in the stream is the tail of that request, and there
	// is no way to tell where it ends and the next one begins, so answering and
	// closing is the only honest thing to do with it.
	switch err := scan.Err(); {
	case err == nil:
	case errors.Is(err, os.ErrDeadlineExceeded):
		// An idle client is not a failure and has nothing to be told: it is
		// still holding its end of a connection nobody is using.
	case errors.Is(err, bufio.ErrTooLong):
		tooLong := fmt.Errorf("request is longer than the %d byte limit, so it was "+
			"not read", maxRequestBytes)
		_ = enc.Encode(Response{Error: tooLong.Error()})
		fmt.Fprintln(os.Stderr, "brigd: "+tooLong.Error())
	default:
		_ = enc.Encode(Response{Error: err.Error()})
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
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

// config resolves one request into a run, and hands back the buffer that run
// says things into.
//
// Two things separate a daemon's run from the CLI's, and both are settings on
// the Config rather than behaviour of their own. It has no terminal to put a
// question to, so it declares that up front and the image check takes the path
// it already has for having nobody to ask: refuse, and say which setting lets
// a caller proceed deliberately. And what it says belongs to the client that
// asked rather than to whatever terminal brigd was started from, so both
// writers go to a buffer this request owns; concurrent requests each get their
// own.
func (d *daemon) config(req Request) (*wrap.Config, *bytes.Buffer, error) {
	t, ok := profile.Lookup(req.Agent)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q", req.Agent)
	}
	cfg, err := wrap.Load(t, wrap.Options{Name: req.Name}, d.rt)
	if err != nil {
		return nil, nil, err
	}
	said := &bytes.Buffer{}
	cfg.Out = said
	cfg.Err = said
	cfg.NoTerminal = true
	return cfg, said, nil
}

// warnings turns what a run wrote into the response's warnings, one line each.
func warnings(buf *bytes.Buffer) []string {
	if buf == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ensure boots the sandbox if it is not already up, using exactly the CLI's
// path: same workspace preparation, same credential resolution, same
// stale-share check.
func (d *daemon) ensure(req Request) Response {
	cfg, said, err := d.config(req)
	if err != nil {
		return Response{Error: err.Error()}
	}
	lock := d.lockFor(cfg.VMName)
	lock.Lock()
	defer lock.Unlock()

	// BuildEnv fails the whole request rather than booting a sandbox missing a
	// binding it needs -- same rule as the CLI's own path. A run naming several
	// absent secrets makes this Error multi-line, one fix per secret -- import
	// or create, depending on the secret; there is no brigd client in this
	// repo today, so whatever renders
	// Response.Error next must not collapse those newlines onto one line.
	set, err := cfg.BuildEnv()
	if err != nil {
		return Response{Error: err.Error(), Warnings: warnings(said)}
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return Response{Error: err.Error(), Warnings: warnings(said)}
	}
	d.remember(cfg)
	return Response{OK: true, Warnings: warnings(said), Sessions: d.status()}
}

func (d *daemon) stop(req Request) Response {
	cfg, said, err := d.config(req)
	if err != nil {
		return Response{Error: err.Error()}
	}
	lock := d.lockFor(cfg.VMName)
	lock.Lock()
	defer lock.Unlock()

	if err := cfg.Stop(); err != nil {
		return Response{Error: err.Error(), Warnings: warnings(said)}
	}
	d.mu.Lock()
	delete(d.sessions, cfg.VMName)
	d.mu.Unlock()
	return Response{OK: true, Warnings: warnings(said), Sessions: d.status()}
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
