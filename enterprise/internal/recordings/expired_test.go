// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/internal/cli"
)

const (
	pauseLine  = "brig: your sign-in to %s expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n"
	failedLine = "brig: the sign-in did not finish, so this run is not recorded. Run `brig login` to sign in\n"
)

// oldToken is the token of a sign-in that expired. The fake deployment accepts
// recToken only, so a run that used the old token would be refused.
const oldToken = "nfoa-OLD-expired-token"

// expire stores a login for the deployment h.srv stands for and marks it
// expired, so the person's sign-in has run out. Its token is oldToken.
func (h *recHost) expire(t *testing.T) nofireauth.Deployment {
	t.Helper()
	return h.expireAt(t, h.srv.URL())
}

// expireAt is expire for the deployment at url.
func (h *recHost) expireAt(t *testing.T, url string) nofireauth.Deployment {
	t.Helper()
	h.signInTo(t, url, oldToken)
	d, err := nofireauth.ParseDeployment(url)
	if err != nil {
		t.Fatal(err)
	}
	h.logins.mu.Lock()
	h.logins.expired[d.String()] = true
	h.logins.mu.Unlock()
	return d
}

// renew is a sign-in that succeeds: it stores recToken, the token the fake
// deployment accepts, in place of the expired one, as brig login does.
func (h *recHost) renew(out io.Writer, d nofireauth.Deployment) error {
	h.logins.mu.Lock()
	defer h.logins.mu.Unlock()
	h.logins.items[d.String()] = recToken
	h.logins.expired[d.String()] = false
	return nil
}

// A run that could be recorded, with an expired sign-in, pauses before the
// command starts, signs the person in again for that deployment, and is then
// recorded as a signed-in run is. Nothing happens before the relay is called.
func TestAnExpiredSignInPausesSignsInAndTheRunIsRecorded(t *testing.T) {
	h := newRecHost(t)
	d := h.expire(t)
	seen := pipes(t)

	var signed []string
	signIn = func(out io.Writer, dep nofireauth.Deployment) error {
		signed = append(signed, dep.String())
		// The line is printed before the sign-in, and nothing has been sent to
		// the recording routes yet.
		if want := "brig: your sign-in to " + d.String() + " expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n"; h.notice.String() != want {
			t.Errorf("notice at sign-in %q, want %q", h.notice, want)
		}
		if n := h.srv.Requests(); n != 0 {
			t.Errorf("%d recording requests before the sign-in finished", n)
		}
		return h.renew(out, dep)
	}

	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("an expired sign-in on a run that could be recorded got no relay")
	}
	if len(signed) != 0 || h.notice.Len() != 0 || h.srv.Requests() != 0 {
		t.Fatalf("before the relay ran: %d sign-ins, notice %q, %d requests", len(signed), h.notice, h.srv.Requests())
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Errorf("state dir was created before the relay ran: %v", err)
	}

	code, err := relay([]string{"sh", "-c", "echo after-sign-in; exit 4"}, os.Environ())
	if err != nil || code != 4 {
		t.Fatalf("relay: %d %v", code, err)
	}
	if len(signed) != 1 || signed[0] != d.String() {
		t.Errorf("sign-ins %v, want one for %s", signed, d)
	}
	if want := "brig: your sign-in to " + d.String() + " expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n" + recordedNotice + "\n"; h.notice.String() != want {
		t.Errorf("notice %q, want %q", h.notice, want)
	}
	keys := h.srv.Keys()
	if len(keys) != 1 {
		t.Fatalf("recordings %v, want one", keys)
	}
	if rec := h.srv.Recording(keys[0]); !rec.Closed || rec.End.ExitCode == nil || *rec.End.ExitCode != 4 {
		t.Errorf("close %+v", rec.End)
	}
	if h.srv.BadAuth() != 0 {
		t.Errorf("%d requests with a token the deployment does not accept: the run used the expired token", h.srv.BadAuth())
	}
	if h.srv.Requests() == 0 {
		t.Error("no request reached the deployment")
	}
	if out, _ := seen(); out == "" {
		t.Error("the command's output did not reach the terminal")
	}
}

