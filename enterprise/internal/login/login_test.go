// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
	"github.com/brig-sh/brig/internal/cli"
	"github.com/brig-sh/brig/internal/exitcode"
	"github.com/brig-sh/brig/internal/secret/login"
	"github.com/brig-sh/brig/internal/ttytest"
)

// The exit statuses the verbs' errors give, as the command line reads them.
var exitCode = cli.ExitCode

const (
	exitFailure = exitcode.Failure
	exitUsage   = exitcode.Usage
)

// memLogins stands in for the keychain: internal/secret/login has its own
// tests, and these tests are about the verbs.
type memLogins struct {
	items                       map[string]login.Token
	saveErr, loadErr, deleteErr error
}

var _ login.Store = (*memLogins)(nil)

func (m *memLogins) Save(t login.Token) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.items[t.URL] = t
	return nil
}

func (m *memLogins) Load(url string) (login.Token, error) {
	if m.loadErr != nil {
		return login.Token{}, m.loadErr
	}
	t, ok := m.items[url]
	if !ok {
		return login.Token{}, login.ErrNotFound
	}
	// As the keychain store does: an item that names another deployment
	// is an error, not a login.
	if t.URL != url {
		return login.Token{}, errors.New("the stored login for " + url + " names another deployment")
	}
	return t, nil
}

func (m *memLogins) Delete(url string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	if _, ok := m.items[url]; !ok {
		return login.ErrNotFound
	}
	delete(m.items, url)
	return nil
}

// loginHost gives a test an in-memory store, a device flow that does not
// wait, an opener that fails the test unless the test replaces it, and no
// BRIG_NOFIRE_URL from the developer's shell.
func loginHost(t *testing.T) *memLogins {
	t.Helper()
	m := &memLogins{items: map[string]login.Token{}}
	oldStore, oldOpen, oldSleep := openLoginStore, openBrowser, loginSleep
	openLoginStore = func() (login.Store, error) { return m, nil }
	openBrowser = func(string) error { t.Error("a browser was opened"); return nil }
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { openLoginStore, openBrowser, loginSleep = oldStore, oldOpen, oldSleep })
	t.Setenv(nofireauth.EnvURL, "")
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	pipeStdin(t, "")
	return m
}

