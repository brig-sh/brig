package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// A symlink or hard link planted at the lock path must be refused, not
// followed and truncated. brigd opens the lock with O_NOFOLLOW and, from the
// descriptor, refuses a file it does not own by a single name -- so the file a
// planted link points at keeps its contents. Reachable when --socket names a
// directory another local user can write.
func TestLockSocketRefusesAPlantedLink(t *testing.T) {
	dir := shortDir(t)
	socket := filepath.Join(dir, "brigd.sock")
	lockPath := socket + ".lock"

	const content = "keep me"
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	assertUntouched := func(t *testing.T) {
		t.Helper()
		b, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != content {
			t.Errorf("the file the link pointed at was written through the lock path: %q", b)
		}
	}

	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink(victim, lockPath); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(lockPath) }()
		f, err := lockSocket(socket)
		if err == nil {
			_ = f.Close()
			t.Fatal("brigd followed a symlink planted at the lock path")
		}
		if !strings.Contains(err.Error(), "planted at the lock path") {
			t.Errorf("the symlink refusal is not the diagnosable one: %v", err)
		}
		assertUntouched(t)
	})

	t.Run("hardlink", func(t *testing.T) {
		if err := os.Link(victim, lockPath); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(lockPath) }()
		f, err := lockSocket(socket)
		if err == nil {
			_ = f.Close()
			t.Fatal("brigd truncated a hard link planted at the lock path")
		}
		if !strings.Contains(err.Error(), "hard link") {
			t.Errorf("the refusal does not name the hard link: %v", err)
		}
		assertUntouched(t)
	})
}

// The link check has to run before the flock, not after: a hard link whose
// target the attacker also holds an exclusive lock on would otherwise make the
// flock fail first, so the check never runs and the holder message reads the
// linked file. With the check first, the hard link is refused whatever its lock
// state, and the target is neither read nor written.
func TestLockSocketRefusesAHardLinkEvenWhenLocked(t *testing.T) {
	dir := shortDir(t)
	socket := filepath.Join(dir, "brigd.sock")
	lockPath := socket + ".lock"

	const content = "victim-bytes-that-must-not-leak"
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, lockPath); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("could not hold the lock for the test: %v", err)
	}

	f, err := lockSocket(socket)
	if err == nil {
		_ = f.Close()
		t.Fatal("brigd used a flock-held hard link as its lock")
	}
	if !strings.Contains(err.Error(), "hard link") {
		t.Errorf("the refusal is the flock message, not the hard-link one, so the check ran too late: %v", err)
	}
	if strings.Contains(err.Error(), content) {
		t.Errorf("the target file's contents leaked into the refusal: %v", err)
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != content {
		t.Errorf("the target file was modified through the lock path: %q", b)
	}
}

// A non-regular entry that still opens read-write -- a FIFO -- is refused as
// not a regular file, so the type check covers what O_NOFOLLOW and the O_RDWR
// open do not turn away on their own.
func TestLockSocketRefusesANonRegularFile(t *testing.T) {
	socket := filepath.Join(shortDir(t), "brigd.sock")
	if err := syscall.Mkfifo(socket+".lock", 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	f, err := lockSocket(socket)
	if err == nil {
		_ = f.Close()
		t.Fatal("brigd accepted a FIFO as its lock")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal does not name the file type: %v", err)
	}
}

// The clean path still works: a lock file brigd creates itself -- a plain file
// it owns by one name -- is accepted and carries this process's pid, so the
// check the planted-link test relies on does not refuse the normal case.
func TestLockSocketAcceptsAFileItCreates(t *testing.T) {
	socket := filepath.Join(shortDir(t), "brigd.sock")
	f, err := lockSocket(socket)
	if err != nil {
		t.Fatalf("lockSocket refused a clean lock path: %v", err)
	}
	defer func() { _ = f.Close() }()
	b, err := os.ReadFile(socket + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock file does not hold this pid: %q", b)
	}
}