// A sign-in that fails or is cancelled, or does not leave a usable token prints
// one more line, sends nothing to the recording routes, creates no file, does
// not run the command itself, and declines so that the runtime hands the
// process over unrecorded.
func TestASignInThatDoesNotFinishLeavesTheRunUnrecordedWithOneLine(t *testing.T) {
	cases := map[string]struct {
		signIn func(h *recHost) func(io.Writer, nofireauth.Deployment) error
		after  func(nofireauth.Deployment, time.Duration) (string, string, bool, bool)
		last   string
	}{
		"fails or is cancelled": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return errors.New("login cancelled") }
		}, last: "brig: login cancelled\n" + failedLine},
		"finishes with no token to read": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return nil }
		}, last: failedLine},
		"finishes and the keychain does not answer": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return nil }
		}, after: func(nofireauth.Deployment, time.Duration) (string, string, bool, bool) { return "", "", true, false },
			last: "brig: could not read your login within 3 seconds, so this run is not recorded\n"},
		"finishes and the trust store is not accepted": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return nil }
		}, after: func(nofireauth.Deployment, time.Duration) (string, string, bool, bool) {
			return "", "SSL_CERT_FILE", false, false
		}, last: "brig: could not check whether this run is recorded: SSL_CERT_FILE is set and was not accepted when you signed in. Run `brig login` to accept it\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRecHost(t)
			d := h.expire(t)
			marker := filepath.Join(t.TempDir(), "ran")
			calls := 0
			signIn = func(out io.Writer, dep nofireauth.Deployment) error {
				calls++
				return c.signIn(h)(out, dep)
			}
			if c.after != nil {
				first := true
				base := accessToken
				accessToken = func(dep nofireauth.Deployment, w time.Duration) (string, string, bool, bool) {
					if first {
						first = false
						return base(dep, w)
					}
					return c.after(dep, w)
				}
			}
			relay := Attach(hullRun())
			if relay == nil {
				t.Fatal("no relay")
			}
			code, err := relay([]string{"sh", "-c", "touch " + marker}, os.Environ())
			if !errors.Is(err, cli.ErrRelayDeclined) || code != 0 {
				t.Errorf("relay: %d %v, want it to decline", code, err)
			}
			if want := "brig: your sign-in to " + d.String() + " expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n" + c.last; h.notice.String() != want {
				t.Errorf("notice %q, want %q", h.notice, want)
			}
			if calls != 1 {
				t.Errorf("%d sign-ins, want 1", calls)
			}
			if h.srv.Requests() != 0 {
				t.Errorf("%d requests to the recording routes", h.srv.Requests())
			}
			if _, err := os.Stat(h.state); !os.IsNotExist(err) {
				t.Errorf("state dir was created: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("the relay ran the command; the runtime does that for a declined run")
			}
		})
	}
}

// A sign-in that ends with an error shows that error to the person before the
// line that says it did not finish. One of them says a token the deployment
// issued is still valid, and the person has to revoke it. The text is cleaned
// as brig login cleans it, so it puts no control character on the terminal.
func TestASignInErrorThatSaysToRevokeATokenReachesThePerson(t *testing.T) {
	h := newRecHost(t)
	d := h.expire(t)
	revoke := "cannot store the token: the keychain is locked. Brig also could not revoke it, so it stays valid until it expires: the deployment is down. Revoke it from the dashboard"
	signIn = func(io.Writer, nofireauth.Deployment) error { return errors.New(revoke + "\x1b[31m") }
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("no relay")
	}
	if _, err := relay([]string{"true"}, os.Environ()); !errors.Is(err, cli.ErrRelayDeclined) {
		t.Fatalf("relay: %v, want it to decline", err)
	}
	want := fmt.Sprintf(pauseLine, d) + "brig: " + nofireauth.CleanMessage(revoke+"\x1b[31m") + "\n" + failedLine
	if h.notice.String() != want {
		t.Errorf("notice %q, want %q", h.notice, want)
	}
	if !strings.Contains(h.notice.String(), "Revoke it from the dashboard") || strings.ContainsRune(h.notice.String(), 0x1b) {
		t.Errorf("notice %q: want the revoke text and no escape", h.notice)
	}
}

