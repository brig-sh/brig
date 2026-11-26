// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
)

// clock is a hand-wound clock shared by the recorder and the fake server.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// run is a Session driven by hand: no relay, no timers, no sleeping.
type run struct {
	t      *testing.T
	f      *recordingtest.Server
	clk    *clock
	s      *Session
	dir    string
	notice *bytes.Buffer
	sl     *sleeps
	n      int
}

func newRun(t *testing.T) *run {
	t.Helper()
	f := newFake(t)
	clk := newClock()
	f.SetClock(clk.Now)
	r := &run{t: t, f: f, clk: clk, dir: t.TempDir(), notice: &bytes.Buffer{}, sl: &sleeps{}}
	p := beginParams(f, r.dir)
	p.Now, p.Sleep, p.Notice = clk.Now, r.sl.sleep, r.notice
	p.DrainTimeout = 100 * time.Millisecond
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	r.s = s
	return r
}

// say makes the terminal print text and completes its frame.
func (r *run) say(text string) {
	r.t.Helper()
	at := r.clk.Now()
	r.s.rec.Output(at, []byte(text))
	r.s.rec.Output(at.Add(20*time.Millisecond), nil)
}

func (r *run) tick() error { r.t.Helper(); return r.s.tick(context.Background()) }

func (r *run) mustTick() {
	r.t.Helper()
	if err := r.tick(); err != nil {
		r.t.Fatalf("tick: %v", err)
	}
}

// end finishes the run as brig does and returns the spool's bytes.
func (r *run) end() []byte {
	r.t.Helper()
	r.s.rec.Finish(r.clk.Now(), 0)
	spool, _ := os.ReadFile(r.s.paths[0])
	zero := 0
	r.s.finish("exit", &zero, "")
	r.s.printNotices()
	return spool
}

func (r *run) keepalives(id string) []recordingtest.Put {
	var out []recordingtest.Put
	for _, p := range r.f.Puts() {
		if p.Keepalive && p.ID == id {
			out = append(out, p)
		}
	}
	return out
}

func TestQuietRunSendsKeepalivesAndKeepsOneRecording(t *testing.T) {
	r := newRun(t)
	r.say("before\r\n")
	r.mustTick()
	for i := 0; i < 600; i++ { // ten quiet minutes, one second at a time
		r.clk.Advance(time.Second)
		r.mustTick()
	}
	ka := r.keepalives("rec-1")
	if len(ka) != 13 {
		t.Fatalf("%d keepalives in 10 minutes, want 13", len(ka))
	}
	for i := 1; i < len(ka); i++ {
		if gap := ka[i].At.Sub(ka[i-1].At); gap != 45*time.Second {
			t.Errorf("keepalive %d came %v after the one before", i, gap)
		}
	}
	if keys := r.f.Keys(); len(keys) != 1 {
		t.Fatalf("recordings %v", keys)
	}
	if rec := r.f.Recording(r.f.Keys()[0]); rec.Closed {
		t.Fatal("the server closed a recording whose recorder was alive")
	}
	r.say("after\r\n")
	r.mustTick()
	r.end()
	rec := r.f.Recording(r.f.Keys()[0])
	if got := string(outputOf(decodeAll(t, rec.Raw))); got != "before\r\nafter\r\n" {
		t.Errorf("output %q", got)
	}
	if !rec.Closed || rec.Stale || rec.End.EndReason != "exit" {
		t.Errorf("close %+v stale %v", rec.End, rec.Stale)
	}
	if r.notice.Len() != 0 {
		t.Errorf("a quiet run printed %q", r.notice)
	}
}

func TestAppendResetsTheKeepaliveClock(t *testing.T) {
	r := newRun(t)
	r.mustTick() // the opening resize frame, at t=0
	r.clk.Advance(20 * time.Second)
	r.say("x")
	r.mustTick() // an append at t=20 s
	// 50 s after the open but only 30 s after the append: no keepalive yet.
	r.clk.Advance(30 * time.Second)
	r.mustTick()
	r.clk.Advance(14 * time.Second)
	r.mustTick() // 44 s after the append
	if n := len(r.keepalives("rec-1")); n != 0 {
		t.Fatalf("%d keepalives less than 45 s after the last append", n)
	}
	r.clk.Advance(time.Second)
	r.mustTick() // 45 s after the append
	ka := r.keepalives("rec-1")
	if len(ka) != 1 {
		t.Fatalf("keepalives %d, want 1 at 45 s after the append", len(ka))
	}
}

// uploaderOn is an uploader at the end of a one-frame spool, on a fake clock.
func uploaderOn(t *testing.T, f *recordingtest.Server, clk *clock, key string) (*Uploader, *sleeps) {
	t.Helper()
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: key})
	sl := &sleeps{}
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "hello\r\n"), MaxBatch: open.MaxBatchBytes,
		WindowBytes: open.WindowBytes, Sleep: sl.sleep, Now: clk.Now}
	if err := u.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	return u, sl
}

type failOnce struct {
	http.RoundTripper
	left int
}

func (f *failOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.left > 0 {
		f.left--
		return nil, errors.New("connection reset")
	}
	return f.RoundTripper.RoundTrip(r)
}

