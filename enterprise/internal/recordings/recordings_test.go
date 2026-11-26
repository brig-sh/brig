// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recordings

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/login"
	"github.com/brig-sh/brig/enterprise/internal/nofireauth"
	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
	"github.com/brig-sh/brig/internal/cli"
)

// recToken is the sentinel the leak tests look for. It is long and odd so that
// no other text in a run contains it by chance.
const recToken = "nfoa-SENTINEL-4f19c0de-must-not-leak"

// logins stands in for the login store: login.AccessToken has its own tests,
// and these tests are about what the recordings code does with the answer. It
// is keyed by the deployment's normalised URL, as the store is, and remembers
// every deployment it was asked about.
type logins struct {
	mu    sync.Mutex
	items map[string]string
	// trust is what the person accepted at login for each deployment.
	trust map[string]map[string]string
	asked []string
}

// token answers as login.AccessToken does for a stored, unexpired login, with
// the rule that decides on the trust store taken from the login package.
func (l *logins) token(d nofireauth.Deployment, _ time.Duration) (string, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, d.String())
	tok := l.items[d.String()]
	if tok == "" {
		return "", "", false
	}
	if v := login.UnacceptedTrust(l.trust[d.String()], os.Getenv); v != "" {
		return "", v, false
	}
	return tok, "", false
}

// recHost wires the gate to a fake deployment, an in-memory login store and a
// temporary state directory.
type recHost struct {
	srv    *recordingtest.Server
	logins *logins
	state  string
	notice *bytes.Buffer
}

func newRecHost(t *testing.T) *recHost {
	t.Helper()
	h := &recHost{srv: recordingtest.New(t, recToken), logins: &logins{items: map[string]string{}, trust: map[string]map[string]string{}},
		state: filepath.Join(t.TempDir(), "state"), notice: &bytes.Buffer{}}
	t.Setenv(nofireauth.EnvURL, h.srv.URL())
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	oldTok, oldH, oldS, oldN, oldT, oldSame := accessToken, httpClient, stateDir, notice, isTerminal, sameTerminal
	isTerminal = func(*os.File) bool { return true }
	sameTerminal = func(a, b *os.File) bool { return true }
	accessToken = h.logins.token
	httpClient = h.srv.Client()
	stateDir = func() string { return h.state }
	notice = h.notice
	t.Cleanup(func() {
		accessToken, httpClient, stateDir, notice, isTerminal, sameTerminal = oldTok, oldH, oldS, oldN, oldT, oldSame
	})
	return h
}

// signIn stores recToken for the deployment h.srv stands for.
func (h *recHost) signIn(t *testing.T) { t.Helper(); h.signInTo(t, h.srv.URL(), recToken) }

func (h *recHost) signInTo(t *testing.T, url, token string) {
	t.Helper()
	d, err := nofireauth.ParseDeployment(url)
	if err != nil {
		t.Fatal(err)
	}
	h.logins.mu.Lock()
	h.logins.items[d.String()] = token
	h.logins.mu.Unlock()
}

// accept stores what the person accepted at login for the deployment.
func (h *recHost) accept(t *testing.T, url string, trust map[string]string) {
	t.Helper()
	d, err := nofireauth.ParseDeployment(url)
	if err != nil {
		t.Fatal(err)
	}
	h.logins.mu.Lock()
	h.logins.trust[d.String()] = trust
	h.logins.mu.Unlock()
}

func (h *recHost) recordingsDir() string { return filepath.Join(h.state, "recordings") }

func hullRun() cli.AttachedRun {
	return cli.AttachedRun{Sandbox: "brig-ubuntu-x", Ref: "ubuntu@x", Profile: "ubuntu", Runtime: "hull", Version: "test", Cols: 100, Rows: 30}
}

func TestGateWithoutALoginTouchesNothing(t *testing.T) {
	cases := map[string]func(h *recHost){
		"no token": func(h *recHost) {},
		"a token the store reads as unusable": func(h *recHost) {
			accessToken = func(nofireauth.Deployment, time.Duration) (string, string, bool) { return "", "", false }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRecHost(t)
			setup(h)
			if relay := Attach(hullRun()); relay != nil {
				t.Fatal("recorded")
			}
			if n := h.srv.Requests(); n != 0 {
				t.Errorf("%d HTTP requests with no usable login", n)
			}
			if _, err := os.Stat(h.state); !os.IsNotExist(err) {
				t.Errorf("state dir was created: %v", err)
			}
			if h.notice.Len() != 0 {
				t.Errorf("notice %q", h.notice)
			}
		})
	}
}

