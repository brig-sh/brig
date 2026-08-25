package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubRuntime points the daemon at a runtime binary that exists and does
// nothing. The machine running the tests has neither hull nor nerdctl, and
// detection is the first thing the daemon does.
func stubRuntime(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hull")
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
func TestASecondDaemonRefusesTheSocketOfARunningOne(t *testing.T) {
	stubRuntime(t)
	socket := filepath.Join(t.TempDir(), "brigd.sock")
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
