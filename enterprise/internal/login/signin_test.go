// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
	"github.com/brig-sh/brig/internal/secret/login"
)

// expiredLogin stores a login for url that ran out an hour ago.
func expiredLogin(m *memLogins, url string) {
	m.items[url] = login.Token{URL: url, AccessToken: "old-token", ExpiresAt: time.Now().Add(-time.Hour)}
}

// AccessToken says a stored login has expired, returns no token for it, and
// says nothing of the kind for a missing, empty or unexpired one.
func TestAccessTokenSaysWhenTheStoredLoginHasExpired(t *testing.T) {
	m := loginHost(t)
	a := parseDeployment(t, "https://a.example")
	cases := []struct {
		name        string
		tok         login.Token
		stored      bool
		wantExpired bool
	}{
		{"none", login.Token{}, false, false},
		{"unexpired", login.Token{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour)}, true, false},
		{"expired", login.Token{AccessToken: "t", ExpiresAt: time.Now().Add(-time.Minute)}, true, true},
		{"empty and expired", login.Token{ExpiresAt: time.Now().Add(-time.Minute)}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clear(m.items)
			if c.stored {
				c.tok.URL = a.String()
				m.items[a.String()] = c.tok
			}
			tok, v, timedOut, expired := AccessToken(a, KeyringWait)
			if expired != c.wantExpired || timedOut || v != "" || (c.wantExpired && tok != "") {
				t.Errorf("token %q, unaccepted %q, timedOut %v, expired %v; want expired %v and no token", tok, v, timedOut, expired, c.wantExpired)
			}
		})
	}
	// Another deployment's expired login says nothing about this one.
	clear(m.items)
	b := parseDeployment(t, "https://b.example")
	expiredLogin(m, b.String())
	if _, _, _, expired := AccessToken(a, KeyringWait); expired {
		t.Error("an expired login for b read as expired for a")
	}
}

// SignIn is brig login for the deployment brig login would resolve: it asks
// the question brig login asks for a deployment from the environment, uses the
// flow brig login picks for the host, stores the token as brig login does, and
// retires the expired one. AccessToken then returns the new token.
func TestSignInReplacesAnExpiredLoginWithTheFlowBrigLoginPicks(t *testing.T) {
	for _, flow := range []string{"device", "browser"} {
		t.Run(flow, func(t *testing.T) {
			m := loginHost(t)
			s := fakeDeployment(t, fakeas.Options{})
			expiredLogin(m, s.URL)
			t.Setenv(nofireauth.EnvURL, s.URL)
			t.Setenv("SSH_CONNECTION", "")
			t.Setenv("SSH_TTY", "")
			t.Setenv("DISPLAY", ":0")
			t.Setenv("WAYLAND_DISPLAY", "")
			if flow == "device" {
				t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
			}
			opened := 0
			openBrowser = func(path string) error {
				opened++
				u, err := fakeas.FollowPage(path)
				if err != nil {
					return err
				}
				resp, err := http.Get(u)
				if err != nil {
					return err
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				return resp.Body.Close()
			}
			master := terminalStdin(t)
			if _, err := master.Write([]byte("y\n")); err != nil {
				t.Fatal(err)
			}
			d := parseDeployment(t, s.URL)
			var out bytes.Buffer
			var err error
			stderr := captureStderr(t, func() { err = SignIn(&out, d) })
			if err != nil {
				t.Fatalf("SignIn: %v\n%s", err, out.String())
			}
			if !strings.Contains(stderr, "Continue logging in to "+s.URL+"? [y/N]") {
				t.Errorf("the deployment from the environment was not confirmed: %q", stderr)
			}
			if (flow == "browser") != (opened == 1) {
				t.Errorf("flow %s opened the browser %d times", flow, opened)
			}
			issued := s.Issued()
			if len(issued) != 1 {
				t.Fatalf("issued %d tokens", len(issued))
			}
			if got, _, _, expired := AccessToken(d, KeyringWait); got != issued[0] || expired {
				t.Errorf("AccessToken = %q, expired %v; want the new token", got, expired)
			}
			if time.Until(m.items[s.URL].ExpiresAt) < 6*24*time.Hour {
				t.Errorf("expires %s, want the default lifetime", m.items[s.URL].ExpiresAt)
			}
			if !slices.Equal(s.Revoked(), []string{"old-token"}) {
				t.Errorf("revoked %v, want the expired token", s.Revoked())
			}
			if strings.Contains(out.String(), issued[0]) {
				t.Error("the token was printed")
			}
		})
	}
}

// A deployment other than the one brig login would use is refused before
// anything is sent, so the sign-in cannot be pointed somewhere else.
func TestSignInRefusesADeploymentBrigLoginWouldNotUse(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	err := SignIn(io.Discard, parseDeployment(t, s.URL)) // BRIG_NOFIRE_URL is unset: the default is the one
	if err == nil {
		t.Fatal("signed in to a deployment brig login would not use")
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests %v", r)
	}
}

// With no terminal a deployment from the environment cannot be confirmed, so
// there is no sign-in, as for brig login: nothing is sent.
func TestSignInWithoutATerminalCannotConfirmADeploymentFromTheEnvironment(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	err := SignIn(io.Discard, parseDeployment(t, s.URL))
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("err = %v, want the needs-a-terminal refusal", err)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests %v", r)
	}
}

