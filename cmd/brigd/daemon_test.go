package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/ttytest"
)

// stubRuntime points the daemon at a runtime binary that exists and does
// nothing. The machine running the tests has neither hull nor nerdctl, and a
// request whose profile names no runtimeBin of its own has to find one.
func stubRuntime(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(shortDir(t), "hull")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	return bin
}

// startDaemon runs a daemon in this process and returns once it answers.
//
// It stays up for the rest of the test binary's life: serve has no way to be
// asked to stop other than a signal, and every test gets a socket path of its
// own, so there is nothing to collide with.
func startDaemon(t *testing.T, socket string) {
	t.Helper()
	failed := make(chan error, 1)
	go func() { failed <- serve(socket) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-failed:
			t.Fatalf("the daemon exited instead of listening: %v", err)
		default:
		}
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the daemon never started listening")
}

// ask sends one request on a connection of its own and returns the response.
func ask(t *testing.T, socket, request string) Response {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, request+"\n"); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("no response to %s: %v", request, err)
	}
	return resp
}

// A second daemon must not take a socket path a live one is serving. Binding a
// unix socket takes no lock, so without one of its own both daemons stay up
// and the second answers requests the first believes it is handling.

// shortDir is a temporary directory whose path stays under the unix socket
// limit. t.TempDir puts the test's own name in the path, and on macOS that
// lands the socket past 104 bytes, where bind fails with "invalid argument"
// and the daemon looks broken for a reason that has nothing to do with it.
//
// chooseSocket now refuses such a path by name, but refusing it is not binding
// it: a test that wants a listener still needs one that fits.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "brigd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestASecondDaemonRefusesTheSocketOfARunningOne(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	// Bounded, because the failure this asserts against is a second daemon
	// that starts and serves forever rather than one that exits.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, os.Args[0], "-test.run=TestServeInAHelperProcess")
	second.Env = append(os.Environ(), "BRIGD_HELPER_SOCKET="+socket)
	out, err := second.CombinedOutput()

	if err == nil {
		t.Fatalf("a second daemon started on a socket already being served: %s", out)
	}
	if !strings.Contains(string(out), strconv.Itoa(os.Getpid())) {
		t.Errorf("the refusal does not name the running daemon (pid %d): %s", os.Getpid(), out)
	}
	// The first one is the whole point: refusing is only right if the daemon
	// that was already there is still serving.
	if resp := ask(t, socket, `{"op":"version"}`); !resp.OK {
		t.Errorf("the running daemon stopped serving: %+v", resp)
	}
}

// The socket is unlinked by closing the listener, and net does that unlink
// before it closes the descriptor -- which is what makes the unlink ordered
// before Accept returns and therefore before serve releases the flock. That
// ordering is the entire argument that a daemon on its way out cannot unlink
// the socket of the one that replaces it, and it is a property of the standard
// library rather than of anything here, so it is asserted rather than assumed.
func TestClosingTheListenerUnlinksTheSocket(t *testing.T) {
	socket := filepath.Join(shortDir(t), "brigd.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the listener bound but left nothing at %s: %v", socket, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("closing the listener left the socket at %s (%v), so shutdown "+
			"would need a removal of its own, outside the lock", socket, err)
	}
}

// A signalled daemon cleans up after itself and leaves the path free for the
// next one. This is the half of the removal's job that had to survive dropping
// it: the shutdown no longer names the path, so if closing the listener did not
// unlink it, a successor would find a stale socket in the way.
func TestASignalledDaemonLeavesThePathFreeForItsSuccessor(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(shortDir(t), "brigd.sock")

	// In a process of its own, because a signal is process-wide: delivering
	// SIGTERM to this one would take the test binary with it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first := exec.CommandContext(ctx, os.Args[0], "-test.run=TestServeInAHelperProcess")
	first.Env = append(os.Environ(), "BRIGD_HELPER_SOCKET="+socket)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Process.Kill() }()
	waitUntilServing(t, socket)

	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("the daemon did not exit cleanly on SIGTERM: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("a signalled daemon left its socket behind at %s (%v)", socket, err)
	}

	// The successor is what the removal was for. It has to bind the same path
	// and answer on it.
	startDaemon(t, socket)
	if resp := ask(t, socket, `{"op":"version"}`); !resp.OK {
		t.Errorf("the successor is not serving: %+v", resp)
	}
}

