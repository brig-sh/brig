package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/brig-sh/brig/internal/brigsock"
	"github.com/brig-sh/brig/internal/buildinfo"
)

// The report opens with the binary that produced it, so a bug report carries
// the build without a second command.
func TestDoctorNamesItsOwnBuild(t *testing.T) {
	healthyHost(t)
	checks := runDoctor(nil, nil)
	if checks[0].Name != "brig" {
		t.Errorf("the first row is %q, want brig", checks[0].Name)
	}
	c := findCheck(t, checks, "brig")
	if c.State != statePass {
		t.Errorf("brig row is %q, want ok", c.State)
	}
	if want := buildinfo.Read().String(); c.Finding != want {
		t.Errorf("brig row = %q, want %q", c.Finding, want)
	}
}

// A running daemon is asked which build it is. The same build as this brig is
// fine; a different one is a failure whose fix is to restart it, because a
// daemon left up across an upgrade serves the old code with no other sign.
func TestDoctorComparesTheRunningBrigdBuild(t *testing.T) {
	mine := buildinfo.Read()
	for _, c := range []struct {
		name   string
		answer string
		state  checkState
		want   []string
	}{
		{"same build",
			`{"v":1,"ok":true,"code":0,"version":"` + mine.Version + `","commit":"` + mine.Commit + `"}`,
			statePass, []string{"build " + mine.Version}},
		{"no commit to compare",
			`{"v":1,"ok":true,"code":0,"version":"` + mine.Version + `","commit":"abcdef0123456789abcdef0123456789abcdef01"}`,
			statePass, []string{"build " + mine.Version}},
		{"different build",
			`{"v":1,"ok":true,"code":0,"version":"v9.9.9","commit":"abcdef0123456789abcdef0123456789abcdef01"}`,
			stateFail, []string{"build v9.9.9 (abcdef0)"}},
		{"no answer",
			`not json`,
			statePass, []string{"did not answer"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			healthyHost(t)
			fakeBrigd(t, c.answer)
			got := brigdCheck()
			if got.State != c.state {
				t.Errorf("state = %q, want %q: %s", got.State, c.state, got.Finding)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Finding, w) {
					t.Errorf("finding %q does not say %q", got.Finding, w)
				}
			}
			if c.state == stateFail && !strings.Contains(got.Fix, "restart") {
				t.Errorf("fix %q does not say to restart brigd", got.Fix)
			}
		})
	}
}

// Two builds are the same code when the versions match and, where both name a
// commit, the commits and the modified flags match too. A build with no commit
// -- from `go install` -- is not a mismatch a restart could fix.
func TestSameBuild(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, c := range []struct {
		name string
		x, y buildinfo.Info
		want bool
	}{
		{"same", buildinfo.Info{Version: "v0.3.0", Commit: a}, buildinfo.Info{Version: "v0.3.0", Commit: a}, true},
		{"other version", buildinfo.Info{Version: "v0.3.0", Commit: a}, buildinfo.Info{Version: "v0.3.1", Commit: a}, false},
		{"other commit", buildinfo.Info{Version: "dev", Commit: a}, buildinfo.Info{Version: "dev", Commit: b}, false},
		{"one modified", buildinfo.Info{Version: "dev", Commit: a, Modified: true}, buildinfo.Info{Version: "dev", Commit: a}, false},
		{"one without a commit", buildinfo.Info{Version: "v0.3.0"}, buildinfo.Info{Version: "v0.3.0", Commit: a}, true},
		{"without a commit, other version", buildinfo.Info{Version: "v0.3.0"}, buildinfo.Info{Version: "v0.3.1", Commit: a}, false},
	} {
		if got := sameBuild(c.x, c.y); got != c.want {
			t.Errorf("%s: sameBuild = %v, want %v", c.name, got, c.want)
		}
	}
}

// A modified build the version does not already mark says so, so a mismatch
// on the modified flag alone does not read as two identical labels.
func TestBuildLabel(t *testing.T) {
	const rev = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	for _, c := range []struct {
		info buildinfo.Info
		want string
	}{
		{buildinfo.Info{Version: "v0.3.0"}, "v0.3.0"},
		{buildinfo.Info{Version: "v0.3.0", Commit: rev}, "v0.3.0 (b2b2b2b)"},
		{buildinfo.Info{Version: "v0.3.0+dirty", Commit: rev, Modified: true}, "v0.3.0+dirty (b2b2b2b)"},
		{buildinfo.Info{Version: "dev", Commit: rev, Modified: true}, "dev (b2b2b2b, modified)"},
	} {
		if got := buildLabel(c.info); got != c.want {
			t.Errorf("buildLabel(%+v) = %q, want %q", c.info, got, c.want)
		}
	}
}

// fakeBrigd serves the default socket the way a daemon would -- 0600, the
// lock file held -- and answers every request with one canned line.
//
// The socket lives in a directory of its own under the system temp root rather
// than t.TempDir(): a unix socket path has a kernel limit brigsock.MaxPath
// names, and the test binary's temp paths are longer than it on macOS.
func fakeBrigd(t *testing.T, answer string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "brigd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket, _ := brigsock.Default()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(socket+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
					return
				}
				_, _ = conn.Write([]byte(answer + "\n"))
			}()
		}
	}()
	// The pid beside the lock, as brigd writes it, so the row names one.
	_, _ = fmt.Fprintln(lock, os.Getpid())
}
