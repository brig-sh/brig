package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// longSocket builds a socket path one byte over the limit, under a directory
// short enough that the padding is what makes it too long.
func longSocket(t *testing.T, dir string) string {
	t.Helper()
	name := strings.Repeat("s", maxSocketPath-len(dir))
	path := filepath.Join(dir, name)
	if len(path) != maxSocketPath+1 {
		t.Fatalf("the test built a %d byte path, wanted %d", len(path), maxSocketPath+1)
	}
	return path
}

// maxSocketPath has to be the kernel's limit rather than a number somebody
// wrote down: a refusal that names the wrong one either turns away a path that
// would have worked or lets through the "bind: invalid argument" it exists to
// replace. So this asserts against bind itself, on the platform running the
// test.
func TestMaxSocketPathIsWhatTheKernelAccepts(t *testing.T) {
	dir := shortDir(t)

	atLimit := filepath.Join(dir, strings.Repeat("s", maxSocketPath-len(dir)-1))
	ln, err := net.Listen("unix", atLimit)
	if err != nil {
		t.Fatalf("a %d byte path was refused by bind, so the limit is lower than "+
			"maxSocketPath = %d: %v", len(atLimit), maxSocketPath, err)
	}
	_ = ln.Close()

	overLimit := longSocket(t, dir)
	ln, err = net.Listen("unix", overLimit)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("a %d byte path was bound, so the limit is higher than "+
			"maxSocketPath = %d", len(overLimit), maxSocketPath)
	}
	// The error this whole change exists to replace.
	if !strings.Contains(err.Error(), "invalid argument") {
		t.Logf("bind refused the over-long path with %v, not \"invalid argument\"", err)
	}
}

// An over-long --socket used to reach bind and come back as "bind: invalid
// argument", which names neither the path, nor the limit, nor the fact that
// length is what is wrong with it.
func TestAnOverlongSocketFlagIsRefusedByName(t *testing.T) {
	path := longSocket(t, shortDir(t))

	socket, err := chooseSocket(path)
	if err == nil {
		t.Fatalf("a %d byte socket path was accepted: %s", len(path), socket)
	}
	msg := err.Error()
	for _, want := range []string{
		path,                        // which path
		strconv.Itoa(len(path)),     // how long it is
		strconv.Itoa(maxSocketPath), // what the limit is
		"--socket",                  // what supplied it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "invalid argument") {
		t.Errorf("the refusal still hands back the kernel's error: %s", msg)
	}
}

// The default path is chosen from XDG_RUNTIME_DIR, which can be as long as
// whoever set it made it, so the check has to cover the path brigd picks for
// itself as well as the one it is given.
func TestAnOverlongDefaultSocketIsRefusedByName(t *testing.T) {
	dir := shortDir(t)
	// One byte over, once brigd.sock is joined onto it.
	runtimeDir := filepath.Join(dir, strings.Repeat("d", maxSocketPath-len(dir)-len("brigd.sock")))
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	socket, err := chooseSocket("")
	if err == nil {
		t.Fatalf("an over-long default socket path was accepted: %s", socket)
	}
	msg := err.Error()
	if !strings.Contains(msg, "XDG_RUNTIME_DIR") {
		t.Errorf("the refusal does not name the setting that supplied the path: %s", msg)
	}
	if !strings.Contains(msg, strconv.Itoa(maxSocketPath)) {
		t.Errorf("the refusal does not name the limit: %s", msg)
	}
}

// A path within the limit is returned untouched, from either source.
func TestASocketPathWithinTheLimitIsAccepted(t *testing.T) {
	dir := shortDir(t)

	given := filepath.Join(dir, "brigd.sock")
	got, err := chooseSocket(given)
	if err != nil {
		t.Fatalf("chooseSocket refused %s: %v", given, err)
	}
	if got != given {
		t.Errorf("chooseSocket(%q) = %q", given, got)
	}

	t.Setenv("XDG_RUNTIME_DIR", dir)
	got, err = chooseSocket("")
	if err != nil {
		t.Fatalf("chooseSocket refused the default: %v", err)
	}
	if want := filepath.Join(dir, "brigd.sock"); got != want {
		t.Errorf("the default socket is %q, want %q", got, want)
	}
}