// fakeDeployment starts a fake authorization server. Its own poll interval
// is lowered so the device flow, which does not wait under test, is not told
// to slow down.
func fakeDeployment(t *testing.T, o fakeas.Options) *fakeas.Server {
	t.Helper()
	o.Interval = time.Nanosecond
	s, err := fakeas.Start("127.0.0.1:0", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestLoginDeviceStoresToken(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	var out bytes.Buffer
	if err := loginCmd(&out, []string{s.URL + "/", "--device"}); err != nil {
		t.Fatal(err)
	}
	issued := s.Issued()
	tok, ok := m.items[s.URL]
	if !ok || len(issued) != 1 || tok.AccessToken != issued[0] || tok.URL != s.URL {
		t.Fatalf("stored %+v under %v, issued %d", tok, slices.Collect(maps.Keys(m.items)), len(issued))
	}
	if d := time.Until(tok.ExpiresAt); d < 6*24*time.Hour || d > 7*24*time.Hour {
		t.Errorf("expires in %s, want the 7 days granted", d)
	}
	want := "Logged in to " + s.URL + ". Token expires " + tok.ExpiresAt.Format("2006-01-02") + "."
	if !strings.Contains(out.String(), "Logging in to "+s.URL+" (from the command line)") || !strings.Contains(out.String(), want) {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
	if strings.Contains(out.String(), issued[0]) {
		t.Error("the token was printed")
	}
}

func TestLoginBrowserStoresToken(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
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
	if err := loginCmd(io.Discard, []string{s.URL, "--browser"}); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || len(s.Issued()) != 1 || m.items[s.URL].AccessToken != s.Issued()[0] {
		t.Errorf("opened %d, stored %+v", opened, m.items)
	}
}

// Without a keychain there is nowhere to keep a token, so nothing is asked
// for: a token issued and then dropped is a credential nobody can revoke.
func TestStoreUnavailableFailsFirst(t *testing.T) {
	loginHost(t)
	openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
	s := fakeDeployment(t, fakeas.Options{})
	err := loginCmd(io.Discard, []string{s.URL, "--device"})
	if !errors.Is(err, login.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

func TestStoreWriteFailureRevokes(t *testing.T) {
	m := loginHost(t)
	m.saveErr = errors.New("keychain is locked")
	s := fakeDeployment(t, fakeas.Options{})
	err := loginCmd(io.Discard, []string{s.URL, "--device"})
	if err == nil || !strings.Contains(err.Error(), "keychain is locked") {
		t.Fatalf("err = %v", err)
	}
	if issued, revoked := s.Issued(), s.Revoked(); len(issued) != 1 || !slices.Equal(revoked, issued) {
		t.Errorf("issued %d, revoked %d: the issued token was not revoked", len(issued), len(revoked))
	}
	if strings.Contains(err.Error(), s.Issued()[0]) {
		t.Error("the error names the token")
	}
}

// A repository's .envrc can set BRIG_NOFIRE_URL, so a URL from the
// environment is named and confirmed before anything is sent to it.
func TestEnvURLAsksOnATerminal(t *testing.T) {
	loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv(nofireauth.EnvURL, s.URL)
	master := terminalStdin(t)
	if _, err := master.Write([]byte("n\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, []string{"--device"}) })
	if err == nil || exitCode(err) != exitFailure {
		t.Errorf("err = %v (exit %d), want exit 1", err, exitCode(err))
	}
	if !strings.Contains(stderr, "Continue logging in to "+s.URL+"? [y/N]") {
		t.Errorf("no question asked: %q %q", out.String(), stderr)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

// A script has nobody to confirm a deployment the environment named, so the
// login is refused before the keychain or the deployment is touched. A
// script that means it passes the URL as the argument.
func TestEnvURLRefusedWithoutATerminal(t *testing.T) {
	m := loginHost(t)
	opened := false
	openLoginStore = func() (login.Store, error) { opened = true; return m, nil }
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv(nofireauth.EnvURL, s.URL)
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, []string{"--device"}) })
	want := "BRIG_NOFIRE_URL names " + s.URL + ", and brig needs a terminal to confirm it. Pass the URL as an argument to log in without one"
	if err == nil || err.Error() != want || exitCode(err) != exitFailure {
		t.Errorf("err = %v (exit %d), want %q", err, exitCode(err), want)
	}
	if strings.Contains(out.String()+stderr, "Continue logging in") {
		t.Errorf("asked with no terminal: %q %q", out.String(), stderr)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
	if opened || len(m.items) != 0 {
		t.Errorf("the store was opened (%v) or written: %v", opened, m.items)
	}
}

// The argument wins over the environment, and the user typed it, so there
// is nothing to confirm. An answer is typed in case the code asks anyway, so
// a wrong question fails the test instead of hanging it.
func TestURLArgumentBeatsEnvWithoutAsking(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv(nofireauth.EnvURL, "https://nofire.example.internal")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("n\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, []string{s.URL, "--device"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String()+stderr, "Continue logging in") {
		t.Errorf("asked about a URL typed on the line: %q %q", out.String(), stderr)
	}
	if !strings.Contains(out.String(), "Logging in to "+s.URL+" (from the command line)") {
		t.Errorf("out = %q", out.String())
	}
	if _, ok := m.items[s.URL]; !ok {
		t.Error("nothing stored")
	}
}

// A second login replaces the stored token, and nothing here could log the
// first one out afterwards, so it is revoked.
func TestLoginAgainRevokesTheEarlierToken(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	for range 2 {
		if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
			t.Fatal(err)
		}
	}
	issued := s.Issued()
	if len(issued) != 2 || !slices.Equal(s.Revoked(), issued[:1]) {
		t.Errorf("issued %d, revoked %d; want the first one revoked", len(issued), len(s.Revoked()))
	}
	if m.items[s.URL].AccessToken != issued[1] {
		t.Error("the new token is not the stored one")
	}
}

// The new login worked, so a revocation that fails is a note, not a failure.
func TestLoginAgainRevokeFailsIsANote(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{RevokeStatus: http.StatusServiceUnavailable})
	var err error
	stderr := captureStderr(t, func() {
		for range 2 {
			if err = loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("err = %v, want exit 0", err)
	}
	want := "cannot revoke the earlier token for " + s.URL + ": "
	if !strings.Contains(stderr, want) || !strings.Contains(stderr, "Revoke it from the dashboard") {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if m.items[s.URL].AccessToken != s.Issued()[1] {
		t.Error("the new token is not the stored one")
	}
}

// The default deployment is not one a repository can slip in, so naming it
// in the environment asks nothing. The store is made unavailable so the test
// stops before any request leaves the machine: reaching the store at all
// shows the question was skipped.
func TestEnvNamingDefaultDoesNotAsk(t *testing.T) {
	loginHost(t)
	openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
	t.Setenv(nofireauth.EnvURL, "my.nofire.ai")
	terminalStdin(t)
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, nil) })
	if !errors.Is(err, login.ErrUnsupported) {
		t.Errorf("err = %v, want it to reach the store", err)
	}
	if strings.Contains(out.String()+stderr, "Continue logging in") {
		t.Errorf("asked about the default deployment: %q %q", out.String(), stderr)
	}
	if !strings.Contains(out.String(), "Logging in to "+nofireauth.DefaultURL+" (from BRIG_NOFIRE_URL)") {
		t.Errorf("out = %q", out.String())
	}
}

func TestBrowserAndDeviceTogetherRefused(t *testing.T) {
	loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	err := loginCmd(io.Discard, []string{s.URL, "--browser", "--device"})
	if exitCode(err) != exitUsage {
		t.Errorf("err = %v (exit %d), want a usage error", err, exitCode(err))
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

// Asking for more than the deployment's maximum is not an error: the
// deployment grants its maximum, and Brig says so.
func TestTTLGrantedLessIsNoted(t *testing.T) {
	loginHost(t)
	s := fakeDeployment(t, fakeas.Options{MaxTTL: 30 * 24 * time.Hour})
	var out bytes.Buffer
	if err := loginCmd(&out, []string{s.URL, "--device", "--ttl", "90d"}); err != nil {
		t.Fatal(err)
	}
	if got := s.RequestedTTL(); !slices.Equal(got, []int{90 * 24 * 3600}) {
		t.Errorf("requested ttl %v", got)
	}
	if want := s.URL + " granted 30d, less than the 90d that --ttl asked for."; !strings.Contains(out.String(), want) {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
}

func TestLogoutRevokesAndDeletes(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
		t.Fatal(err)
	}
	if err := logoutCmd(io.Discard, []string{s.URL}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.items[s.URL]; ok {
		t.Error("the token is still stored")
	}
	if issued, revoked := s.Issued(), s.Revoked(); !slices.Equal(revoked, issued) {
		t.Errorf("revoked %d of %d", len(revoked), len(issued))
	}
}

func TestLogoutNotLoggedIn(t *testing.T) {
	loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	var out bytes.Buffer
	if err := logoutCmd(&out, []string{s.URL}); err != nil {
		t.Fatalf("err = %v, want exit 0", err)
	}
	if want := "Not logged in to " + s.URL + "."; !strings.Contains(out.String(), want) {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

// The local copy goes whatever the server says: keeping it would leave a
// token here that the user asked to be rid of. The exit status still tells a
// script that the token may be live.
func TestLogoutRevokeFailsStillDeletes(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{RevokeStatus: http.StatusServiceUnavailable})
	if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
		t.Fatal(err)
	}
	err := logoutCmd(io.Discard, []string{s.URL})
	if err == nil || exitCode(err) != exitFailure {
		t.Fatalf("err = %v, want exit 1", err)
	}
	want := "brig removed the token here, but " + s.URL + " did not confirm the revocation: "
	if !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), ". Revoke it from the dashboard") {
		t.Errorf("err = %q", err)
	}
	if _, ok := m.items[s.URL]; ok {
		t.Error("the token is still stored")
	}
}

// A login that cannot be read cannot be revoked, but it still goes, so it
// does not stay stuck in the keychain.
func TestLogoutUnreadableLoginIsRemoved(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	m.items[s.URL] = login.Token{URL: s.URL}
	m.loadErr = errors.New("the stored login is unreadable")
	err := logoutCmd(io.Discard, []string{s.URL})
	want := "the stored login for " + s.URL + " is unreadable, so brig removed it without revoking the token. Revoke the token from the dashboard"
	if err == nil || err.Error() != want || exitCode(err) != exitFailure {
		t.Errorf("err = %v (exit %d), want %q", err, exitCode(err), want)
	}
	if _, ok := m.items[s.URL]; ok {
		t.Error("the unreadable login is still stored")
	}
}

// When both the revocation and the removal fail, both are said: the token
// may be live on the server and still here.
func TestLogoutReportsRevokeAndDeleteFailures(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{RevokeStatus: http.StatusServiceUnavailable})
	if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
		t.Fatal(err)
	}
	m.deleteErr = errors.New("keychain is locked")
	err := logoutCmd(io.Discard, []string{s.URL})
	if err == nil || !strings.Contains(err.Error(), "keychain is locked") || !strings.Contains(err.Error(), "did not confirm the revocation") {
		t.Errorf("err = %v, want both failures", err)
	}
}

func TestLoginOneDeploymentKeepsAnother(t *testing.T) {
	m := loginHost(t)
	a := fakeDeployment(t, fakeas.Options{})
	b := fakeDeployment(t, fakeas.Options{})
	for _, s := range []*fakeas.Server{a, b} {
		if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := logoutCmd(io.Discard, []string{b.URL}); err != nil {
		t.Fatal(err)
	}
	if tok, ok := m.items[a.URL]; !ok || tok.AccessToken != a.Issued()[0] {
		t.Errorf("A's login = %+v, %v; want it intact", tok, ok)
	}
	if _, ok := m.items[b.URL]; ok {
		t.Error("B's login is still stored")
	}
	if len(a.Revoked()) != 0 {
		t.Error("logging out of B revoked A's token")
	}
}

// interruptAfterGrant starts a fake behind a proxy that presses Ctrl-C after
// the fake has issued a token and before Brig reads it. The interrupt is a
// real SIGINT, delivered through brig login's own signal context; the proxy
// answers only once that context is done.
func interruptAfterGrant(t *testing.T, o fakeas.Options) (*fakeas.Server, string) {
	t.Helper()
	ctxs := make(chan context.Context, 1)
	old := loginContext
	loginContext = func() (context.Context, context.CancelFunc) {
		ctx, stop := old()
		ctxs <- ctx
		return ctx, stop
	}
	t.Cleanup(func() { loginContext = old })

	var target *url.URL
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp := httputil.NewSingleHostReverseProxy(target)
		if r.URL.Path != "/oauth/token" {
			rp.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		rp.ServeHTTP(rec, r)
		ctx := <-ctxs
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Errorf("SIGINT: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			t.Error("the SIGINT did not end brig login's context")
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(proxy.Close)
	o.Issuer, o.EndpointBase, o.VerificationURI = proxy.URL, proxy.URL, proxy.URL+"/device"
	s := fakeDeployment(t, o)
	target, _ = url.Parse(s.URL)
	return s, proxy.URL
}

// Ctrl-C after the deployment issued the token: the user asked to stop, so
// the token is revoked, not saved.
func TestInterruptAfterGrantRevokesTheToken(t *testing.T) {
	m := loginHost(t)
	s, base := interruptAfterGrant(t, fakeas.Options{})
	err := loginCmd(io.Discard, []string{base, "--device"})
	if err == nil || err.Error() != "login cancelled. Brig revoked the token the deployment issued" {
		t.Fatalf("err = %v, want the cancelled-and-revoked error", err)
	}
	issued := s.Issued()
	if len(issued) != 1 || !slices.Equal(s.Revoked(), issued) || len(m.items) != 0 {
		t.Errorf("issued %d, revoked %d, stored %d; want one token issued, revoked and not stored",
			len(issued), len(s.Revoked()), len(m.items))
	}
}

// When the revocation fails too, the user is told the token is still live.
func TestInterruptAfterGrantSaysWhenRevokeFails(t *testing.T) {
	m := loginHost(t)
	s, base := interruptAfterGrant(t, fakeas.Options{RevokeStatus: http.StatusInternalServerError})
	err := loginCmd(io.Discard, []string{base, "--device"})
	if err == nil || !strings.Contains(err.Error(), "could not revoke") || !strings.Contains(err.Error(), "dashboard") {
		t.Fatalf("err = %v, want one saying the token could not be revoked", err)
	}
	if len(s.Issued()) != 1 || len(m.items) != 0 || strings.Contains(err.Error(), s.Issued()[0]) {
		t.Errorf("issued %d, stored %d, or the error names the token", len(s.Issued()), len(m.items))
	}
}

// An item that names another deployment is treated as unreadable: removed,
// never revoked, and exit 1. Its token is not sent anywhere.
func TestLogoutRemovesALoginThatNamesAnotherDeployment(t *testing.T) {
	m := loginHost(t)
	s := fakeDeployment(t, fakeas.Options{})
	m.items[s.URL] = login.Token{URL: "https://evil.example", AccessToken: "planted"}
	err := logoutCmd(io.Discard, []string{s.URL})
	if err == nil || !strings.Contains(err.Error(), "unreadable") || exitCode(err) != exitFailure {
		t.Errorf("err = %v (exit %d), want the unreadable-login error", err, exitCode(err))
	}
	if _, ok := m.items[s.URL]; ok {
		t.Error("the item is still stored")
	}
	if r := s.Requests(); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

// What brig login prints on failure can quote a deployment's malformed
// header, through net/http, or the keychain's own words. Neither reaches the
// terminal with a control character in it.
func TestLoginErrorsAreCleaned(t *testing.T) {
	const forged = "\x1b[2K\x1b[1A\x1b[2K\rLogged in to https://my.nofire.ai. Token expires 2027-01-01.\x1b[8m"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Read(make([]byte, 4096))
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n" + forged + "\r\n\r\n"))
			conn.Close()
		}
	}()
	t.Run("malformed header", func(t *testing.T) {
		loginHost(t)
		err := loginCmd(io.Discard, []string{"http://" + ln.Addr().String(), "--browser"})
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v, want the malformed header error", err)
		}
		if strings.ContainsAny(err.Error(), "\x1b\r\n") {
			t.Errorf("error holds terminal controls: %q", err)
		}
	})
	t.Run("keychain error", func(t *testing.T) {
		m := loginHost(t)
		m.saveErr = errors.New("locked" + forged)
		s := fakeDeployment(t, fakeas.Options{})
		err := loginCmd(io.Discard, []string{s.URL, "--device"})
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("err = %v, want the store error", err)
		}
		if strings.ContainsAny(err.Error(), "\x1b\r\n") {
			t.Errorf("error holds terminal controls: %q", err)
		}
	})
}

// Once Ctrl-C has ended the wait, brig login stops catching it, so a second
// Ctrl-C during the revocation that follows ends the process instead of
// waiting out the request timeout.
func TestInterruptStopsCatchingCtrlC(t *testing.T) {
	loginHost(t)
	stopped := make(chan struct{})
	var stoppedFirst atomic.Bool
	o := fakeas.Options{Log: func(line string) {
		if line != "POST /oauth/revoke" {
			return
		}
		select {
		case <-stopped:
			stoppedFirst.Store(true)
		case <-time.After(5 * time.Second):
		}
	}}
	_, base := interruptAfterGrant(t, o)
	inner := loginContext
	loginContext = func() (context.Context, context.CancelFunc) {
		ctx, stop := inner()
		var once sync.Once
		return ctx, func() {
			once.Do(func() { close(stopped) })
			stop()
		}
	}
	t.Cleanup(func() { loginContext = inner })
	err := loginCmd(io.Discard, []string{base, "--device"})
	if err == nil || err.Error() != "login cancelled. Brig revoked the token the deployment issued" {
		t.Fatalf("err = %v, want the cancelled-and-revoked error", err)
	}
	if !stoppedFirst.Load() {
		t.Error("brig login still caught Ctrl-C while it revoked the token")
	}
}

// A trust store from the environment lets whoever set it read the sign-in
// through TLS, so it is named and confirmed as BRIG_NOFIRE_URL is. Answered
// n, nothing is opened or sent.
func TestCATrustStoreAsksOnATerminal(t *testing.T) {
	m := loginHost(t)
	opened := false
	openLoginStore = func() (login.Store, error) { opened = true; return m, nil }
	s := fakeDeployment(t, fakeas.Options{})
	t.Setenv("SSL_CERT_FILE", "./ca\x1b[2K.pem")
	master := terminalStdin(t)
	if _, err := master.Write([]byte("n\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = loginCmd(&out, []string{s.URL, "--device"}) })
	if err == nil || exitCode(err) != exitFailure {
		t.Errorf("err = %v (exit %d), want exit 1", err, exitCode(err))
	}
	wd, _ := os.Getwd()
	if !strings.Contains(out.String(), "Trusting certificates from "+filepath.Join(wd, "ca[2K.pem")+" (from SSL_CERT_FILE)\n") {
		t.Errorf("out = %q, want the trust store named, cleaned", out.String())
	}
	if !strings.Contains(stderr, "Continue logging in to "+s.URL+"? [y/N]") {
		t.Errorf("no question asked: %q", stderr)
	}
	if r := s.Requests(); len(r) != 0 || opened {
		t.Errorf("requests = %v, store opened %v; want neither", r, opened)
	}
}

// Without a terminal a trust store from the environment cannot be
// confirmed, so the login is refused before the keychain or the deployment
// is touched.
func TestCATrustStoreRefusedWithoutATerminal(t *testing.T) {
	for _, v := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		t.Run(v, func(t *testing.T) {
			m := loginHost(t)
			opened := false
			openLoginStore = func() (login.Store, error) { opened = true; return m, nil }
			s := fakeDeployment(t, fakeas.Options{})
			t.Setenv(v, "/tmp/ca")
			err := loginCmd(io.Discard, []string{s.URL, "--device"})
			want := v + " is set, and brig login needs a terminal to confirm it. Unset " + v + ", or run brig login from a terminal"
			if err == nil || err.Error() != want || exitCode(err) != exitFailure {
				t.Errorf("err = %v (exit %d), want %q", err, exitCode(err), want)
			}
			if r := s.Requests(); len(r) != 0 || opened {
				t.Errorf("requests = %v, store opened %v; want neither", r, opened)
			}
		})
	}
}

// The default deployment adds no risk of its own, but a trust store from the
// environment does, so the question is asked for it too, and only once when
// BRIG_NOFIRE_URL also asks for it.
func TestCATrustStoreAsksOnceForAnyDeployment(t *testing.T) {
	for name, envURL := range map[string]string{"default": "", "from the environment": "https://nofire.example.internal"} {
		t.Run(name, func(t *testing.T) {
			loginHost(t)
			t.Setenv(nofireauth.EnvURL, envURL)
			t.Setenv("SSL_CERT_DIR", "/tmp/certs")
			want := nofireauth.DefaultURL
			if envURL != "" {
				want = envURL
			}
			master := terminalStdin(t)
			if _, err := master.Write([]byte("n\n")); err != nil {
				t.Fatal(err)
			}
			var err error
			stderr := captureStderr(t, func() { err = loginCmd(io.Discard, []string{"--device"}) })
			if err == nil {
				t.Error("login succeeded")
			}
			if n := strings.Count(stderr, "Continue logging in to "+want+"? [y/N]"); n != 1 {
				t.Errorf("asked %d times: %q", n, stderr)
			}
		})
	}
}

// proxyChild is set in the environment of the copy of this test binary that
// TestProxyIsNamed starts. net/http reads the proxy variables once a
// process, so each case needs a process of its own.
const proxyChild = "BRIG_TEST_PROXY_CHILD"

// A proxy from the environment is named, without its password, by brig
// login and brig logout. It is a notice only: TLS still runs through it.
func TestProxyIsNamed(t *testing.T) {
	if verb := os.Getenv(proxyChild); verb != "" {
		openLoginStore = func() (login.Store, error) { return nil, login.ErrUnsupported }
		t.Setenv(nofireauth.EnvURL, "")
		if verb == "login" {
			_ = loginCmd(os.Stdout, []string{"https://nofire.example.internal", "--device"})
		} else {
			_ = logoutCmd(os.Stdout, []string{"https://nofire.example.internal"})
		}
		return
	}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"HTTPS_PROXY", []string{"HTTPS_PROXY=http://user:s3cret@proxy.example:3128"},
			"Using the proxy http://proxy.example:3128 (from HTTPS_PROXY)\n"},
		{"https_proxy", []string{"https_proxy=http://proxy.example:8080"},
			"Using the proxy http://proxy.example:8080 (from https_proxy)\n"},
		{"HTTP_PROXY only", []string{"HTTP_PROXY=http://proxy.example:3128"}, ""},
		{"none", nil, ""},
		{"NO_PROXY matches", []string{"HTTPS_PROXY=http://proxy.example:3128", "NO_PROXY=.example.internal"}, ""},
	} {
		for _, verb := range []string{"login", "logout"} {
			t.Run(tc.name+"/"+verb, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestProxyIsNamed$")
				var env []string
				for _, kv := range os.Environ() {
					switch k, _, _ := strings.Cut(kv, "="); strings.ToUpper(k) {
					case "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "REQUEST_METHOD", "SSL_CERT_FILE", "SSL_CERT_DIR":
						continue
					}
					env = append(env, kv)
				}
				cmd.Env = append(append(env, proxyChild+"="+verb), tc.env...)
				got, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, got)
				}
				notice := ""
				for _, line := range strings.SplitAfter(string(got), "\n") {
					if strings.HasPrefix(line, "Using the proxy") {
						notice += line
					}
				}
				if notice != tc.want {
					t.Errorf("notice = %q, want %q", notice, tc.want)
				}
				if strings.Contains(string(got), "s3cret") || strings.Contains(string(got), "user@") {
					t.Errorf("the proxy's user info was printed: %s", got)
				}
			})
		}
	}
}

