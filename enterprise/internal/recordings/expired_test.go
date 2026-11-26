// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/internal/cli"
)

const (
	pauseLine  = "brig: your sign-in to %s expired. Signing you in again so this run can be recorded. Ctrl-C runs it without recording\n"
	failedLine = "brig: the sign-in did not finish, so this run is not recorded. Run `brig login` to sign in\n"
)

// expire stores a login for the deployment h.srv stands for and marks it
// expired, so the person's sign-in has run out.
func (h *recHost) expire(t *testing.T) nofireauth.Deployment {
	t.Helper()
	h.signIn(t)
	d, err := nofireauth.ParseDeployment(h.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	h.logins.mu.Lock()
	h.logins.expired[d.String()] = true
	h.logins.mu.Unlock()
	return d
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
		h.logins.mu.Lock()
		h.logins.expired[dep.String()] = false
		h.logins.mu.Unlock()
		return nil
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
		t.Errorf("%d requests without the bearer token", h.srv.BadAuth())
	}
	if out, _ := seen(); out == "" {
		t.Error("the command's output did not reach the terminal")
	}
}

// A sign-in that fails, is cancelled, or does not leave a usable token prints
// one more line, sends nothing to the recording routes, creates no file, does
// not run the command itself, and declines so that the runtime hands the
// process over unrecorded.
func TestASignInThatDoesNotFinishLeavesTheRunUnrecordedWithOneLine(t *testing.T) {
	cases := map[string]struct {
		signIn func(h *recHost) func(io.Writer, nofireauth.Deployment) error
		after  func(nofireauth.Deployment, time.Duration) (string, string, bool, bool)
		last   string
	}{
		"fails": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return errors.New("the deployment is down") }
		}, last: failedLine},
		"cancelled": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return errors.New("login cancelled") }
		}, last: failedLine},
		"finishes with no token to read": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return nil }
		}, last: failedLine},
		"finishes and the keychain does not answer": {signIn: func(*recHost) func(io.Writer, nofireauth.Deployment) error {
			return func(io.Writer, nofireauth.Deployment) error { return nil }
		}, after: func(nofireauth.Deployment, time.Duration) (string, string, bool, bool) { return "", "", true, false },
			last: "brig: could not read your login within 3 seconds, so this run is not recorded\n"},
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
	}{{"stderr is a file", false}, {"stderr is a terminal", true}} {
		t.Run(c.name, func(t *testing.T) {
			h := newRecHost(t)
			d := h.expire(t)
			seen := pipes(t)
			notice = os.Stderr
			isTerminal = func(f *os.File) bool { return f != os.Stderr || c.stderrTerminal }
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
			lines := fmt.Sprintf(pauseLine, d) + "SIGN-IN OUTPUT\n" + failedLine
			want, wantNot := stdout, stderr
			if c.stderrTerminal {
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