func TestGateAsksOnlyForHull(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	run := hullRun()
	run.Runtime = "nerdctl"
	if Attach(run) != nil || h.srv.Requests() != 0 || len(h.logins.asked) != 0 {
		t.Fatalf("nerdctl: requests %d, store asked %v", h.srv.Requests(), h.logins.asked)
	}
}

// declines runs the relay Attach returned and reports whether it declined the
// run, which is how a run goes on as it always has.
func declines(t *testing.T, relay func(argv, env []string) (int, error)) bool {
	t.Helper()
	if relay == nil {
		t.Fatal("Attach returned no relay for a signed-in run on hull")
	}
	code, err := relay([]string{"sh", "-c", "echo must-not-run"}, os.Environ())
	if err != nil && !errors.Is(err, cli.ErrRelayDeclined) {
		t.Fatalf("relay: %d %v", code, err)
	}
	return errors.Is(err, cli.ErrRelayDeclined)
}

func TestGateRunsAsTodayWhenTheAnswerIsNoOrFails(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	h.srv.SetRecorded(false)
	if !declines(t, Attach(hullRun())) || h.notice.Len() != 0 {
		t.Fatal("recorded after a no")
	}
	if _, err := os.Stat(h.recordingsDir()); !os.IsNotExist(err) {
		t.Errorf("recordings dir exists after a no")
	}
	if h.srv.Requests() != 1 {
		t.Errorf("requests %d", h.srv.Requests())
	}

	// A wrong token is a 401, not a recording.
	h.srv.SetRecorded(true)
	h.signInTo(t, h.srv.URL(), "wrong")
	if !declines(t, Attach(hullRun())) || h.notice.Len() != 0 {
		t.Fatal("recorded with a rejected token")
	}

	// An unreachable deployment.
	h.srv.SetRecorded(true)
	t.Setenv(nofireauth.EnvURL, "http://127.0.0.1:1")
	h.signInTo(t, "http://127.0.0.1:1", recToken)
	if !declines(t, Attach(hullRun())) || h.notice.Len() != 0 {
		t.Fatal("recorded with no deployment")
	}
}

// Attach decides from what is on this machine. The deployment is asked, a file
// is made and the notice is printed only when the runtime calls the relay, which
// it does after the exec's command is built.
func TestAttachAsksTheDeploymentOnlyWhenTheRelayRuns(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	if n := h.srv.Requests(); n != 0 {
		t.Errorf("%d requests before the relay ran", n)
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		t.Errorf("state dir was created before the relay ran: %v", err)
	}
	if h.notice.Len() != 0 {
		t.Errorf("notice %q before the relay ran", h.notice)
	}
}

// pipes swaps os.Stdin, os.Stdout and os.Stderr for pipes, so the relay has no
// terminal, and returns a function that closes the write ends, waits for the
// copiers and returns what the "terminal" and stderr saw. The buffers are only
// read after the copiers have finished, so the test and the copiers never
// touch them together.
func pipes(t *testing.T) func() (stdout, stderr string) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	var seenOut, seenErr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { _, _ = io.Copy(&seenOut, outR); wg.Done() }()
	go func() { _, _ = io.Copy(&seenErr, errR); wg.Done() }()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
			inW.Close()
			outW.Close()
			errW.Close()
			wg.Wait()
		})
	}
	t.Cleanup(finish)
	return func() (string, string) { finish(); return seenOut.String(), seenErr.String() }
}