// Logout sends the stored token, so a trust store from the environment is
// confirmed before it too: answered n, or with no terminal, nothing is sent
// and the token stays stored; answered y, the logout runs as usual.
func TestLogoutConfirmsATrustStore(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		terminal     bool
	}{
		{"answered n", "n\n", true},
		{"no terminal", "", false},
		{"answered y", "y\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loginHost(t)
			s := fakeDeployment(t, fakeas.Options{})
			if err := loginCmd(io.Discard, []string{s.URL, "--device"}); err != nil {
				t.Fatal(err)
			}
			before := len(s.Requests())
			t.Setenv("SSL_CERT_FILE", "/tmp/ca.pem")
			if tc.terminal {
				master := terminalStdin(t)
				if _, err := master.Write([]byte(tc.answer)); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			var err error
			stderr := captureStderr(t, func() { err = logoutCmd(&out, []string{s.URL}) })
			if !strings.Contains(out.String(), "Trusting certificates from /tmp/ca.pem (from SSL_CERT_FILE)\n") {
				t.Errorf("out = %q, want the trust store named", out.String())
			}
			asked := strings.Contains(stderr, "Continue logging out of "+s.URL+"? [y/N]")
			if asked != tc.terminal {
				t.Errorf("asked = %v, want %v: %q", asked, tc.terminal, stderr)
			}
			_, stored := m.items[s.URL]
			sent := s.Requests()[before:]
			switch tc.name {
			case "answered y":
				if err != nil || stored || len(s.Revoked()) != 1 {
					t.Errorf("err = %v, stored %v, revoked %d; want a normal logout", err, stored, len(s.Revoked()))
				}
			case "no terminal":
				want := "SSL_CERT_FILE is set, and brig logout needs a terminal to confirm it. Unset SSL_CERT_FILE, or run brig logout from a terminal"
				if err == nil || err.Error() != want || exitCode(err) != exitFailure {
					t.Errorf("err = %v (exit %d), want %q", err, exitCode(err), want)
				}
				fallthrough
			default:
				if err == nil || exitCode(err) != exitFailure || !stored || len(sent) != 0 {
					t.Errorf("err = %v, stored %v, requests %v; want exit 1, the token kept and nothing sent", err, stored, sent)
				}
			}
		})
	}
}

// pipeStdin points os.Stdin at a file holding data. A file is not a
// terminal, which is the only property the code under test asks about.
func pipeStdin(t *testing.T, data string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// terminalStdin points os.Stdin at a real pseudo-terminal and returns the
// master end, for a test that types an answer.
func terminalStdin(t *testing.T) *os.File {
	t.Helper()
	return ttytest.AsStdin(t)
}

// captureStderr points os.Stderr at a file while fn runs and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = old
	f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