func TestKeepaliveFollowsTheAppendRules(t *testing.T) {
	d := func(s ...int) []time.Duration {
		var o []time.Duration
		for _, v := range s {
			o = append(o, time.Duration(v)*time.Second)
		}
		return o
	}
	cases := []struct {
		name  string
		fail  []int
		sleep []time.Duration
	}{
		{"429 waits 2 s", []int{429, 429}, d(2, 2)},
		{"5xx backs off 1 s doubling", []int{500, 503, 502}, d(1, 2, 4)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			clk := newClock()
			f.SetClock(clk.Now)
			u, sl := uploaderOn(t, f, clk, "k")
			before := len(f.Puts())
			f.FailNextPuts(tc.fail...)
			if err := u.Keepalive(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(sl.d) != len(tc.sleep) {
				t.Fatalf("sleeps %v, want %v", sl.d, tc.sleep)
			}
			for i := range sl.d {
				if sl.d[i] != tc.sleep[i] {
					t.Errorf("sleep %d = %v, want %v", i, sl.d[i], tc.sleep[i])
				}
			}
			puts := f.Puts()[before:]
			if last := puts[len(puts)-1]; !last.Keepalive || last.Offset != u.Offset {
				t.Errorf("last put %+v, offset %d", last, u.Offset)
			}
		})
	}

	t.Run("transport error backs off then succeeds", func(t *testing.T) {
		f := newFake(t)
		clk := newClock()
		f.SetClock(clk.Now)
		u, sl := uploaderOn(t, f, clk, "k")
		u.C.HTTP = &http.Client{Transport: &failOnce{RoundTripper: f.Client().Transport, left: 2}}
		if err := u.Keepalive(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sl.d) != 2 || sl.d[0] != time.Second || sl.d[1] != 2*time.Second {
			t.Errorf("sleeps %v", sl.d)
		}
	})

	t.Run("backoff stops growing at 30 s", func(t *testing.T) {
		f := newFake(t)
		clk := newClock()
		f.SetClock(clk.Now)
		u, sl := uploaderOn(t, f, clk, "k")
		f.FailNextPuts(500, 500, 500, 500, 500, 500, 500)
		if err := u.Keepalive(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := sl.d[len(sl.d)-1]; got != 30*time.Second {
			t.Errorf("last backoff %v", got)
		}
	})

	t.Run("401 and 403 stop at once and keep the spool", func(t *testing.T) {
		for _, st := range []int{401, 403} {
			f := newFake(t)
			clk := newClock()
			f.SetClock(clk.Now)
			u, sl := uploaderOn(t, f, clk, "k")
			before := len(f.Puts())
			f.FailNextPuts(st)
			if err := u.Keepalive(context.Background()); !errors.Is(err, ErrAuth) || len(sl.d) != 0 || len(f.Puts())-before != 1 {
				t.Errorf("%d: err %v sleeps %v", st, err, sl.d)
			}
		}
	})

	t.Run("409 offset_mismatch takes the server's offset", func(t *testing.T) {
		f := newFake(t)
		clk := newClock()
		f.SetClock(clk.Now)
		c := fakeClient(f)
		open, _ := c.Open(context.Background(), OpenRequest{RunKey: "m"})
		rd := spoolWith(t, "abc\r\nabc\r\n")
		all, _ := os.ReadFile(rd.Name())
		half, _ := frame.WholePrefix(all[:len(all)-3])
		f.Preload("m", all[:half])
		u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: rd, Offset: int64(len(all)), MaxBatch: open.MaxBatchBytes,
			WindowBytes: open.WindowBytes, Sleep: (&sleeps{}).sleep, Now: clk.Now}
		if err := u.Keepalive(context.Background()); err != nil {
			t.Fatal(err)
		}
		if u.Offset != int64(half) {
			t.Fatalf("offset %d, want %d", u.Offset, half)
		}
		if err := u.Pump(context.Background()); err != nil || !bytes.Equal(f.Recording("m").Raw, all) {
			t.Fatalf("resume: %v", err)
		}
	})

	t.Run("409 recording_closed is reported, not retried", func(t *testing.T) {
		f := newFake(t)
		clk := newClock()
		f.SetClock(clk.Now)
		u, sl := uploaderOn(t, f, clk, "k")
		clk.Advance(3 * time.Minute)
		if err := u.Keepalive(context.Background()); !errors.Is(err, ErrRecordingClosed) || len(sl.d) != 0 {
			t.Errorf("err %v sleeps %v", err, sl.d)
		}
	})
}

func TestAuthRefusalStopsTheLiveLoopAndItsKeepalives(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.clk.Advance(45 * time.Second)
	r.f.FailNextPuts(401)
	ticks := make(chan time.Time, 10)
	for i := 0; i < 10; i++ {
		ticks <- time.Now()
	}
	before := len(r.f.Puts())
	done := make(chan struct{})
	go func() { r.s.liveLoop(context.Background(), ticks); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the live loop kept going after a 401")
	}
	if n := len(r.f.Puts()) - before; n != 1 {
		t.Errorf("%d requests after the 401, want just the refused keepalive", n)
	}
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 {
		t.Errorf("the spool was not kept: %v", files)
	}
}

// cutOff makes the server's clock pass the stale rule while the recorder's
// requests fail, as when a laptop sleeps or the network is gone.
func (r *run) cutOff() {
	r.f.FailNextPuts(503, 503)
	r.clk.Advance(3 * time.Minute)
}