func TestRecordedRunEndToEnd(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	seen := pipes(t)

	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	code, err := relay([]string{"sh", "-c", `printf '\033[1mbold\033[0m\n'; echo line2; exit 5`}, os.Environ())
	if err != nil || code != 5 {
		t.Fatalf("relay: %d %v", code, err)
	}
	if got := h.notice.String(); got != "brig: this run is recorded for your organization\n" {
		t.Errorf("notice %q", got)
	}
	if strings.Contains(h.notice.String(), recToken) {
		t.Error("the token was printed")
	}
	keys := h.srv.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys %v", keys)
	}
	rec := h.srv.Recording(keys[0])
	fs, err := frame.Decode(rec.Raw)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, f := range fs {
		if f.Type == frame.Output {
			out = append(out, f.Payload...)
		}
	}
	want := "\x1b[1mbold\x1b[0m\r\nline2\r\n"
	if string(out) != want {
		t.Errorf("recorded %q, want %q", out, want)
	}
	last := fs[len(fs)-1]
	if last.Type != frame.Exit || last.Payload[0] != 5 {
		t.Errorf("last frame %c %v", last.Type, last.Payload)
	}
	if !rec.Closed || rec.End.EndReason != "exit" || rec.End.ExitCode == nil || *rec.End.ExitCode != 5 {
		t.Errorf("close %+v", rec.End)
	}
	for k, v := range map[string]string{"sandbox": "brig-ubuntu-x", "ref": "ubuntu@x", "profile": "ubuntu", "runtime": "hull", "brig_version": "test"} {
		if rec.Open[k] != v {
			t.Errorf("open %s = %v, want %s", k, rec.Open[k], v)
		}
	}
	if h.srv.BadAuth() != 0 {
		t.Errorf("%d requests without the bearer token", h.srv.BadAuth())
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
	if got, _ := seen(); !strings.Contains(got, "line2") {
		t.Errorf("the user's terminal saw %q", got)
	}
}

