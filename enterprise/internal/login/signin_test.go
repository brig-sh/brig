// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
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
			if !strings.Contains(out.String(), "Continue logging in to "+s.URL+"? [y/N]") {
				t.Errorf("the deployment from the environment was not confirmed: %q", out.String())
			}
			if stderr != "" {
				t.Errorf("SignIn wrote to stderr, not only to its writer: %q", stderr)
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

// A second Ctrl-C during the sign-in must not end Brig: SignIn keeps catching
// Ctrl-C until it returns. The signals arrive while the y/N question waits,
// because SignIn cannot return before it is answered. With a context that
// stops catching after the first Ctrl-C, the second takes the default action
// and kills this process, so the test passes only if the process survives to
// see the error.
func TestSignInSurvivesASecondCtrlC(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	master := terminalStdin(t)
	go func() {
		for range 2 {
			time.Sleep(300 * time.Millisecond)
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = master.Write([]byte("y\n"))
	}()
	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || err.Error() != "login cancelled" {
		t.Fatalf("err = %v, want login cancelled", err)
	}
	if len(s.Issued()) != 0 || m.items[s.URL].AccessToken != "old-token" {
		t.Errorf("issued %d, stored %+v", len(s.Issued()), m.items[s.URL])
	}
}

// Ctrl-C while the y/N question waits for its answer takes effect once the
// question is answered: even with "y", the sign-in is cancelled before the
// keychain is opened and before the deployment is contacted.
func TestCtrlCWhileAQuestionIsAnsweredCancelsTheSignIn(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	opened := 0
	inner := openLoginStore
	openLoginStore = func() (login.Store, error) { opened++; return inner() }
	master := terminalStdin(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(300 * time.Millisecond)
		_, _ = master.Write([]byte("y\n"))
	}()
	var err error
	var out bytes.Buffer
	captureStderr(t, func() { err = SignIn(&out, parseDeployment(t, s.URL)) })
	if !strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("the question was not asked: %q", out.String())
	}
	if err == nil || err.Error() != "login cancelled" {
		t.Fatalf("err = %v, want login cancelled", err)
	}
	if opened != 0 {
		t.Error("the keychain was opened after the sign-in was cancelled")
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("the deployment was contacted: %v", r)
	}
	if len(s.Issued()) != 0 || m.items[s.URL].AccessToken != "old-token" {
		t.Errorf("issued %d, stored %+v", len(s.Issued()), m.items[s.URL])
	}
}

// SignIn prints everything to the writer it is given: the questions about the
// environment and the note about an earlier token that could not be revoked.
// A caller whose stderr is not a terminal hands it the terminal, so nothing is
// written to the process's stderr.
func TestSignInWritesQuestionsAndNotesToItsWriterAndNotToStderr(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{RevokeStatus: http.StatusServiceUnavailable})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = SignIn(&out, parseDeployment(t, s.URL)) })
	if err != nil {
		t.Fatalf("SignIn: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Continue logging in to " + s.URL + "? [y/N]", "brig: cannot revoke the earlier token for " + s.URL} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the writer got %q, want %q in it", out.String(), want)
		}
	}
	if stderr != "" {
		t.Errorf("stderr got %q", stderr)
	}
}

// slowStore blocks a keychain call until the test releases it, as a keychain
// does that waits on an unlock prompt nobody sees. Its backend takes no
// context, so only the bound SignIn puts around the call can end the wait.
type slowStore struct {
	*memLogins
	blockSave bool
	release   chan struct{}
}

func (s *slowStore) Save(t login.Token) error {
	if s.blockSave {
		<-s.release
	}
	return s.memLogins.Save(t)
}

// signInHost sets up an expired login, a device flow and the answer to the
// question about a deployment from the environment.
func signInHost(t *testing.T) (*memLogins, *fakeas.Server) {
	t.Helper()
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	expiredLogin(m, s.URL)
	t.Setenv(nofireauth.EnvURL, s.URL)
	t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	return m, s
}

// A keychain that does not finish the save does not hold the sign-in for ever:
// the sign-in fails after the bound, the token the deployment issued is
// revoked, and nothing waits on the keychain.
func TestSignInEndsWhenTheKeychainDoesNotFinishTheSave(t *testing.T) {
	m, s := signInHost(t)
	st := &slowStore{memLogins: m, blockSave: true, release: make(chan struct{})}
	t.Cleanup(func() { close(st.release) })
	openLoginStore = func() (login.Store, error) { return st, nil }
	old := signInKeychainWait
	signInKeychainWait = 300 * time.Millisecond
	t.Cleanup(func() { signInKeychainWait = old })

	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || !strings.Contains(err.Error(), "cannot store the token") || !strings.Contains(err.Error(), "did not answer") || !strings.Contains(err.Error(), "Brig revoked the token") {
		t.Fatalf("err = %v, want the keychain not answering, and the token revoked", err)
	}
	issued := s.Issued()
	if len(issued) != 1 || !slices.Contains(s.Revoked(), issued[0]) {
		t.Errorf("issued %v, revoked %v; want the new token revoked", issued, s.Revoked())
	}
}

// Ctrl-C while the keychain blocks on the save ends the sign-in at once, and
// revokes the token, instead of being caught and dropped.
func TestCtrlCEndsASignInThatWaitsOnTheKeychain(t *testing.T) {
	m, s := signInHost(t)
	st := &slowStore{memLogins: m, blockSave: true, release: make(chan struct{})}
	t.Cleanup(func() { close(st.release) })
	openLoginStore = func() (login.Store, error) { return st, nil }
	old := signInKeychainWait
	signInKeychainWait = 10 * time.Second
	t.Cleanup(func() { signInKeychainWait = old })
	openBrowser = nil
	go func() {
		// Once the token is issued the sign-in is in the blocked save.
		for len(s.Issued()) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	start := time.Now()
	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || !strings.Contains(err.Error(), "Brig revoked the token") {
		t.Fatalf("err = %v, want the sign-in ended and the token revoked", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the sign-in took %s after Ctrl-C", time.Since(start))
	}
	if issued := s.Issued(); !slices.Contains(s.Revoked(), issued[0]) {
		t.Errorf("issued %v, revoked %v", issued, s.Revoked())
	}
}

// A keychain that does not open ends the sign-in before the deployment is
// contacted.
func TestSignInEndsWhenTheKeychainDoesNotOpen(t *testing.T) {
	_, s := signInHost(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	openLoginStore = func() (login.Store, error) { <-release; return nil, errors.New("late") }
	old := signInKeychainWait
	signInKeychainWait = 300 * time.Millisecond
	t.Cleanup(func() { signInKeychainWait = old })
	var err error
	captureStderr(t, func() { err = SignIn(io.Discard, parseDeployment(t, s.URL)) })
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v, want the keychain not answering", err)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("the deployment was contacted: %v", r)
	}
}