func TestServerClosesTheRecordingAndTheRunContinuesInASecond(t *testing.T) {
	r := newRun(t)
	r.mustTick() // the opening resize frame goes first, so "one" is sent with a dictionary
	r.say("one\r\n")
	r.mustTick()
	r.clk.Advance(time.Second)
	r.s.rec.Resize(r.clk.Now(), 120, 40)
	r.say("two\r\n") // written to the spool, never taken by the first recording
	r.cutOff()
	r.mustTick()
	r.clk.Advance(5 * time.Second)
	r.say("three\r\n")
	r.mustTick()
	if r.notice.Len() != 0 {
		t.Fatalf("something was printed while the run went on: %q", r.notice)
	}
	spool := r.end()

	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	first, second := r.f.Recording(keys[0]), r.f.Recording(keys[1])
	if !first.Stale || !first.Closed {
		t.Errorf("first recording: closed %v stale %v", first.Closed, first.Stale)
	}
	if !second.Closed || second.Stale || second.End.EndReason != "exit" || second.End.FinalOffset != int64(len(second.Raw)) {
		t.Errorf("second recording: %+v", second.End)
	}
	if second.Open["run_key"] == first.Open["run_key"] {
		t.Error("the second recording reused the run key")
	}
	for _, k := range []string{"sandbox", "ref", "profile", "host", "runtime", "brig_version"} {
		if second.Open[k] != first.Open[k] {
			t.Errorf("open field %s: %v then %v", k, first.Open[k], second.Open[k])
		}
	}
	if second.Open["cols"] != float64(120) || second.Open["rows"] != float64(40) {
		t.Errorf("the second open carried %v by %v", second.Open["cols"], second.Open["rows"])
	}
	// The second recording opens with the inserted resize frame, then the
	// frames the first recording never took. Nothing else.
	inserted := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(120, 40))
	if !bytes.HasPrefix(second.Raw, inserted) {
		t.Fatalf("the second recording does not start with a resize to 120 by 40")
	}
	joined := append(append([]byte(nil), first.Raw...), second.Raw[len(inserted):]...)
	if !bytes.Equal(joined, spool) {
		t.Fatalf("the two recordings hold %d bytes plus the resize, the run's stream is %d: lost or repeated bytes", len(joined), len(spool))
	}
	if got := string(outputOf(decodeAll(t, first.Raw))) + string(outputOf(decodeAll(t, second.Raw))); got != "one\r\ntwo\r\nthree\r\n" {
		t.Errorf("output %q", got)
	}
	// No dictionary for the first batch of the new recording; the first
	// recording's later batches did use one.
	var firstPut, dictInFirst bool
	for _, p := range r.f.Puts() {
		if p.ID == second.ID && !p.Keepalive && p.Status == 200 && !firstPut {
			firstPut = true
			if p.Offset != 0 || p.Dict {
				t.Errorf("first batch of the second recording: %+v", p)
			}
		}
		if p.ID == first.ID && p.Dict {
			dictInFirst = true
		}
	}
	if !firstPut || !dictInFirst {
		t.Errorf("first batch seen %v, dictionary seen in the first recording %v", firstPut, dictInFirst)
	}
	want := "brig: this run's recording was interrupted and continues in a second recording\n"
	if r.notice.String() != want {
		t.Errorf("closing lines %q", r.notice)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
}

func TestRunThatEndsWhileCutOffFinishesInASecondRecording(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.clk.Advance(3 * time.Minute) // asleep: no tick, no keepalive
	spool := r.end()
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	first, second := r.f.Recording(keys[0]), r.f.Recording(keys[1])
	inserted := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), first.Raw...), second.Raw[len(inserted):]...)
	if !bytes.HasPrefix(second.Raw, inserted) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d, stream %d", len(first.Raw), len(second.Raw), len(spool))
	}
	if !second.Closed || second.End.EndReason != "exit" {
		t.Errorf("second %+v", second.End)
	}
	if !strings.Contains(r.notice.String(), "continues in a second recording") {
		t.Errorf("closing line %q", r.notice)
	}
}

func TestSecondOpenNotRecordedStopsRecordingAndRemovesTheSpool(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	r.f.SetRecorded(false)
	stoppedAt := r.clk.Now()
	if err := r.tick(); !errors.Is(err, errStopped) {
		t.Fatalf("tick: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("the spool was kept: %v", left)
	}
	if r.notice.Len() != 0 {
		t.Fatalf("printed while the run went on: %q", r.notice)
	}
	r.clk.Advance(time.Minute)
	r.say("more\r\n") // the run goes on; nothing is recorded
	r.end()
	want := "brig: recording stopped before the run ended (your organization turned recording off); output after " +
		stoppedAt.Local().Format("15:04:05") + " is not recorded\n"
	if r.notice.String() != want {
		t.Errorf("closing lines %q\nwant %q", r.notice, want)
	}
	if n := len(r.f.Keys()); n != 1 {
		t.Errorf("%d recordings", n)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files came back: %v", left)
	}
}

// advancing makes every wait move the run's clock, as a real wait would, and
// records it.
func (r *run) advancing() *[]time.Duration {
	var waits []time.Duration
	r.s.up.Sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		r.clk.Advance(d)
		return ctx.Err()
	}
	return &waits
}