func TestRecordingsFlushSendsAWaitingSpool(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	// The network is down for the whole drain; the spool stays.
	for i := 0; i < 500; i++ {
		h.srv.FailNextPuts(503)
	}
	h.srv.FailNextPuts()
	start := time.Now()
	if _, err := relay([]string{"sh", "-c", "echo waiting"}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 9*time.Second {
		t.Errorf("drain held the run for %v", el)
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 2 {
		t.Fatalf("spool and meta should stay: %v", left)
	}
	h.srv.ClearFailures()
	var out bytes.Buffer
	if err := recordingsCmd(&out, []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 sent and closed") {
		t.Errorf("flush said %q", out.String())
	}
	rec := h.srv.Recording(h.srv.Keys()[0])
	if !rec.Closed || !bytes.Contains(rec.Raw, []byte("waiting")) {
		t.Errorf("recording %+v", rec.End)
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if strings.Contains(out.String(), recToken) {
		t.Error("token printed")
	}
}

func TestRecordingsFlushWithNoLoginLeavesTheSpool(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	h.srv.FailNextPuts(503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503)
	_, _ = relay([]string{"sh", "-c", "echo x"}, os.Environ())
	h.srv.ClearFailures()
	h.logins.mu.Lock()
	h.logins.items = map[string]string{} // signed out meanwhile
	h.logins.mu.Unlock()
	before := h.srv.Requests()
	var out bytes.Buffer
	if err := recordingsCmd(&out, []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if h.srv.Requests() != before {
		t.Error("flush made a request without a login")
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 2 {
		t.Errorf("left %v", left)
	}
}

// A spool the deployment does not know for the stored login is not sent and not
// opened again, and the command says so in its own count.
func TestRecordingsFlushSaysWhenARecordingWaitsForTheLoginThatRecordedIt(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	h.srv.FailNextPuts(503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503)
	_, _ = relay([]string{"sh", "-c", "echo x"}, os.Environ())
	h.srv.ClearFailures()
	keys := len(h.srv.Keys())
	before := h.srv.Requests()
	h.srv.FailNextPuts(404) // the answer a deployment gives for another login's recording
	var out bytes.Buffer
	if err := recordingsCmd(&out, []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if want := "recordings: 0 sent and closed, 0 waiting for a login or a live run, 0 not sent yet, 1 waiting for the login that recorded them\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	if got := h.srv.Requests() - before; got != 1 || len(h.srv.Keys()) != keys {
		t.Errorf("%d requests and %d new recordings; want the one ownership check and no open", got, len(h.srv.Keys())-keys)
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 2 {
		t.Errorf("left %v", left)
	}
}

func TestRecordingsFlushSaysToLogInAgainOnAuthRefusal(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	h.srv.FailNextPuts(503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503)
	_, _ = relay([]string{"sh", "-c", "echo x"}, os.Environ())
	h.srv.ClearFailures()
	h.srv.FailNextPuts(401)
	var out bytes.Buffer
	err := recordingsCmd(&out, []string{"flush"})
	if err == nil || !strings.Contains(err.Error(), "brig login") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error()+out.String(), recToken) || strings.Contains(err.Error(), h.srv.URL()) {
		t.Error("the message names the token or the URL")
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 2 {
		t.Errorf("left %v", left)
	}
}

// The command's own surface: its usage, its word, a word it does not have, and
// that telemetry does not count it.
func TestRecordingsCommandSurface(t *testing.T) {
	cmds := Commands()
	if len(cmds) != 1 || cmds[0].Verb != "recordings" || !cmds[0].Uncounted {
		t.Fatalf("Commands() = %+v", cmds)
	}
	c := cmds[0]
	if !strings.HasSuffix(c.Synopsis, "\n") || !strings.Contains(c.Synopsis, "brig recordings flush") {
		t.Errorf("synopsis %q", c.Synopsis)
	}
	if got := c.Complete(nil, ""); len(got) != 1 || got[0] != "flush" {
		t.Errorf("completion offers %v, want flush", got)
	}
	if got := c.Complete([]string{"flush"}, ""); len(got) != 0 {
		t.Errorf("completion after flush offers %v", got)
	}
	var out bytes.Buffer
	if err := c.Run(&out, []string{"--help"}); err != nil || out.String() != c.Usage {
		t.Errorf("--help: %v, printed %q", err, out.String())
	}
	if err := c.Run(&out, []string{"nope"}); err == nil || cli.ExitCode(err) == 0 {
		t.Errorf("an unknown subcommand: %v", err)
	}
	if err := c.Run(&out, []string{"flush", "extra"}); err == nil {
		t.Error("flush took an argument")
	}
}

// Only a terminal session is recorded. A run whose stdin or stdout is a pipe or
// a file gets no relay, so it is handed over as an unrecorded run is: the
// deployment is asked nothing, no token is read, nothing is printed and no file
// is made.
func TestARunThatIsNotATerminalSessionIsNotRecordedAndTouchesNothing(t *testing.T) {
	in, out := os.Stdin, os.Stdout
	for name, tty := range map[string]func(f *os.File) bool{
		"stdin a pipe":   func(f *os.File) bool { return f != in },
		"stdout a pipe":  func(f *os.File) bool { return f != out },
		"both pipes":     func(f *os.File) bool { return false },
		"both terminals": func(f *os.File) bool { return true },
		"two terminals":  func(f *os.File) bool { return true }, // see below
	} {
		t.Run(name, func(t *testing.T) {
			h := newRecHost(t)
			h.signIn(t)
			isTerminal = tty
			if name == "two terminals" {
				isTerminal = func(*os.File) bool { return true }
				sameTerminal = func(a, b *os.File) bool { return false }
			}
			relay := Attach(hullRun())
			if name == "both terminals" {
				if relay == nil {
					t.Fatal("a terminal session was not recorded")
				}
				return
			}
			if relay != nil {
				t.Fatal("a run that is not a terminal session got a relay")
			}
			if len(h.logins.asked) != 0 || h.srv.Requests() != 0 || h.notice.Len() != 0 {
				t.Errorf("store asked %v, %d requests, notice %q", h.logins.asked, h.srv.Requests(), h.notice)
			}
			if _, err := os.Stat(h.state); !os.IsNotExist(err) {
				t.Errorf("state dir was created: %v", err)
			}
		})
	}
}

// tokenForSpool tells a URL that can never have a token from one that has none
// now.
func TestTokenForSpoolSaysWhetherTheURLCouldEverHaveAToken(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	if tok, usable := tokenForSpool("http://[bad"); tok != "" || usable {
		t.Errorf("a URL that does not parse: %q, %v", tok, usable)
	}
	if tok, usable := tokenForSpool("https://never-signed-in.example"); tok != "" || !usable {
		t.Errorf("a deployment with no login: %q, %v", tok, usable)
	}
	if tok, usable := tokenForSpool(h.srv.URL()); tok != recToken || !usable {
		t.Errorf("a signed-in deployment: %q, %v", tok, usable)
	}
}

// A keychain that does not answer within the bound makes the run go on
// unrecorded, with one line that says why, printed when the exec is about to
// run. The deployment is asked nothing. A run that is not a terminal session
// gets no line.
func TestAKeychainThatDoesNotAnswerLeavesTheRunUnrecordedWithOneLine(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	accessToken = func(nofireauth.Deployment, time.Duration) (string, string, bool) { return "", "", true }
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("no relay, so no line")
	}
	if h.notice.Len() != 0 {
		t.Errorf("printed before the exec: %q", h.notice)
	}
	if _, err := relay([]string{"sh", "-c", "echo must-not-run"}, os.Environ()); !errors.Is(err, cli.ErrRelayDeclined) {
		t.Errorf("relay err = %v, want it to decline", err)
	}
	if want := "brig: could not read your login within 3 seconds, so this run is not recorded\n"; h.notice.String() != want {
		t.Errorf("line %q, want %q", h.notice, want)
	}
	if h.srv.Requests() != 0 {
		t.Errorf("%d requests", h.srv.Requests())
	}

	h.notice.Reset()
	isTerminal = func(*os.File) bool { return false }
	if Attach(hullRun()) != nil || h.notice.Len() != 0 {
		t.Errorf("a run that is not a terminal session: notice %q", h.notice)
	}
}

// The spool goes under brig's state directory as the rest of brig resolves it:
// BRIG_STATE_DIR when set. Nothing is written under ~/.brig then, so a project
// that is the home directory cannot hold the spool, which the guest could write.
func TestTheSpoolGoesUnderBRIGSTATEDIRAndNothingUnderTheHome(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	home, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("BRIG_STATE_DIR", state)
	stateDir = defaultStateDir
	if got := defaultStateDir(); got != state {
		t.Fatalf("state directory %q, want %q", got, state)
	}
	pipes(t)
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	for i := 0; i < 500; i++ {
		h.srv.FailNextPuts(503) // leave the spool on disk
	}
	if _, err := relay([]string{"sh", "-c", "echo x"}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(state, "recordings", "*")); len(files) != 2 {
		t.Errorf("under the state directory: %v", files)
	}
	if _, err := os.Stat(filepath.Join(home, ".brig")); !os.IsNotExist(err) {
		t.Errorf("something was made under the home: %v", err)
	}
	h.srv.ClearFailures()
	var out bytes.Buffer
	if err := recordingsCmd(&out, []string{"flush"}); err != nil || !strings.Contains(out.String(), "1 sent and closed") {
		t.Errorf("flush: %v %q", err, out.String())
	}
}

// A relay that cannot start the command, after the deployment said yes, makes
// the run decline: the recording opened for it is closed at the deployment, no
// spool is left, one line corrects the notice already printed, and the runtime
// then runs the command as an unrecorded run.
func TestARelayThatCannotStartTheCommandDeclinesAndLeavesNothing(t *testing.T) {
	h := newRecHost(t)
	h.signIn(t)
	pipes(t)
	relay := Attach(hullRun())
	if relay == nil {
		t.Fatal("not recorded")
	}
	code, err := relay([]string{"/nonexistent/brig-recorder-test"}, os.Environ())
	if !errors.Is(err, cli.ErrRelayDeclined) || code != 0 {
		t.Fatalf("relay = %d, %v; want it to decline", code, err)
	}
	want := recordedNotice + "\nbrig: this run is not recorded after all: Brig could not set up the recorder\n"
	if h.notice.String() != want {
		t.Errorf("lines %q, want %q", h.notice, want)
	}
	keys := h.srv.Keys()
	if len(keys) != 1 {
		t.Fatalf("recordings %v", keys)
	}
	if rec := h.srv.Recording(keys[0]); !rec.Closed || rec.End.EndReason != "recorder_error" {
		t.Errorf("the recording was not closed honestly: %+v", rec.End)
	}
	if left, _ := filepath.Glob(filepath.Join(h.recordingsDir(), "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
}