// interruptingOpener is a browser that does not open: it presses Ctrl-C.
func interruptingOpener() nofireauth.Opener {
	return func(string) error { return syscall.Kill(os.Getpid(), syscall.SIGINT) }
}

// The context of a sign-in is ended by the first Ctrl-C and goes on catching
// the ones after it, so none of them reaches the default and ends Brig.
func TestHeldContextKeepsCatchingCtrlCAfterTheFirst(t *testing.T) {
	ctx, stop := heldContext()
	defer stop()
	for i := range 3 {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the first Ctrl-C did not end the context")
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Ctrl-C during the sign-in cancels the sign-in. It does not end the process,
// and nothing is stored or issued.
func TestCtrlCDuringSignInCancelsOnlyTheSignIn(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	openBrowser = interruptingOpener()
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || err.Error() != "login cancelled" {
		t.Fatalf("err = %v, want login cancelled", err)
	}
	if len(s.Issued()) != 0 || m.items[s.URL].AccessToken != "old-token" {
		t.Errorf("issued %d, stored %+v", len(s.Issued()), m.items[s.URL])
	}
}

// Brig can be started with SIGINT ignored, by a script that wants its
// children to outlive Ctrl-C. A sign-in catches Ctrl-C while it runs and
// ignores it again when it returns, so the command that starts next inherits
// what Brig inherited. The test runs in a child process that starts that way.
func TestSignInLeavesAnIgnoredSIGINTIgnored(t *testing.T) {
	if os.Getenv("BRIG_TEST_SIGNIN_IGNORED_CHILD") == "1" {
		t.Skip("this is the parent half")
	}
	cmd := exec.Command("sh", "-c", `trap '' INT; exec "$0" -test.run='^TestSignInIgnoredChild$' -test.v`, os.Args[0])
	cmd.Env = append(os.Environ(), "BRIG_TEST_SIGNIN_IGNORED_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestSignInIgnoredChild") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

// TestSignInIgnoredChild is the half that runs with SIGINT ignored from the
// start. Run alone, it does nothing.
func TestSignInIgnoredChild(t *testing.T) {
	if os.Getenv("BRIG_TEST_SIGNIN_IGNORED_CHILD") != "1" {
		t.Skip("runs only from TestSignInLeavesAnIgnoredSIGINTIgnored")
	}
	if !signal.Ignored(syscall.SIGINT) {
		t.Fatal("SIGINT was not ignored at start")
	}
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	openBrowser = interruptingOpener()
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || err.Error() != "login cancelled" {
		t.Fatalf("err = %v, want login cancelled: Ctrl-C did not reach the sign-in", err)
	}
	// The signal is ignored again, as the process inherited it: this one does
	// not end the process, and the test ends with PASS only if it survives.
	// signal.Ignored cannot say, because it does not see a restored disposition.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
}