// joinedStream checks that the recordings together are the spool plus one
// inserted resize frame for every recording after the first, and returns how
// many there are.
func (r *run) checkNoLoss(spool []byte) int {
	r.t.Helper()
	resize := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	keys := r.f.Keys()
	var joined []byte
	for i, k := range keys {
		raw := r.f.Recording(k).Raw
		if i > 0 {
			if len(raw) > 0 && !bytes.HasPrefix(raw, resize) {
				r.t.Fatalf("recording %d does not start with the resize frame", i+1)
			}
			raw = raw[min(len(resize), len(raw)):]
		}
		joined = append(joined, raw...)
	}
	if !bytes.Equal(joined, spool) {
		r.t.Fatalf("%d recordings hold %d bytes, the run's stream is %d: lost or repeated bytes", len(keys), len(joined), len(spool))
	}
	return len(keys)
}

func TestARunInterruptedEightTimesAcrossADayUsesNineRecordingsAndNeverWaits(t *testing.T) {
	r := newRun(t)
	waits := r.advancing()
	r.say("start\r\n")
	r.mustTick()
	for i := 1; i <= 8; i++ {
		r.clk.Advance(2 * time.Hour) // the lid is shut: no keepalive, the server closes the recording
		r.say("after lid " + string(rune('0'+i)) + "\r\n")
		r.mustTick()
		if r.s.stopped {
			t.Fatalf("recording stopped after interruption %d", i)
		}
	}
	r.say("last\r\n")
	r.mustTick()
	spool := r.end()
	if len(*waits) != 0 {
		t.Errorf("the guard made the recorder wait: %v", *waits)
	}
	if n := r.checkNoLoss(spool); n != 9 {
		t.Fatalf("%d recordings, want 9", n)
	}
	if want := "brig: this run's recording was interrupted and continues in a ninth recording\n"; r.notice.String() != want {
		t.Errorf("closing lines %q", r.notice)
	}
}

func TestLoopGuardSpacesOpensWhenTheServerClosesEveryContinuation(t *testing.T) {
	r := newRun(t)
	waits := r.advancing()
	r.mustTick()
	r.say("one\r\n")
	r.mustTick()
	r.f.SetCloseOnOpen(true)
	r.clk.Advance(3 * time.Minute)
	r.say("two\r\n")
	detected := r.clk.Now()
	for i := 0; i < 6; i++ {
		r.mustTick()
		r.say("more\r\n")
	}
	keys := r.f.Keys()
	if len(keys) != 7 {
		t.Fatalf("%d recordings, want the first and 6 continuations", len(keys))
	}
	prev := detected
	var gaps []time.Duration
	for _, k := range keys[1:] {
		at := r.f.Recording(k).OpenedAt
		gaps = append(gaps, at.Sub(prev))
		prev = at
	}
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if gaps[i] != want[i] {
			t.Errorf("open %d came %v after the one before, want %v (all %v)", i+1, gaps[i], want[i], gaps)
			break
		}
	}
	if len(*waits) != 5 {
		t.Errorf("waits %v", *waits)
	}
	// The spool is whole the whole time, and nothing but the first recording
	// took bytes.
	spool, _ := os.ReadFile(r.s.paths[0])
	first := r.f.Recording(keys[0]).Raw
	if !bytes.HasPrefix(spool, first) || len(spool) <= len(first) {
		t.Errorf("spool %d bytes, first recording %d", len(spool), len(first))
	}
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 || r.s.stopped {
		t.Errorf("files %v stopped %v", files, r.s.stopped)
	}
}

func TestAContinuationThatStaysOpenTenMinutesClearsTheWait(t *testing.T) {
	r := newRun(t)
	waits := r.advancing()
	r.mustTick()
	r.say("one\r\n")
	r.mustTick()
	r.f.SetCloseOnOpen(true)
	r.clk.Advance(3 * time.Minute)
	r.say("two\r\n")
	r.mustTick() // c1, at once
	r.mustTick() // c1 closed early: c2 after 60 s
	r.mustTick() // c2 closed early: c3 after 120 s
	if want := []time.Duration{time.Minute, 2 * time.Minute}; len(*waits) != 2 || (*waits)[0] != want[0] || (*waits)[1] != want[1] {
		t.Fatalf("waits %v, want %v", *waits, want)
	}
	r.f.SetCloseOnOpen(false)
	r.mustTick() // c3 opens after 240 s and is not closed by the server
	if n := len(*waits); n != 3 || (*waits)[2] != 4*time.Minute {
		t.Fatalf("waits %v", *waits)
	}
	for i := 0; i < 11*60; i++ { // alive for 11 minutes
		r.clk.Advance(time.Second)
		r.mustTick()
	}
	r.clk.Advance(3 * time.Minute) // cut off
	r.say("three\r\n")
	before := len(*waits)
	r.mustTick()
	if len(*waits) != before {
		t.Errorf("waited %v after a continuation that lived 11 minutes", (*waits)[before:])
	}
	keys := r.f.Keys()
	// The first, three the server closed at once, one that lived, and the
	// continuation opened after it was cut off.
	if n := len(keys); n != 6 {
		t.Fatalf("%d recordings, want 6", n)
	}
	if last := r.f.Recording(keys[5]); last.OpenedAt != r.clk.Now() {
		t.Errorf("the fifth recording opened at %v, want %v", last.OpenedAt, r.clk.Now())
	}
	if r.s.meta.Wait != 0 {
		t.Errorf("wait %v", r.s.meta.Wait)
	}
}

