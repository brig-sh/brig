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

	"github.com/brig-sh/brig/internal/brigsock"
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

// idleTimeout is how long a connection may sit silent before the daemon closes
// it.
//
// It bounds the silence, not the work: the deadline is reset on every read of
// the connection, so an ensure that spends a minute booting a sandbox is not
// racing it, and neither is a request that arrives in pieces. A client that
// opens a connection and then says nothing costs a goroutine and a descriptor
// until this runs out.
const idleTimeout = 5 * time.Minute

// writeTimeout is how long the daemon will spend delivering one response.
//
// A response is a line of JSON, so it fits the socket buffer and this never
// comes near being reached by a client that reads its answers. One that does
// not read them is the case it exists for: once the buffer fills, the write
// blocks, and with no deadline it blocks for as long as that client cares to
// leave it there, holding a goroutine and a descriptor the read deadline says
// nothing about.
const writeTimeout = 30 * time.Second

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
	//
	// Only things to act on. What the run narrated about its own progress is
	// not among them, so an ensure that went entirely well comes back with no
	// warnings at all and a client can treat any at all as worth showing.
	Warnings []string  `json:"warnings,omitempty"`
	Version  string    `json:"version,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
}

// Session is one sandbox the daemon knows about.
type Session struct {
	Agent     string `json:"agent"`
	Name      string `json:"name,omitempty"`
	Sandbox   string `json:"sandbox"`
	Workspace string `json:"workspace"`
	Running   bool   `json:"running"`
	// RunningError is why the runtime could not be asked. When it is set,
	// Running says nothing -- a runtime that failed to answer is not a runtime
	// reporting a stopped sandbox, and a client that shows the second for the
	// first sends its reader looking for a sandbox that may well still be up. It
	// carries the reason rather than a bare flag because that reason is the only
	// account of what broke, and the daemon has no terminal to print it on.
	RunningError string `json:"runningError,omitempty"`
}

func main() {
	flagSocket := flag.String("socket", "",
		"unix socket to listen on (default $XDG_RUNTIME_DIR/brigd.sock, or ~/.brig/brigd.sock)")
	flag.Parse()

	socket, err := chooseSocket(*flagSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
		os.Exit(1)
	}

	// The daemon builds the registry from the same sources the CLI does. It
	// looks profiles up by name, so without this it would know only the names
	// the CLI happens to have loaded -- which is none of them, the two being
	// separate processes. A broken file is reported and skipped.
	if err := profile.Load(profile.Dir()); err != nil {
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
	}

	if err := serve(socket); err != nil {
		fmt.Fprintln(os.Stderr, "brigd: "+err.Error())
		os.Exit(1)
	}
}

// maxSocketPath is brigsock.MaxPath, kept under its old name so this file and
// its socket_test read the one value. The derivation and the reason for it now
// live in internal/brigsock, where brig doctor can reach them too.
const maxSocketPath = brigsock.MaxPath

// chooseSocket settles which socket the daemon listens on -- the --socket
// value if one was given, the default otherwise -- and refuses a path the
// kernel cannot bind.
//
// Both sources go through the same check, because the default is not
// automatically short: XDG_RUNTIME_DIR is whatever the session manager set,
// and a home directory can be nested anywhere.
//
// The check is here, where the path is chosen, rather than around the bind.
// bind answers an over-long path with EINVAL, and net renders that as "bind:
// invalid argument" -- which names neither the path, nor the limit, nor the
// fact that length is what is wrong with it, so the daemon looks broken for a
// reason that has nothing to do with it. The one thing the reader needs is the
// three numbers, and the only place all three are known is here.
func chooseSocket(flagValue string) (string, error) {
	socket, source := flagValue, "--socket"
	if socket == "" {
		socket, source = brigsock.Default()
	}
	if len(socket) > maxSocketPath {
		return "", fmt.Errorf("refusing to listen on %s: a unix socket path on this system "+
			"can be at most %d bytes and this one is %d, so the kernel would turn the bind "+
			"away without saying why. It came from %s; give brigd a shorter path with "+
			"--socket", socket, maxSocketPath, len(socket), source)
	}
	return socket, nil
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
	mu       sync.Mutex
	sessions map[string]entry       // keyed by VM name
	locks    map[string]*sync.Mutex // one per VM name, see lockFor

	// idle is how long a connection may sit silent and write is how long one
	// response may take to deliver. Fields rather than the constants so a test
	// can ask what happens on either side of them without waiting minutes for
	// the answer.
	idle  time.Duration
	write time.Duration
}

// entry is one remembered sandbox and the runtime that booted it.
//
// The runtime is per sandbox because it is per profile: two profiles can name
// two different runtime binaries, and asking the wrong one whether a sandbox is
// running gets a truthful answer to a different question. It is kept beside the
// Session rather than in it because Session is the wire type, and which binary
// brig drove is the daemon's business.
type entry struct {
	Session
	rt runtime.Runtime
}

func newDaemon() *daemon {
	return &daemon{
		sessions: map[string]entry{},
		locks:    map[string]*sync.Mutex{},
		idle:     idleTimeout,
		write:    writeTimeout,
	}
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
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	// Armed before the socket exists, not after. Notify changes the process's
	// disposition for these signals, and until it runs the default one applies:
	// SIGTERM kills outright, no deferred close, no unlink, so the socket is
	// left behind for the next daemon to trip over. The listen below is what
	// makes brigd reachable, so anything watching for the socket can signal it
	// from that moment, and the handler has to already be there. The channel is
	// buffered, so a signal that arrives before the goroutine starts is held
	// rather than dropped.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

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

	d := newDaemon()

	go func() {
		<-stop
		// Closing the listener is the whole of the shutdown, and the socket is
		// unlinked by that close rather than by a removal of our own. The
		// removal used to be here, one statement later, and one statement later
		// is outside the lock: this goroutine could be descheduled between the
		// two, the accept loop return, the deferred Close release the flock, a
		// second daemon start and bind a socket of its own at the same path, and
		// the removal then unlink the successor's live socket. The successor
		// keeps serving a socket with no name, and every client that dials the
		// path gets a connection refused to a daemon that is running.
		//
		// A close cannot do that, because the unlink is what net does before it
		// closes the descriptor -- and it is that descriptor closing which makes
		// Accept return. So the unlink is ordered before the accept loop returns
		// and therefore before the lock is released, which is the same rule the
		// stale-socket removal above follows. See
		// TestClosingTheListenerUnlinksTheSocket for the assertion that this
		// ordering is real.
		ln.Close()
	}()

	// No runtime named here any more: there is one per request, resolved from
	// the profile the request names, so there is no single answer to give.
	fmt.Fprintf(os.Stderr, "brigd %s listening on %s\n", version, socket)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // the listener closed: a signal, not a failure
		}
		go d.handle(conn)
	}
}

// idleConn arms the read deadline afresh before every read of the connection.
//
// The deadline used to be set once per scan, and a scan reads a whole line, so
// what it bounded was how long a request took to arrive rather than how long
// the client was silent: a client trickling one request across the window was
// cut off however recently its last byte had landed. The daemon has no quarrel
// with a slow writer -- what costs it a goroutine and a descriptor is a
// connection nobody is using -- so the deadline belongs on each read.
type idleConn struct {
	conn net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.conn.Read(p)
}

// replier writes one response per call, each bounded by a deadline of its own.
//
// Nothing bounded a write before this. A client that sends a request and then
// stops reading fills the socket buffer, and Encode then blocks in a write that
// no read deadline covers, holding a goroutine and a descriptor for as long as
// that client leaves the connection there.
//
// A failed write ends the connection rather than being retried: part of the
// answer is already on the wire with no way to say where it stopped, so a
// client reading again would find the tail of one response where the next one
// should start.
type replier struct {
	conn net.Conn
	enc  *json.Encoder
	d    time.Duration
}

func (r replier) reply(resp Response) error {
	if err := r.conn.SetWriteDeadline(time.Now().Add(r.d)); err != nil {
		return err
	}
	return r.enc.Encode(resp)
}

func (d *daemon) handle(conn net.Conn) {
	defer conn.Close()
	scan := bufio.NewScanner(idleConn{conn: conn, idle: d.idle})
	// One byte more than the limit, for the newline. The scanner's cap is on
	// what it buffers rather than on the token it hands back, and a request of
	// exactly the limit is only buffered whole if its terminator fits too.
	scan.Buffer(make([]byte, 0, 64*1024), maxRequestBytes+1)
	out := replier{conn: conn, enc: json.NewEncoder(conn), d: d.write}
	for {
		if !scan.Scan() {
			break
		}
		var req Request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			if err := out.reply(Response{Error: "bad request: " + err.Error()}); err != nil {
				return
			}
			continue
		}
		if err := out.reply(d.dispatch(req)); err != nil {
			return
		}
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
		_ = out.reply(Response{Error: tooLong.Error()})
		fmt.Fprintln(os.Stderr, "brigd: "+tooLong.Error())
	default:
		_ = out.reply(Response{Error: err.Error()})
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
// The runtime is resolved here, per request and from the profile, in the same
// call the CLI makes for the same reason: a profile that names a runtimeBin is
// asking for that binary, and detecting once at startup with no profile in hand
// meant the daemon honoured it for nobody and bound one runtime for its whole
// life. Doing it per request also means a daemon can serve a profile that names
// a binary installed after it started.
//
// Two things separate a daemon's run from the CLI's, and both are settings on
// the Config rather than behaviour of their own. It has no terminal to put a
// question to, so it declares that up front and the image check takes the path
// it already has for having nobody to ask: refuse, and say which setting lets
// a caller proceed deliberately. And what it says belongs to the client that
// asked rather than to whatever terminal brigd was started from, so both
// writers go to a buffer this request owns; concurrent requests each get their
// own. Progress narration is the exception and stays on the daemon's stderr,
// for the reason given where it is set.
func (d *daemon) config(req Request) (*wrap.Config, *bytes.Buffer, error) {
	t, ok := profile.Lookup(req.Agent)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q", req.Agent)
	}
	rt, err := runtime.DetectFor(runtime.Preference{Bin: t.RuntimeBin})
	if err != nil {
		return nil, nil, err
	}
	cfg, err := wrap.Load(t, wrap.Options{Name: req.Name}, rt)
	if err != nil {
		return nil, nil, err
	}
	said := &bytes.Buffer{}
	cfg.Out = said
	cfg.Err = said
	// Not into that buffer. What the run narrates about its own progress is not
	// a warning, and the buffer is delivered to the client as the request's
	// warnings: with the narration in it, an ensure that did everything right
	// answered {"ok":true} carrying "starting sandbox ..." as a complaint about
	// itself, and a client with any rule about a non-empty warnings list acted
	// on it. It goes to the daemon's stderr instead, which is its log and the
	// one place a line saying a boot has started is worth having.
	cfg.Progress = os.Stderr
	// And it keeps narrating there. Progress and the runtime's output sit behind
	// --verbose for a terminal somebody is reading; the daemon's stderr is a
	// log, and a log with the boot lines missing is the wrong trade. The client
	// is unaffected either
	// way: its warnings come from Err, which is the buffer above.
	cfg.Verbosity = wrap.Verbose
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
	d.sessions[cfg.VMName] = entry{
		Session: Session{
			Agent:     cfg.Profile.Name,
			Name:      cfg.RawName,
			Sandbox:   cfg.VMName,
			Workspace: cfg.Workspace,
		},
		// The runtime this sandbox was booted with, so asking whether it is
		// still running asks the binary that booted it.
		rt: cfg.Runtime,
	}
}

// status re-reads liveness from the runtime rather than trusting the
// inventory: a sandbox can be stopped by anything, including a `brig stop`
// that never came through the daemon.
func (d *daemon) status() []Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Session, 0, len(d.sessions))
	for vm, e := range d.sessions {
		running, err := e.rt.Running(vm)
		// Reported as unanswered rather than as stopped. The inventory says brig
		// booted this sandbox, so "not running" is a claim that it exited, and a
		// runtime nobody could reach never made that claim.
		e.Running = running
		if err != nil {
			e.RunningError = err.Error()
		}
		out = append(out, e.Session)
	}
	return out
}