// waitUntilServing returns once something answers on socket.
func waitUntilServing(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing ever started serving %s", socket)
}

// A request past the limit is answered rather than dropped. The scanner used
// to end the scan on one, which closed the connection with nothing written to
// the client and nothing written to the log: a client that sent too much saw
// exactly what a crashed daemon looks like.
func TestAnOverlongRequestIsAnswered(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	// A request of exactly the limit is still served. Without this the test
	// below would pass against a daemon that refused everything.
	atTheLimit := `{"op":"version"}`
	atTheLimit += strings.Repeat(" ", maxRequestBytes-len(atTheLimit))
	if resp := ask(t, socket, atTheLimit); !resp.OK {
		t.Errorf("a request of exactly the limit was refused: %+v", resp)
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// One byte over, written from a goroutine: the daemon answers and closes
	// as soon as it knows the request is too long, so the tail of the write
	// may well have nowhere to go.
	go func() {
		_, _ = io.WriteString(conn, strings.Repeat("a", maxRequestBytes+1)+"\n")
	}()

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("an over-length request got no response at all: %v", err)
	}
	if resp.OK {
		t.Errorf("an over-length request was reported as served: %+v", resp)
	}
	if !strings.Contains(resp.Error, strconv.Itoa(maxRequestBytes)) {
		t.Errorf("the error does not say what the limit is: %q", resp.Error)
	}
}

