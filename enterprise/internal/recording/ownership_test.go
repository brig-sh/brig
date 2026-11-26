// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/ptyrelay"
	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
)

// gate is a transport that holds back every request once it is closed, so a
// test can freeze a run in the middle of its final drain.
type gate struct {
	next    http.RoundTripper
	mu      sync.Mutex
	closed  bool
	waiting chan struct{}
	open    chan struct{}
}

func newGate(next http.RoundTripper) *gate {
	return &gate{next: next, waiting: make(chan struct{}, 16), open: make(chan struct{})}
}

func (g *gate) close() { g.mu.Lock(); g.closed = true; g.mu.Unlock() }

func (g *gate) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	held := g.closed
	g.mu.Unlock()
	if held {
		g.waiting <- struct{}{}
		select {
		case <-g.open:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return g.next.RoundTrip(r)
}

func flushWith(t *testing.T, dir string, tok string, f *recordingtest.Server) (FlushResult, error) {
	t.Helper()
	return Flush(context.Background(), dir, func(string) (string, bool) { return tok, true },
		func(url, token string) *Client {
			return &Client{Base: f.URL(), Token: token, HTTP: f.Client(), UserAgent: "brig-test"}
		})
}

// While a run drains its spool at the end, another brig process that flushes
// the same directory leaves the spool alone: the run keeps its lock until it
// has removed the spool, so nothing is sent twice and no second recording opens.
func TestAnotherProcessDoesNotFlushASpoolThatIsStillDraining(t *testing.T) {
	r := newRun(t)
	g := newGate(r.f.Client().Transport)
	c := &Client{Base: r.f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: g}, UserAgent: "brig-test"}
	r.s.p.Client, r.s.up.C = c, c
	r.s.p.DrainTimeout = 10 * time.Second
	r.say("one\r\n")
	g.close()

	r.s.rec.Finish(r.clk.Now(), 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		zero := 0
		r.s.finish("exit", &zero, "")
	}()
	select {
	case <-g.waiting: // the final drain is on the wire
	case <-time.After(10 * time.Second):
		t.Fatal("the run never started its final drain")
	}

	before := r.f.Requests()
	res, err := flushWith(t, r.dir, "tok-secret", r.f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Closed != 0 || r.f.Requests() != before {
		t.Errorf("a flush during the drain: %+v, %d requests sent", res, r.f.Requests()-before)
	}

	close(g.open)
	<-done
	if len(r.f.Keys()) != 1 {
		t.Errorf("recordings %v, want the run's own alone", r.f.Keys())
	}
	if rec := r.f.Recording(r.f.Keys()[0]); !rec.Closed || rec.End.EndReason != "exit" {
		t.Errorf("close %+v", rec.End)
	}
	if files := r.dirFiles(); len(files) != 0 {
		t.Errorf("files left %v", files)
	}
}

// A flush that opened a spool just before its run finished and removed it must
// not send from the file it holds: the path no longer names it.
func TestAFlushThatLocksAFileTheRunJustRemovedSendsNothing(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	old := beforeSpoolLock
	t.Cleanup(func() { beforeSpoolLock = old })
	beforeSpoolLock = func() {
		_ = os.Remove(r.s.paths[0]) // the run finished: its spool is gone, its lock released
		r.s.rec.Abort()
		r.s.rec.Close()
	}
	before := r.f.Requests()
	res, err := flushWith(t, r.dir, "tok-secret", r.f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || r.f.Requests() != before {
		t.Errorf("flush %+v, %d requests", res, r.f.Requests()-before)
	}
}

// A leftover spool is sent with whatever login is stored now. When that login
// is not the one that recorded it, the deployment answers 404 to the ownership
// check, and the flush sends nothing, opens nothing and leaves the spool.
func TestFlushSendsNothingForARecordingThatIsNotThisLogins(t *testing.T) {
	r, _ := leftoverRun(t)
	other := recordingtest.New(t, "tok-other")
	before := r.f.Requests()

	res, err := flushWith(t, r.dir, "tok-other", other)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotOurs != 1 || res.Closed != 0 || res.Pending != 0 || res.Skipped != 0 {
		t.Errorf("flush %+v", res)
	}
	if other.Requests() != 1 || len(other.Keys()) != 0 {
		t.Errorf("the other login's deployment saw %d requests and %d opened recordings; want one check and no open", other.Requests(), len(other.Keys()))
	}
	if puts := other.Puts(); len(puts) != 1 || puts[0].Status != 404 {
		t.Errorf("puts %+v", puts)
	}
	if r.f.Requests() != before {
		t.Error("the first deployment was reached")
	}
	if files := r.dirFiles(); len(files) != 2 {
		t.Errorf("the spool was not left in place: %v", files)
	}

	// The login that recorded it still sends it.
	if res, err := flushWith(t, r.dir, "tok-secret", r.f); err != nil || res.Closed != 1 {
		t.Errorf("the right login: %+v %v", res, err)
	}
}

// The check is the first request: for a recording this login owns and the
// deployment closed as stale, the answer 409 recording_closed is what allows
// the continuation in a new recording.
func TestFlushContinuesOnlyAfterTheDeploymentSaysTheRecordingIsThisLoginsAndClosed(t *testing.T) {
	r, spool := leftoverRun(t)
	r.clk.Advance(3 * time.Minute) // the deployment closes it as stale
	puts := len(r.f.Puts())
	if res := r.flush(); res.Closed != 1 {
		t.Fatalf("flush %+v", res)
	}
	first := r.f.Puts()[puts]
	if first.ID != r.f.Recording(r.f.Keys()[0]).ID || first.Status != 409 {
		t.Errorf("the first request was %+v, want the ownership check answered 409 recording_closed", first)
	}
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	if second := r.f.Recording(keys[1]); !second.Closed || len(second.Raw) == 0 || len(second.Raw) > len(spool)+64 {
		t.Errorf("continuation %+v", second.End)
	}
}

// A spool goes once it is older than 30 days only for a reason that does not
// pass: the deployment says the recording is not this login's, the meta file
// cannot be used, or there is no meta file. A spool that only lacks a usable
// login now stays however old, and a later sign-in sends it. A younger one stays.
func TestAFlushRemovesSpoolsThatCannotBeSentAfterThirtyDays(t *testing.T) {
	age := func(r *run, d time.Duration) {
		old := r.clk.Now().Add(-d)
		for _, p := range r.dirFiles() {
			_ = os.Chtimes(p, old, old)
		}
	}
	run := func(r *run, tok string, f *recordingtest.Server) FlushResult {
		old := flushClock
		flushClock = r.clk.Now
		defer func() { flushClock = old }()
		res, err := flushWith(t, r.dir, tok, f)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	r, _ := leftoverRun(t)
	age(r, 29*24*time.Hour)
	if res := run(r, "", r.f); res.Skipped != 1 || res.Expired != 0 || len(r.dirFiles()) != 2 {
		t.Errorf("29 days, no login: %+v files %v", res, r.dirFiles())
	}
	age(r, 31*24*time.Hour)
	if res := run(r, "", r.f); res.Skipped != 1 || res.Expired != 0 || len(r.dirFiles()) != 2 {
		t.Errorf("31 days, no login now: %+v files %v; a spool that only lacks a login is kept", res, r.dirFiles())
	}
	if res := run(r, "tok-secret", r.f); res.Closed != 1 {
		t.Errorf("after signing in again: %+v", res)
	}

	r, _ = leftoverRun(t)
	if err := os.WriteFile(filepath.Join(r.dir, r.s.p.Open.RunKey+metaExt), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(r, 31*24*time.Hour)
	if res := run(r, "tok-secret", r.f); res.Expired != 1 || len(r.dirFiles()) != 0 {
		t.Errorf("31 days, unusable meta file: %+v files %v", res, r.dirFiles())
	}

	r, _ = leftoverRun(t)
	other := recordingtest.New(t, "tok-other")
	age(r, 31*24*time.Hour)
	if res := run(r, "tok-other", other); res.Expired != 1 || res.NotOurs != 0 || len(r.dirFiles()) != 0 {
		t.Errorf("31 days, another login's: %+v files %v", res, r.dirFiles())
	}

	r, _ = leftoverRun(t)
	age(r, 31*24*time.Hour)
	if res := run(r, "tok-secret", r.f); res.Closed != 1 {
		t.Errorf("31 days but sendable: %+v; an old spool that can be sent is sent", res)
	}

	r, _ = leftoverRun(t)
	meta := filepath.Join(r.dir, r.s.p.Open.RunKey+metaExt)
	_ = os.Remove(meta) // an orphan spool
	age(r, 31*24*time.Hour)
	if res := run(r, "tok-secret", r.f); res.Expired != 1 || len(r.dirFiles()) != 0 {
		t.Errorf("an old spool with no meta file: %+v files %v", res, r.dirFiles())
	}
}

// cutAfter is a transport that passes the first ok uploads and then holds every
// request until its context ends, as a slow link does at the end of a run.
type cutAfter struct {
	next http.RoundTripper
	mu   sync.Mutex
	ok   int
}

func (c *cutAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	hold := r.Method == http.MethodPut && c.ok <= 0
	if r.Method == http.MethodPut {
		c.ok--
	}
	c.mu.Unlock()
	if hold {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return c.next.RoundTrip(r)
}

// A drain that the end-of-run deadline cuts off still saves how far the server
// got. A flush that finds the recording closed as stale then continues from
// there, and no byte reaches the deployment twice.
func TestADrainCutByItsDeadlineSavesWhatTheServerTookAndAContinuationStartsThere(t *testing.T) {
	f := newFake(t)
	clk := newClock()
	f.SetClock(clk.Now)
	f.SetMaxBatch(frame.MaxFrame + 1000)
	c := &Client{Base: f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: &cutAfter{next: f.Client().Transport, ok: 2}}, UserAgent: "brig-test"}
	dir := t.TempDir()
	p := beginParams(f, dir)
	p.Client, p.Now, p.Sleep = c, clk.Now, nil
	p.DrainTimeout = 300 * time.Millisecond
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	for i := 0; i < 3000; i++ {
		at := clk.Now()
		s.rec.Output(at, []byte(strings.Repeat("a line of output ", 8)+"\r\n"))
		s.rec.Output(at.Add(20*time.Millisecond), nil)
	}
	s.rec.Finish(clk.Now(), 0)
	spool, _ := os.ReadFile(s.paths[0])
	zero := 0
	s.finish("exit", &zero, "")

	first := f.Recording(f.Keys()[0])
	if len(first.Raw) == 0 || len(first.Raw) >= len(spool) || first.Closed {
		t.Fatalf("the cut left the server with %d of %d bytes, closed %v", len(first.Raw), len(spool), first.Closed)
	}
	raw, err := os.ReadFile(s.paths[1])
	if err != nil {
		t.Fatalf("the meta file is gone: %v", err)
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Acked != int64(len(first.Raw)) {
		t.Errorf("Acked = %d, the server holds %d", m.Acked, len(first.Raw))
	}

	clk.Advance(3 * time.Minute) // the deployment closes the recording as stale
	old := flushClock
	flushClock = clk.Now
	defer func() { flushClock = old }()
	res, err := flushWith(t, dir, "tok-secret", f)
	if err != nil || res.Closed != 1 {
		t.Fatalf("flush %+v %v", res, err)
	}
	keys := f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	prefix := len(f.Recording(keys[1]).Raw) - (len(spool) - len(first.Raw))
	joined := append(append([]byte(nil), first.Raw...), f.Recording(keys[1]).Raw[prefix:]...)
	if prefix <= 0 || !bytes.Equal(joined, spool) {
		t.Errorf("the second recording repeats or loses bytes: first %d, second %d, spool %d", len(first.Raw), len(f.Recording(keys[1]).Raw), len(spool))
	}
}

// A flush saves how far the deployment got as it goes, so one that is cut off
// part way leaves the position in the meta file.
func TestAFlushSavesProgressAsItAdvances(t *testing.T) {
	f := newFake(t)
	f.SetMaxBatch(frame.MaxFrame + 1000)
	dir := t.TempDir()
	// A run whose uploads never get through leaves the whole spool behind.
	hold := &Client{Base: f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: &cutAfter{next: f.Client().Transport, ok: 0}}, UserAgent: "brig-test"}
	p := beginParams(f, dir)
	p.Client, p.DrainTimeout, p.Sleep = hold, 200*time.Millisecond, nil
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	for i := 0; i < 3000; i++ {
		at := time.Now()
		s.rec.Output(at, []byte(strings.Repeat("a line of output ", 8)+"\r\n"))
		s.rec.Output(at.Add(20*time.Millisecond), nil)
	}
	s.rec.Finish(time.Now(), 0)
	zero := 0
	s.finish("exit", &zero, "")

	// The flush's own link takes the check and two batches, then stalls.
	c := &Client{Base: f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: &cutAfter{next: f.Client().Transport, ok: 3}}, UserAgent: "brig-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, _ = Flush(ctx, dir, func(string) (string, bool) { return "tok-secret", true }, func(string, string) *Client { return c })
	raw, err := os.ReadFile(s.paths[1])
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	var m Meta
	_ = json.Unmarshal(raw, &m)
	held := int64(len(f.Recording(f.Keys()[0]).Raw))
	if held == 0 || m.Acked != held {
		t.Errorf("Acked = %d, the deployment holds %d", m.Acked, held)
	}
}

// A URL that does not parse can never have a token, which no sign-in fixes, so
// its spool is an unusable one and goes after 30 days. A URL that parses but has
// no token now is kept however old, because a sign-in sends it.
func TestASpoolWhoseURLCanNeverHaveATokenAgesOut(t *testing.T) {
	for name, tc := range map[string]struct {
		usable      bool
		wantExpired int
		wantFiles   int
	}{
		"a URL that does not parse":     {false, 1, 0},
		"a URL with no login right now": {true, 0, 2},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := leftoverRun(t)
			old := r.clk.Now().Add(-31 * 24 * time.Hour)
			for _, p := range r.dirFiles() {
				_ = os.Chtimes(p, old, old)
			}
			flushOld := flushClock
			flushClock = r.clk.Now
			defer func() { flushClock = flushOld }()
			res, err := Flush(context.Background(), r.dir, func(string) (string, bool) { return "", tc.usable },
				func(string, string) *Client { return fakeClient(r.f) })
			if err != nil || res.Expired != tc.wantExpired || len(r.dirFiles()) != tc.wantFiles {
				t.Errorf("flush %+v %v, files %v", res, err, r.dirFiles())
			}
		})
	}
}

// A signal that arrives after the command has ended, while Brig drains the
// upload, ends the drain early. Brig exits with the command's own status and not
// with the signal's, and the spool stays for a later flush.
func TestASignalDuringTheDrainEndsItAndTheRunKeepsTheCommandsStatus(t *testing.T) {
	f := newFake(t)
	g := newGate(f.Client().Transport)
	c := &Client{Base: f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: g}, UserAgent: "brig-test"}
	dir := t.TempDir()
	p := beginParams(f, dir)
	p.Client, p.FlushEvery, p.DrainTimeout = c, time.Hour, 30*time.Second
	in, _ := os.Open(os.DevNull)
	out, _ := os.Create(filepath.Join(t.TempDir(), "out"))
	p.In, p.Out = in, out
	notice := &bytes.Buffer{}
	p.Notice = notice
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	g.close() // from here the drain's first request waits
	go func() {
		<-g.waiting // the command has ended and the drain is on the wire
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	go func() {
		code, err := s.Run([]string{"sh", "-c", "exit 7"}, os.Environ())
		done <- res{code, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.code != 7 {
			t.Errorf("Run = %d, %v; want the command's status 7", r.code, r.err)
		}
	case <-time.After(10 * time.Second):
		close(g.open)
		t.Fatal("a signal did not end the drain")
	}
	close(g.open)
	if files := filepath.Join(dir, "*"); len(mustGlob(t, files)) != 2 {
		t.Errorf("the spool must stay: %v", mustGlob(t, files))
	}
	if !strings.Contains(notice.String(), leftoverNotice) {
		t.Errorf("closing lines %q", notice.String())
	}
}

// slowTap holds the first output back, so the relay waits for the tap after the
// command has ended.
type slowTap struct {
	ptyrelay.Tap
	once sync.Once
}

func (s *slowTap) Output(at time.Time, p []byte) {
	s.once.Do(func() { time.Sleep(400 * time.Millisecond) })
	s.Tap.Output(at, p)
}

// sessionAtItsDrain starts a run of cmd whose final drain waits on a closed
// gate. wrap may change what the relay is given. It returns the session, a
// channel for Run's end, the gate, the notice and the spool directory.
func sessionAtItsDrain(t *testing.T, cmd string, wrap func(ptyrelay.Options) ptyrelay.Options) (*Session, chan struct{}, *gate, *bytes.Buffer, string) {
	t.Helper()
	f := newFake(t)
	g := newGate(f.Client().Transport)
	c := &Client{Base: f.URL(), Token: "tok-secret", HTTP: &http.Client{Transport: g}, UserAgent: "brig-test"}
	dir := t.TempDir()
	p := beginParams(f, dir)
	p.Client, p.FlushEvery, p.DrainTimeout = c, time.Hour, 30*time.Second
	in, _ := os.Open(os.DevNull)
	out, _ := os.Create(filepath.Join(t.TempDir(), "out"))
	p.In, p.Out = in, out
	notice := &bytes.Buffer{}
	p.Notice = notice
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	g.close()
	old := relayRun
	relayRun = func(c *exec.Cmd, o ptyrelay.Options) (ptyrelay.Result, error) { return old(c, wrap(o)) }
	t.Cleanup(func() { relayRun = old })
	ended := make(chan struct{})
	go func() {
		_, _ = s.Run([]string{"sh", "-c", cmd}, os.Environ())
		close(ended)
	}()
	return s, ended, g, notice, dir
}

// A Ctrl-C that comes after the command has ended, while the relay still waits
// for the recorder, is kept. It ends the drain, and the person does not have to
// press it again.
func TestASignalAfterTheCommandEndedButBeforeTheRelayReturnedEndsTheDrain(t *testing.T) {
	_, ended, g, notice, dir := sessionAtItsDrain(t, "exit 0", func(o ptyrelay.Options) ptyrelay.Options {
		o.Tap = &slowTap{Tap: o.Tap}
		inner := o.Exited
		o.Exited = func() {
			inner()
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		}
		return o
	})
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		close(g.open)
		t.Fatal("a signal that came after the command ended was dropped")
	}
	close(g.open)
	if n := len(mustGlob(t, filepath.Join(dir, "*"))); n != 2 {
		t.Errorf("the spool must stay: %d files", n)
	}
	if !strings.Contains(notice.String(), leftoverNotice) {
		t.Errorf("closing lines %q", notice.String())
	}
}

// A signal that came while the command ran was forwarded to it by the relay, so
// it does not end the drain as well.
func TestASignalWhileTheCommandRanDoesNotEndTheDrain(t *testing.T) {
	_, ended, g, notice, dir := sessionAtItsDrain(t, "sleep 1", func(o ptyrelay.Options) ptyrelay.Options {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		}()
		return o
	})
	select {
	case <-g.waiting: // the command has ended and the drain is on the wire
	case <-time.After(10 * time.Second):
		close(g.open)
		t.Fatal("the drain never started")
	}
	select {
	case <-ended:
		close(g.open)
		t.Fatal("a signal sent while the command ran ended the drain")
	case <-time.After(500 * time.Millisecond):
	}
	close(g.open)
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the drain went through")
	}
	if n := len(mustGlob(t, filepath.Join(dir, "*"))); n != 0 {
		t.Errorf("the recording was sent whole, so no file stays: %d", n)
	}
	if notice.Len() != 0 {
		t.Errorf("closing lines %q", notice.String())
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A deployment that names an offset outside the spool when it is asked about the
// recording does not make the flush retry the spool for ever: the spool is kept
// for 30 days, as for any spool that cannot be sent, and then removed.
func TestAFlushRemovesASpoolWhoseDeploymentNamesAnOffsetOutsideItAfterThirtyDays(t *testing.T) {
	r, _ := leftoverRun(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"offset_mismatch","next_offset":1099511627776}`))
	}))
	t.Cleanup(srv.Close)
	flush := func() FlushResult {
		old := flushClock
		flushClock = r.clk.Now
		defer func() { flushClock = old }()
		res, err := Flush(context.Background(), r.dir, func(string) (string, bool) { return "tok-secret", true },
			func(url, tok string) *Client { return &Client{Base: srv.URL, Token: tok, HTTP: srv.Client()} })
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	age := func(d time.Duration) {
		old := r.clk.Now().Add(-d)
		for _, p := range r.dirFiles() {
			_ = os.Chtimes(p, old, old)
		}
	}
	age(29 * 24 * time.Hour)
	if res := flush(); res.Pending != 1 || res.Expired != 0 || res.Dropped != 0 || len(r.dirFiles()) != 2 {
		t.Errorf("29 days: %+v files %v, want the spool kept", res, r.dirFiles())
	}
	age(31 * 24 * time.Hour)
	if res := flush(); res.Expired != 1 || len(r.dirFiles()) != 0 {
		t.Errorf("31 days: %+v files %v, want it removed as expired", res, r.dirFiles())
	}
}