func TestRunEndingDuringAGuardWaitKeepsTheSpoolAndFlushFinishesIt(t *testing.T) {
	r := newRun(t)
	r.advancing()
	r.mustTick()
	r.say("one\r\n")
	r.clk.Advance(3 * time.Second)
	r.mustTick()
	r.f.SetCloseOnOpen(true)
	r.clk.Advance(3 * time.Minute)
	r.say("two\r\n")
	r.mustTick() // c1 opens at once and the server closes it at its first request
	r.say("three\r\n")
	spool := r.end() // the run ends while the guard says wait 60 s
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 {
		t.Fatalf("the spool and its meta must stay: %v", files)
	}
	want := "brig: this run's recording was interrupted and continues in a second recording\n" +
		"brig: the end of this run's recording is still on this machine; the next recorded run or `brig recordings flush` will send it\n"
	if r.notice.String() != want {
		t.Errorf("closing lines %q", r.notice)
	}
	if n := len(r.f.Keys()); n != 2 {
		t.Fatalf("%d recordings", n)
	}
	r.f.SetCloseOnOpen(false)
	// Inside the wait, a flush leaves the spool alone and opens nothing.
	if res := r.flush(); res.Closed != 0 || res.Pending != 1 || len(r.f.Keys()) != 2 {
		t.Fatalf("flush inside the wait: %+v, %d recordings", res, len(r.f.Keys()))
	}
	r.clk.Advance(2 * time.Minute)
	if res := r.flush(); res.Closed != 1 {
		t.Fatalf("flush %+v", res)
	}
	if n := r.checkNoLoss(spool); n != 3 {
		t.Fatalf("%d recordings, want 3", n)
	}
	last := r.f.Recording(r.f.Keys()[2])
	if !last.Closed || last.End.EndReason != "exit" {
		t.Errorf("close %+v", last.End)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
}

func TestClosingLineCountsRecordingsInWordsUpToTenAndDigitsAbove(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{2, "continues in a second recording"},
		{10, "continues in a tenth recording"},
		{11, "and the run used 11 recordings"},
		{12, "and the run used 12 recordings"},
	} {
		s := &Session{interrupted: true, meta: Meta{Recordings: tc.n}}
		got := s.Notices()
		if len(got) != 1 || !strings.HasSuffix(got[0], tc.want) {
			t.Errorf("%d: %q", tc.n, got)
		}
	}
}

// crash drops the recorder without a close, as a killed brig does.
func (r *run) crash() []byte {
	r.t.Helper()
	spool, _ := os.ReadFile(r.s.paths[0])
	r.s.rec.Abort()
	r.s.rec.Close()
	return spool
}

func (r *run) flush() FlushResult {
	r.t.Helper()
	old := flushClock
	flushClock = r.clk.Now
	defer func() { flushClock = old }()
	res, err := Flush(context.Background(), r.dir, func(string) (string, bool) { return "tok-secret", true },
		func(url, tok string) *Client { return fakeClient(r.f) })
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func TestFlushFinishesASpoolFromATwoRecordingRun(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	r.mustTick() // the second recording opens and takes "two"
	r.say("three\r\n")
	spool := r.crash() // "three" is in the spool, not at the server
	if res := r.flush(); res.Closed != 1 {
		t.Fatalf("flush %+v", res)
	}
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	first, second := r.f.Recording(keys[0]), r.f.Recording(keys[1])
	inserted := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), first.Raw...), second.Raw[len(inserted):]...)
	if !bytes.HasPrefix(second.Raw, inserted) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d, spool %d", len(first.Raw), len(second.Raw), len(spool))
	}
	if !second.Closed || second.End.EndReason != "recorder_error" || second.End.FinalOffset != int64(len(second.Raw)) {
		t.Errorf("close %+v", second.End)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
}

func TestFlushContinuesInAnotherRecordingWhenTheSecondWasClosedToo(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	r.mustTick()
	r.clk.Advance(3 * time.Second)
	r.say("three\r\n")
	r.mustTick() // the second recording holds "three"; the meta file says so
	r.say("four\r\n")
	spool := r.crash()
	r.clk.Advance(3 * time.Minute) // the server closes the second recording as stale
	if res := r.flush(); res.Closed != 1 {
		t.Fatalf("flush %+v", res)
	}
	keys := r.f.Keys()
	if len(keys) != 3 {
		t.Fatalf("recordings %v", keys)
	}
	a, b, c := r.f.Recording(keys[0]), r.f.Recording(keys[1]), r.f.Recording(keys[2])
	resize := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), a.Raw...), b.Raw[len(resize):]...)
	joined = append(joined, c.Raw[len(resize):]...)
	if !bytes.HasPrefix(c.Raw, resize) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d + %d, spool %d", len(a.Raw), len(b.Raw), len(c.Raw), len(spool))
	}
	if !c.Closed || c.End.EndReason != "recorder_error" {
		t.Errorf("close %+v", c.End)
	}
}

func TestSpoolPosMapsRecordingOffsetsToSpoolPositions(t *testing.T) {
	u := &Uploader{Base: 1000, Prefix: make([]byte, 7)}
	for _, tc := range []struct{ off, want int64 }{{0, 1000}, {7, 1000}, {8, 1001}, {107, 1100}} {
		if got := u.SpoolPos(tc.off); got != tc.want {
			t.Errorf("SpoolPos(%d) = %d, want %d", tc.off, got, tc.want)
		}
	}
	if u := (&Uploader{}); u.SpoolPos(42) != 42 {
		t.Error("a first recording maps offsets to themselves")
	}
}