// A request that arrives in pieces is served. The deadline was set once per
// scan and a scan reads a whole line, so what it bounded was how long the
// request took to arrive: a client trickling one across the window was cut off
// however recently its last byte had landed, which is not what an idle timeout
// means and not what the daemon is protecting itself against.
//
// net.Pipe rather than a socket, because it is unbuffered: every byte written
// here is a read on the daemon's side, which is what makes the reset per read
// the thing under test.
func TestARequestThatArrivesInPiecesIsServed(t *testing.T) {
	d := newDaemon()
	d.idle = 100 * time.Millisecond
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go d.handle(server)

	// Longer than the timeout in total, with every gap well inside it.
	req := []byte(`{"op":"version"}` + "\n")
	for i := range req {
		if err := client.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Write(req[i : i+1]); err != nil {
			t.Fatalf("the daemon stopped reading after byte %d of %d: %v", i, len(req), err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := client.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatalf("a request that arrived in pieces got no response: %v", err)
	}
	if !resp.OK {
		t.Errorf("a request that arrived in pieces was refused: %+v", resp)
	}
}

// The other side of the same timeout: a client that opens a connection and then
// says nothing must not hold a goroutine and a descriptor for as long as it
// likes. Dropping the timeout entirely would pass the test above.
func TestAConnectionThatSaysNothingIsClosed(t *testing.T) {
	d := newDaemon()
	d.idle = 100 * time.Millisecond
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() {
		d.handle(server)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a connection that sent nothing was held open past the idle timeout")
	}
}

// A client that asks and then stops reading must not park the handler in a
// write forever. Nothing bounded a write: the read deadline says nothing about
// one, so once the socket buffer filled, Encode blocked for as long as the
// client left the connection sitting there, holding a goroutine and a
// descriptor apiece.
//
// net.Pipe is unbuffered, so the fill is immediate and the test does not have
// to guess how much a socket takes before it blocks.
func TestAClientThatStopsReadingDoesNotPinTheHandler(t *testing.T) {
	d := newDaemon()
	// Generous next to the write deadline, so a handler that comes back does so
	// because the write gave up and not because the read did.
	d.idle = 30 * time.Second
	d.write = 100 * time.Millisecond
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() {
		d.handle(server)
		close(done)
	}()

	if err := client.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, `{"op":"version"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// Deliberately no read of the response.

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the handler is still blocked writing to a client that never read")
	}
}

// testProfile registers one profile of the test's own and returns its name.
// The registry is global and built from files, exactly as it is in main.
func testProfile(t *testing.T, name string, extra ...string) string {
	t.Helper()
	dir := shortDir(t)
	spec := "name: " + name + "\n" +
		"image: ghcr.io/brig-sh/claude-code:arm64\n" +
		"guestHome: /home/agent\n" +
		"binary: agent\n" +
		"mem: 1024\ncpus: 1\n" +
		strings.Join(extra, "\n")
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(spec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(dir); err != nil {
		t.Fatal(err)
	}
	return name
}

// A daemon never prompts. The image check is the one thing in a run that stops
// to ask, and asking on the daemon's own terminal puts the question where the
// client cannot see it, holds the sandbox's lock across the wait, and times
// the client out. The refusal comes back as a request's answer instead.
func TestARequestThatWouldPromptIsRefusedRatherThanAsked(t *testing.T) {
	stubRuntime(t)
	agent := testProfile(t, "promptcheck")
	t.Setenv("BRIG_WORKSPACE", filepath.Join(shortDir(t), "ws"))
	// A real terminal on the daemon's stdin, the way starting brigd from a
	// shell leaves one, and nothing written to it. This is what makes the test
	// about the fix rather than about `go test` running without a terminal: a
	// daemon that still asks reads from here, blocks, and the request below
	// times out instead of being answered.
	ttytest.AsStdin(t)
	// An image under our own registry whose signature does not check out: the
	// one case that stops to ask.
	t.Setenv("BRIG_VERIFY", "warn")
	t.Setenv("BRIG_COSIGN_BIN", "false")

	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	resp := ask(t, socket, `{"op":"ensure","agent":"`+agent+`"}`)
	if resp.OK {
		t.Fatalf("an image that failed verification was booted: %+v", resp)
	}
	// Named, not merely refused: a client that gets "aborted" with no reason
	// has nothing to act on.
	//
	// Which check stopped it is not this test's business. A cosign that exits
	// non-zero fails the digest resolve before it reaches the signature, so on
	// a runtime that pins the digest this is "could not check" rather than
	// "failed", and the wording differs. What has to hold either way is that
	// the response says the image was not verified and names the way past it.
	if !strings.Contains(resp.Error, "verif") {
		t.Errorf("the error does not say what went wrong: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "BRIG_VERIFY=off") {
		t.Errorf("the error does not name the setting that overrides it: %q", resp.Error)
	}
	said := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(said, "could not be checked") && !strings.Contains(said, "DID NOT VERIFY") {
		t.Errorf("the client was not told what the check concluded: %q", said)
	}
	if strings.Contains(said, "Boot it anyway?") || strings.Contains(resp.Error, "Boot it anyway?") {
		t.Errorf("the daemon asked a question: %q %q", said, resp.Error)
	}
}

// A boot that went well warns about nothing. The daemon collects what a run
// says and returns it to the client as warnings, and the line narrating that
// the boot has started went into the same buffer -- so a cold boot with nothing
// wrong with it answered {"ok":true} carrying a complaint about itself, and a
// client with any rule about a non-empty warnings list acted on it.
//
// The counterpart, that a real warning still travels, is
// TestARequestThatWouldPromptIsRefusedRatherThanAsked: the image check's verdict
// reaches the client in the same field.
func TestACleanColdBootWarnsAboutNothing(t *testing.T) {
	stubRuntime(t)
	agent := testProfile(t, "cleanboot")
	t.Setenv("BRIG_WORKSPACE", filepath.Join(shortDir(t), "ws"))
	// The image check is the other thing that speaks on a cold boot, and with
	// no cosign on this machine it has something true to say. Off, so what is
	// left in the buffer is the narration this test is about -- and off says
	// so itself, which is a warning a client should get: it is a control the
	// user turned off, not something the run did along the way.
	t.Setenv("BRIG_VERIFY", "off")

	socket := filepath.Join(shortDir(t), "brigd.sock")
	startDaemon(t, socket)

	resp := ask(t, socket, `{"op":"ensure","agent":"`+agent+`"}`)
	if !resp.OK {
		t.Fatalf("the sandbox did not come up: %+v", resp)
	}
	// What this test is about: narration is not a warning. A boot that went
	// perfectly used to come back reporting "starting sandbox ..." as a
	// warning about itself, which is the confusion Config.Progress exists to
	// end. Anything a client is told here has to be something to act on.
	for _, w := range resp.Warnings {
		if strings.Contains(w, "starting sandbox") || strings.Contains(w, "workspace ") {
			t.Errorf("progress narration came back as a warning: %q", w)
		}
		if !strings.Contains(w, "BRIG_VERIFY") {
			t.Errorf("a boot with nothing wrong with it warned anyway: %q", w)
		}
	}
}

// A profile's runtimeBin has to reach the same binary through the daemon as it
// does through the CLI. It did not: the daemon detected a runtime once at
// startup, with no profile in hand to take a preference from, so a profile
// pinning a build was honoured when you typed `brig run` and ignored when a
// client asked brigd for the same sandbox.
func TestAProfilesRuntimeBinReachesTheSameBinaryThroughTheDaemon(t *testing.T) {
	// Nothing in the environment: BRIG_RUNTIME_BIN beats the profile, which is
	// the usual order, and would answer this question for the wrong reason.
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", "")
	pinned := filepath.Join(shortDir(t), "hull-of-my-own")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := testProfile(t, "pinnedruntime", "runtimeBin: "+pinned)

	cfg, _, err := newDaemon().config(Request{Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Bin() != pinned {
		t.Errorf("the daemon drives %q, want the profile's %q", cfg.Runtime.Bin(), pinned)
	}

	// The CLI's own resolution, spelled as cmd/brig spells it. The two paths
	// agreeing is the property; either one being right on its own is not.
	p, ok := profile.Lookup(agent)
	if !ok {
		t.Fatalf("profile %q did not register", agent)
	}
	rt, err := runtime.DetectFor(runtime.Preference{Bin: p.RuntimeBin})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Bin() != cfg.Runtime.Bin() {
		t.Errorf("the CLI drives %q and the daemon %q", rt.Bin(), cfg.Runtime.Bin())
	}
}

// livenessRuntime answers the one question the inventory asks. Everything else
// panics through the embedded nil interface, which is louder than a stub that
// quietly answers something the status report never asked.
type livenessRuntime struct {
	runtime.Runtime
	running bool
	err     error
}

func (l livenessRuntime) Running(string) (bool, error) { return l.running, l.err }

// The inventory re-reads liveness from the runtime on every report, so a
// runtime that cannot be asked has to be reported as unanswered rather than as
// a stopped sandbox. Folded into running:false it reads as a sandbox that
// exited, and the operator goes looking for a VM that may well still be up.
func TestStatusSaysWhenTheRuntimeCouldNotBeAsked(t *testing.T) {
	d := newDaemon()
	d.sessions["brig-broken"] = entry{
		Session: Session{Agent: "claude-code", VM: "brig-broken"},
		rt:      livenessRuntime{err: errors.New("cannot connect to the daemon")},
	}
	d.sessions["brig-up"] = entry{
		Session: Session{Agent: "claude-code", VM: "brig-up"},
		rt:      livenessRuntime{running: true},
	}

	byVM := map[string]Session{}
	for _, s := range d.status() {
		byVM[s.VM] = s
	}

	broken := byVM["brig-broken"]
	if broken.RunningError == "" {
		t.Error("a runtime that could not be asked was reported as a plain stopped sandbox")
	}
	if !strings.Contains(broken.RunningError, "cannot connect to the daemon") {
		t.Errorf("the runtime's explanation was dropped: %q", broken.RunningError)
	}
	if broken.Running {
		t.Error("a sandbox nothing could answer for was reported as running")
	}

	// The answers that were given are unchanged.
	if up := byVM["brig-up"]; !up.Running || up.RunningError != "" {
		t.Errorf("a running sandbox was misreported: %+v", up)
	}
}

// TestServeInAHelperProcess is not a test. It is how the test above starts a
// second daemon in a second process, which is the only honest way to ask the
// question: a lock is held against other processes, and this one already holds
// it.
func TestServeInAHelperProcess(t *testing.T) {
	socket := os.Getenv("BRIGD_HELPER_SOCKET")
	if socket == "" {
		t.Skip("not the helper process; see TestASecondDaemonRefusesTheSocketOfARunningOne")
	}
	// main's own exit path, so the test above asserts on what a user would see.
	if err := serve(socket); err != nil {
		_, _ = os.Stderr.WriteString("brigd: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}