// After a sign-in that finished, the deployment decides as it does for any
// signed-in run: a "not recorded" answer, or a deployment that cannot be
// reached, hands the run over with no line beyond the pause line. The sign-in
// ran once, and its token was the one sent.
func TestAfterASignInTheDeploymentCanStillLeaveTheRunUnrecorded(t *testing.T) {
	cases := map[string]func(h *recHost) nofireauth.Deployment{
		"the deployment says not recorded": func(h *recHost) nofireauth.Deployment {
			h.srv.SetRecorded(false)
			return h.expire(t)
		},
		"the deployment cannot be reached": func(h *recHost) nofireauth.Deployment {
			t.Setenv(nofireauth.EnvURL, "http://127.0.0.1:1")
			return h.expireAt(t, "http://127.0.0.1:1")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRecHost(t)
			d := setup(h)
			calls := 0
			signIn = func(out io.Writer, dep nofireauth.Deployment) error { calls++; return h.renew(out, dep) }
			relay := Attach(hullRun())
			if relay == nil {
				t.Fatal("no relay")
			}
			marker := filepath.Join(t.TempDir(), "ran")
			code, err := relay([]string{"sh", "-c", "touch " + marker}, os.Environ())
			if !errors.Is(err, cli.ErrRelayDeclined) || code != 0 {
				t.Errorf("relay: %d %v, want it to decline", code, err)
			}
			if want := fmt.Sprintf(pauseLine, d); h.notice.String() != want {
				t.Errorf("notice %q, want only %q", h.notice, want)
			}
			if calls != 1 {
				t.Errorf("%d sign-ins, want 1", calls)
			}
			if _, err := os.Stat(h.recordingsDir()); !os.IsNotExist(err) {
				t.Errorf("a recording was started: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("the relay ran the command; the runtime does that for a declined run")
			}
		})
	}
}

// A run that cannot be recorded does not pause and starts no sign-in, whatever
// state the stored login is in: no line, no keychain write, no request.
func TestAnExpiredSignInOnARunThatCannotBeRecordedStartsNothing(t *testing.T) {
	notHull := hullRun()
	notHull.Runtime = "other"
	cases := map[string]struct {
		run   cli.AttachedRun
		setup func()
	}{
		"a runtime other than hull": {run: notHull},
		"stdout is not a terminal":  {run: hullRun(), setup: func() { isTerminal = func(f *os.File) bool { return f != os.Stdout } }},
		"stdin is not a terminal":   {run: hullRun(), setup: func() { isTerminal = func(f *os.File) bool { return f != os.Stdin } }},
		"two terminals":             {run: hullRun(), setup: func() { sameTerminal = func(a, b *os.File) bool { return false } }},
		"no deployment resolves": {run: hullRun(), setup: func() {
			t.Setenv(nofireauth.EnvURL, "http://[bad")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRecHost(t)
			h.expire(t)
			if c.setup != nil {
				c.setup()
			}
			if relay := Attach(c.run); relay != nil {
				t.Fatal("got a relay, which would pause the run")
			}
			if h.notice.Len() != 0 || h.srv.Requests() != 0 {
				t.Errorf("notice %q, %d requests", h.notice, h.srv.Requests())
			}
			if _, err := os.Stat(h.state); !os.IsNotExist(err) {
				t.Errorf("state dir was created: %v", err)
			}
		})
	}
}

// When stderr is not a terminal, the pause would be invisible: the person
// would see nothing while the sign-in waited for them. Every line of the pause
// goes to the terminal the run uses, which is stdout, and none to stderr. When
// stderr is a terminal, the lines stay on it.
func TestThePauseGoesToTheTerminalWhenStderrIsNotOne(t *testing.T) {
	for _, c := range []struct {
		name           string
		stderrTerminal bool
		stderrIsRuns   bool
	}{
		{"stderr is a file", false, false},
		{"stderr is the terminal the run uses", true, true},
		{"stderr is another terminal", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRecHost(t)
			d := h.expire(t)
			seen := pipes(t)
			notice = os.Stderr
			isTerminal = func(f *os.File) bool { return f != os.Stderr || c.stderrTerminal }
			sameTerminal = func(a, b *os.File) bool { return b != os.Stderr || c.stderrIsRuns }
			signIn = func(out io.Writer, _ nofireauth.Deployment) error {
				_, _ = io.WriteString(out, "SIGN-IN OUTPUT\n")
				return errors.New("the deployment is down")
			}
			relay := Attach(hullRun())
			if relay == nil {
				t.Fatal("no relay")
			}
			if _, err := relay([]string{"true"}, os.Environ()); !errors.Is(err, cli.ErrRelayDeclined) {
				t.Fatalf("relay: %v, want it to decline", err)
			}
			stdout, stderr := seen()
			lines := fmt.Sprintf(pauseLine, d) + "SIGN-IN OUTPUT\nbrig: the deployment is down\n" + failedLine
			want, wantNot := stdout, stderr
			if c.stderrIsRuns {
				want, wantNot = stderr, stdout
			}
			if want != lines || wantNot != "" {
				t.Errorf("stdout %q, stderr %q, want the lines %q on one of them and nothing on the other", stdout, stderr, lines)
			}
			if h.notice.Len() != 0 {
				t.Errorf("the test notice buffer got %q", h.notice)
			}
		})
	}
}