func TestRecoveryOpenRetriesThroughTransportErrorsAndServerErrors(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	// Two dropped connections, a 503 and a 429, then the open works.
	r.f.FailNextOpens(0, 0, 503, 429)
	r.sl.d = nil
	r.mustTick()
	// The first two waits are the append's own 503s before the server's 409.
	want := []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second, 4 * time.Second, 2 * time.Second}
	if len(r.sl.d) != len(want) {
		t.Fatalf("waits %v, want %v", r.sl.d, want)
	}
	for i := range want {
		if r.sl.d[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, r.sl.d[i], want[i])
		}
	}
	if r.s.stopped {
		t.Fatal("recording stopped on a failed open")
	}
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 {
		t.Fatalf("spool or meta removed: %v", files)
	}
	r.say("three\r\n")
	r.mustTick()
	spool := r.end()
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	first, second := r.f.Recording(keys[0]), r.f.Recording(keys[1])
	inserted := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), first.Raw...), second.Raw[len(inserted):]...)
	if !bytes.HasPrefix(second.Raw, inserted) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d, stream %d", len(first.Raw), len(second.Raw), len(spool))
	}
	if !second.Closed || second.End.EndReason != "exit" {
		t.Errorf("second %+v", second.End)
	}
	if want := "brig: this run's recording was interrupted and continues in a second recording\n"; r.notice.String() != want {
		t.Errorf("closing lines %q", r.notice)
	}
}

func TestRunEndingWhileTheRecoveryOpenFailsKeepsTheSpoolForFlush(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.say("one\r\n")
	r.clk.Advance(3 * time.Second) // long enough for the meta file to record the ack
	r.mustTick()
	r.say("two\r\n")
	r.cutOff()
	r.f.FailNextOpens(503, 503, 503, 503, 503, 503, 503, 503, 503, 503)
	// The run ends while the open still fails: the wait is cut short.
	waits := 0
	r.s.up.Sleep = func(ctx context.Context, d time.Duration) error {
		if waits++; waits > 3 {
			return context.Canceled
		}
		return nil
	}
	if err := r.tick(); !errors.Is(err, context.Canceled) {
		t.Fatalf("tick: %v", err)
	}
	spool := r.end()
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 {
		t.Fatalf("the spool and its meta must stay: %v", files)
	}
	want := "brig: the end of this run's recording is still on this machine; the next recorded run or `brig recordings flush` will send it\n"
	if r.notice.String() != want {
		t.Errorf("closing lines %q", r.notice)
	}
	if n := len(r.f.Keys()); n != 1 {
		t.Fatalf("%d recordings opened while the open failed", n)
	}
	r.f.ClearFailures()
	if res := r.flush(); res.Closed != 1 {
		t.Fatalf("flush %+v", res)
	}
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	first, second := r.f.Recording(keys[0]), r.f.Recording(keys[1])
	inserted := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), first.Raw...), second.Raw[len(inserted):]...)
	if !bytes.HasPrefix(second.Raw, inserted) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d, stream %d", len(first.Raw), len(second.Raw), len(spool))
	}
	if !second.Closed || second.End.EndReason != "exit" {
		t.Errorf("second %+v", second.End)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(left) != 0 {
		t.Errorf("files left %v", left)
	}
}

func blockUntilDone(ctx context.Context, d time.Duration) error { <-ctx.Done(); return ctx.Err() }

const leftoverLine = "brig: the end of this run's recording is still on this machine; the next recorded run or `brig recordings flush` will send it\n"

func tokenOK(string) (string, bool) { return "tok-secret", true }

// leftoverRun is a run that ended with the deployment unreachable: "one" was
// taken, "two" and the exit frame were not. It returns the run and its spool.
func leftoverRun(t *testing.T) (*run, []byte) {
	t.Helper()
	r := newRun(t)
	r.mustTick()
	r.say("one\r\n")
	r.clk.Advance(3 * time.Second) // long enough for the meta file to record the ack
	r.mustTick()
	r.say("two\r\n")
	r.f.FailNextPuts(503, 503, 503)
	r.f.FailNextCloses(503, 503, 503)
	r.s.up.Sleep = blockUntilDone
	spool := r.end()
	r.f.ClearFailures() // the deployment is back
	return r, spool
}

func TestRunEndingWithTheDeploymentUnreachablePrintsTheLeftoverLineOnce(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	if r.notice.Len() != 0 {
		t.Fatal("printed during the run")
	}
	r.f.FailNextPuts(503, 503, 503)
	r.f.FailNextCloses(503, 503, 503)
	r.s.up.Sleep = blockUntilDone // every wait lasts until the drain bound ends it
	start := time.Now()
	r.end()
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("the end of the run waited %v for an unreachable deployment", el)
	}
	if r.notice.String() != leftoverLine {
		t.Errorf("closing lines %q", r.notice)
	}
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 2 {
		t.Errorf("the spool and its meta must stay: %v", files)
	}
}

func TestACleanRunPrintsNoClosingLine(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.end()
	if r.notice.Len() != 0 {
		t.Errorf("printed %q", r.notice)
	}
	if files, _ := filepath.Glob(filepath.Join(r.dir, "*")); len(files) != 0 {
		t.Errorf("files left %v", files)
	}
}

// nextRun records one more run in the same directory on the same fake, with the
// background flush on. It returns once the leftover recording is closed at the
// server, so the run is long enough for the flush to finish.
func nextRun(t *testing.T, r *run, leftoverKey string) *bytes.Buffer {
	t.Helper()
	notice := &bytes.Buffer{}
	p := beginParams(r.f, r.dir)
	p.Now, p.Sleep, p.Notice = r.clk.Now, nil, notice
	p.Token = tokenOK
	p.NewClient = func(url, tok string) *Client { return fakeClient(r.f) }
	in, _ := os.Open(os.DevNull)
	out, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	p.In, p.Out = in, out
	old := flushClock
	flushClock = r.clk.Now
	t.Cleanup(func() { flushClock = old })
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	flag := filepath.Join(t.TempDir(), "go")
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if rec := r.f.Recording(leftoverKey); rec != nil && rec.Closed && len(r.dirFiles()) == 2 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = os.WriteFile(flag, nil, 0o600)
	}()
	script := "until [ -e " + flag + " ]; do sleep 0.01; done; echo next-run"
	if _, err := s.Run([]string{"sh", "-c", script}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	return notice
}

// dirFiles lists the files in the spool directory.
func (r *run) dirFiles() []string {
	files, _ := filepath.Glob(filepath.Join(r.dir, "*"))
	return files
}

func TestNextRecordedRunSendsTheLeftoverInTheBackground(t *testing.T) {
	r, spool := leftoverRun(t)
	if r.notice.String() != leftoverLine {
		t.Fatalf("closing lines %q", r.notice)
	}
	oldKey := r.f.Keys()[0]
	if got := r.f.Recording(oldKey); got.Closed || bytes.Equal(got.Raw, spool) {
		t.Fatal("the leftover was already sent")
	}
	notice := nextRun(t, r, oldKey)
	keys := r.f.Keys()
	if len(keys) != 2 {
		t.Fatalf("recordings %v", keys)
	}
	old, own := r.f.Recording(oldKey), r.f.Recording(keys[1])
	if !bytes.Equal(old.Raw, spool) || !old.Closed || old.End.EndReason != "exit" {
		t.Errorf("the earlier recording holds %d of %d bytes, closed %v %+v", len(old.Raw), len(spool), old.Closed, old.End)
	}
	fs := decodeAll(t, own.Raw)
	if !own.Closed || fs[len(fs)-1].Type != frame.Exit || !bytes.Contains(outputOf(fs), []byte("next-run")) {
		t.Errorf("the new run's own recording: closed %v output %q", own.Closed, outputOf(fs))
	}
	if want := "brig: sent the rest of 1 earlier recording\n"; notice.String() != want {
		t.Errorf("closing lines %q", notice)
	}
	if files := r.dirFiles(); len(files) != 0 {
		t.Errorf("files left %v", files)
	}
}

func TestNextRecordedRunContinuesALeftoverTheServerClosedAsStale(t *testing.T) {
	r, spool := leftoverRun(t)
	oldKey := r.f.Keys()[0]
	r.clk.Advance(3 * time.Minute) // the server closes the earlier recording as stale
	notice := nextRun(t, r, oldKey)
	keys := r.f.Keys()
	if len(keys) != 3 {
		t.Fatalf("recordings %v", keys)
	}
	old, own, cont := r.f.Recording(oldKey), r.f.Recording(keys[1]), r.f.Recording(keys[2])
	resize := frame.Append(nil, 0, frame.Resize, frame.ResizePayload(100, 30))
	joined := append(append([]byte(nil), old.Raw...), cont.Raw[min(len(resize), len(cont.Raw)):]...)
	if !old.Stale || !bytes.HasPrefix(cont.Raw, resize) || !bytes.Equal(joined, spool) {
		t.Fatalf("lost or repeated bytes: %d + %d, spool %d", len(old.Raw), len(cont.Raw), len(spool))
	}
	if !cont.Closed || cont.End.EndReason != "exit" || !own.Closed {
		t.Errorf("close %+v own closed %v", cont.End, own.Closed)
	}
	if want := "brig: sent the rest of 1 earlier recording\n"; notice.String() != want {
		t.Errorf("closing lines %q", notice)
	}
}

func TestBackgroundFlushSkipsALiveRunAndALockedDirectory(t *testing.T) {
	r := newRun(t)
	r.mustTick()
	r.say("live\r\n")
	nc := func(url, tok string) *Client { return fakeClient(r.f) }
	putsBefore := len(r.f.Puts())
	res, err := flushDir(context.Background(), r.dir, tokenOK, nc, flushOpts{stopOnAuth: true})
	if err != nil || res.Skipped != 1 || res.Closed != 0 || len(r.f.Puts()) != putsBefore {
		t.Fatalf("a live run's spool was touched: %+v %v", res, err)
	}
	r.s.rec.Abort() // the run dies; its spool is now a leftover
	r.s.rec.Close()
	lock, err := os.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	res, err = flushDir(context.Background(), r.dir, tokenOK, nc, flushOpts{stopOnAuth: true})
	if err != nil || res.Skipped != 1 || res.Closed != 0 || len(r.f.Puts()) != putsBefore {
		t.Fatalf("a flush ran while another held the lock: %+v %v", res, err)
	}
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	res, err = flushDir(context.Background(), r.dir, tokenOK, nc, flushOpts{stopOnAuth: true})
	if err != nil || res.Closed != 1 {
		t.Fatalf("flush after the lock was released: %+v %v", res, err)
	}
}

func TestEndingTheRunCancelsAFlushWithoutLossOrDuplicates(t *testing.T) {
	r, spool := leftoverRun(t)
	oldKey := r.f.Keys()[0]
	before := len(r.f.Recording(oldKey).Raw)
	nc := func(url, tok string) *Client { return fakeClient(r.f) }
	old := flushClock
	flushClock = r.clk.Now
	defer func() { flushClock = old }()

	// A flush that is mid-way through retrying a 503 when its run ends.
	for i := 0; i < 50; i++ {
		r.f.FailNextPuts(503)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, _ := flushDir(ctx, r.dir, tokenOK, nc, flushOpts{stopOnAuth: true})
	if el := time.Since(start); el > 3*time.Second || res.Closed != 0 {
		t.Fatalf("flush %+v after %v", res, el)
	}
	// The same cancel through a session's own background flush.
	p := beginParams(r.f, t.TempDir())
	p.Dir = r.dir
	p.Token, p.NewClient = tokenOK, nc
	s2, err := Begin(context.Background(), p)
	if err != nil || s2 == nil {
		t.Fatalf("begin: %v %v", s2, err)
	}
	stop := s2.startEarlier()
	if n := stop().Closed; n != 0 {
		t.Errorf("a cancelled flush reported %d finished", n)
	}
	r.f.ClearFailures()
	if got := len(r.f.Recording(oldKey).Raw); got != before {
		t.Fatalf("the cancelled flush changed the recording: %d bytes, was %d", got, before)
	}
	if files := r.dirFiles(); len(files) != 4 { // the leftover's pair and the new run's pair
		t.Fatalf("files %v", files)
	}
	// A later run finishes it: the stream is whole, nothing twice.
	s2.rec.Abort()
	s2.rec.Close()
	_ = os.Remove(s2.paths[0])
	_ = os.Remove(s2.paths[1])
	if res, err := flushDir(context.Background(), r.dir, tokenOK, nc, flushOpts{}); err != nil || res.Closed != 1 {
		t.Fatalf("second flush %+v %v", res, err)
	}
	if got := r.f.Recording(oldKey); !bytes.Equal(got.Raw, spool) || !got.Closed {
		t.Errorf("recording holds %d of %d bytes, closed %v", len(got.Raw), len(spool), got.Closed)
	}
}

// A background flush that is stuck in a token read, as when the keychain is
// slow to answer, when the run ends still reports the recordings it finished
// before it got stuck. The wait for it does not grow.
func TestEndingTheRunReportsWhatAStuckFlushHadFinished(t *testing.T) {
	r1, _ := leftoverRun(t)
	r2, _ := leftoverRun(t)
	// Both leftovers go in one directory; each is on its own fake deployment.
	for _, p := range r2.dirFiles() {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r1.dir, filepath.Base(p)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nc := func(url, tok string) *Client {
		if url == r2.f.URL() {
			return fakeClient(r2.f)
		}
		return fakeClient(r1.f)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	token := func(string) (string, bool) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			<-release // the keychain has not answered
		}
		return "tok-secret", true
	}
	defer close(release)
	old := earlierWait
	earlierWait = 500 * time.Millisecond
	defer func() { earlierWait = old }()

	p := beginParams(r1.f, t.TempDir())
	p.Dir = r1.dir
	p.Token, p.NewClient = token, nc
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	defer func() {
		s.rec.Abort()
		s.rec.Close()
	}()
	stop := s.startEarlier()
	// The second token read is the one that sticks, so the first spool is done.
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the flush never reached the second spool")
		}
	}
	start := time.Now()
	got := stop()
	if got.Closed != 1 {
		t.Errorf("the run reported %+v, want the 1 recording the flush finished", got)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("the end of the run waited %v", el)
	}
}

func TestNoBackgroundFlushWhenTheOpenIsNotRecordedOrFails(t *testing.T) {
	r, _ := leftoverRun(t)
	puts := len(r.f.Puts())
	p := beginParams(r.f, r.dir)
	p.Token, p.NewClient = tokenOK, func(url, tok string) *Client { return fakeClient(r.f) }
	r.f.SetRecorded(false)
	if s, err := Begin(context.Background(), p); s != nil || err != nil {
		t.Fatalf("begin on recorded false: %v %v", s, err)
	}
	r.f.SetRecorded(true)
	r.f.FailNextOpens(503)
	if s, err := Begin(context.Background(), p); s != nil || err == nil {
		t.Fatalf("begin on a failed open: %v %v", s, err)
	}
	if len(r.f.Puts()) != puts || len(r.dirFiles()) != 2 || r.f.Recording(r.f.Keys()[0]).Closed {
		t.Errorf("the leftover was touched: %d puts, files %v", len(r.f.Puts())-puts, r.dirFiles())
	}
}

func TestClosingLineForEarlierRecordingsIsSingularOrPlural(t *testing.T) {
	for n, want := range map[int]string{1: "brig: sent the rest of 1 earlier recording", 3: "brig: sent the rest of 3 earlier recordings"} {
		s := &Session{earlier: n}
		if got := s.Notices(); len(got) != 1 || got[0] != want {
			t.Errorf("%d: %q", n, got)
		}
	}
	if got := (&Session{}).Notices(); len(got) != 0 {
		t.Errorf("nothing earlier: %q", got)
	}
}
