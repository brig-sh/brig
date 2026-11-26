// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/ptyrelay"
	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
)

func TestRecorderFramesDropMarkerAndMode(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	r, err := OpenSpool(filepath.Join(dir, "a.spool"), start, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.Resize(start, 80, 24)
	r.Resize(start.Add(time.Millisecond), 80, 24) // same size: no frame
	r.Output(start.Add(5*time.Millisecond), []byte("one "))
	r.Output(start.Add(8*time.Millisecond), []byte("two"))
	r.Dropped(start.Add(500*time.Millisecond), 1234)
	r.Output(start.Add(600*time.Millisecond), []byte("three"))
	r.Resize(start.Add(700*time.Millisecond), 100, 30)
	r.Finish(start.Add(time.Second), 3)

	st, _ := os.Stat(r.Path())
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	raw, _ := os.ReadFile(r.Path())
	fs := decodeAll(t, raw)
	var kinds string
	for _, f := range fs {
		kinds += string(f.Type)
	}
	if kinds != "romorx" {
		t.Fatalf("kinds %q", kinds)
	}
	if string(fs[1].Payload) != "one two" {
		t.Errorf("payloads %q", fs[1].Payload)
	}
	if string(fs[2].Type) != "m" || string(fs[2].Payload) != "dropped 1234 bytes" {
		t.Errorf("marker %q", fs[2].Payload)
	}
	last := fs[len(fs)-1]
	if last.Type != frame.Exit || last.Payload[0] != 3 {
		t.Errorf("exit frame %+v", last)
	}
}

func TestRecorderTimerWritesPendingOutput(t *testing.T) {
	r, err := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	r.Output(time.Now(), []byte("lonely"))
	deadline := time.Now().Add(2 * time.Second)
	for r.Size() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.Size() == 0 {
		t.Fatal("pending output was never written")
	}
	r.Abort()
}

func TestRecorderSpoolCapStopsRecordingWithAMarker(t *testing.T) {
	start := time.Now()
	r, err := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), start, 300)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r.Output(start.Add(time.Duration(i)*50*time.Millisecond), bytes.Repeat([]byte("x"), 100))
	}
	r.Finish(start.Add(2*time.Second), 0)
	if !r.Capped() {
		t.Fatal("not capped")
	}
	raw, _ := os.ReadFile(r.Path())
	fs := decodeAll(t, raw)
	last := fs[len(fs)-1]
	if last.Type != frame.Marker || !strings.Contains(string(last.Payload), "spool cap") {
		t.Errorf("last frame %c %q", last.Type, last.Payload)
	}
	if int64(len(raw)) > 300+64 {
		t.Errorf("spool is %d bytes", len(raw))
	}
	for _, f := range fs {
		if f.Type == frame.Exit {
			t.Error("a capped recording has no exit frame")
		}
	}
}

func TestUploaderContiguousBatchesAndCloseAfterDrain(t *testing.T) {
	f := newFake(t)
	f.SetMaxBatch(frame.MaxFrame + 1000)
	c := fakeClient(f)
	dir := t.TempDir()
	start := time.Now()
	rec, _ := OpenSpool(filepath.Join(dir, "a.spool"), start, 0)
	for i := 0; i < 2000; i++ {
		rec.Output(start.Add(time.Duration(i)*20*time.Millisecond), []byte(strings.Repeat("line of text ", 8)+"\r\n"))
	}
	rec.Finish(start.Add(10*time.Second), 0)
	open, err := c.Open(context.Background(), OpenRequest{RunKey: "k1", Cols: 80, Rows: 24})
	if err != nil || !open.Recorded {
		t.Fatalf("open: %+v %v", open, err)
	}
	rd, _ := os.Open(rec.Path())
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: rd, MaxBatch: open.MaxBatchBytes, WindowBytes: open.WindowBytes}
	if err := u.Close(context.Background(), CloseRequest{EndReason: "exit"}); err != nil {
		t.Fatal(err)
	}
	spool, _ := os.ReadFile(rec.Path())
	if !bytes.Equal(f.Recording("k1").Raw, spool) {
		t.Fatal("server bytes differ from spool")
	}
	puts := f.Puts()
	if len(puts) < 3 {
		t.Errorf("expected several batches, got %d", len(puts))
	}
	var prev int64 = -1
	for i, p := range puts {
		if p.Status != 200 || (i == 0 && p.Offset != 0) || p.Offset <= prev {
			t.Fatalf("put %d: offset %d status %d", i, p.Offset, p.Status)
		}
		if _, err := time.Parse(time.RFC3339Nano, p.HostTime); err != nil {
			t.Errorf("host time %q: %v", p.HostTime, err)
		}
		prev = p.Offset
	}
	if r := f.Recording("k1"); !r.Closed || r.End.FinalOffset != int64(len(spool)) || r.End.EndReason != "exit" {
		t.Errorf("close %+v", r.End)
	}
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func spoolWith(t *testing.T, text string) *os.File {
	t.Helper()
	start := time.Now()
	rec, err := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), start, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec.Output(start, []byte(text))
	rec.Finish(start.Add(time.Second), 0)
	rd, err := os.Open(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

func TestUploaderStatusHandling(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: "k2"})
	sl := &sleeps{}
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "hello\r\n"),
		MaxBatch: open.MaxBatchBytes, WindowBytes: open.WindowBytes, Sleep: sl.sleep}

	f.FailNextPuts(429, 500, 502, 503, 429)
	if err := u.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2 * time.Second, time.Second, 2 * time.Second, 4 * time.Second, 2 * time.Second}
	if len(sl.d) != len(want) {
		t.Fatalf("sleeps %v", sl.d)
	}
	for i := range want {
		if sl.d[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, sl.d[i], want[i])
		}
	}
	if !bytes.Contains(f.Recording("k2").Raw, []byte("hello")) {
		t.Error("batch never arrived")
	}
}

func TestUploaderBackoffCapsAt30s(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: "k3"})
	sl := &sleeps{}
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "x"),
		MaxBatch: open.MaxBatchBytes, WindowBytes: open.WindowBytes, Sleep: sl.sleep}
	f.FailNextPuts(500, 500, 500, 500, 500, 500, 500, 500)
	if err := u.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sl.d[len(sl.d)-1]; got != 30*time.Second {
		t.Errorf("last backoff %v", got)
	}
}

func TestUploaderResumesAfterOffsetMismatch(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: "k4"})
	rd := spoolWith(t, "abc\r\n")
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: rd, MaxBatch: open.MaxBatchBytes, WindowBytes: open.WindowBytes}
	// The server holds the spool already (an earlier attempt landed but its
	// answer was lost); brig believes offset 0.
	all, _ := os.ReadFile(rd.Name())
	f.Preload("k4", all)
	if err := u.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.Offset != int64(len(all)) {
		t.Errorf("offset %d, want %d", u.Offset, len(all))
	}
	if !bytes.Equal(f.Recording("k4").Raw, all) {
		t.Error("server bytes changed")
	}
}

func TestUploaderFatalStatuses(t *testing.T) {
	for _, st := range []int{404, 413, 422, 409} {
		f := newFake(t)
		c := fakeClient(f)
		open, _ := c.Open(context.Background(), OpenRequest{RunKey: "k5"})
		u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "q"), MaxBatch: open.MaxBatchBytes,
			WindowBytes: open.WindowBytes, Sleep: (&sleeps{}).sleep}
		f.FailNextPuts(st)
		if err := u.Pump(context.Background()); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("status %d: %v", st, err)
		}
	}
}

func TestUploaderStopsWhenContextEnds(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: "k6"})
	ctx, cancel := context.WithCancel(context.Background())
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "q"), MaxBatch: open.MaxBatchBytes,
		WindowBytes: open.WindowBytes, Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}
	f.FailNextPuts(500, 500, 500)
	if err := u.Pump(ctx); err == nil {
		t.Fatal("expected the context error")
	}
}

func TestOpenIsIdempotentAndNotRecordedAnswer(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	a, err := c.Open(context.Background(), OpenRequest{RunKey: "same"})
	b, err2 := c.Open(context.Background(), OpenRequest{RunKey: "same"})
	if err != nil || err2 != nil || a.RecordingID != b.RecordingID {
		t.Fatalf("%+v %+v %v %v", a, b, err, err2)
	}
	f.SetRecorded(false)
	r, err := c.Open(context.Background(), OpenRequest{RunKey: "other"})
	if err != nil || r.Recorded {
		t.Fatalf("%+v %v", r, err)
	}
}

func beginParams(f *recordingtest.Server, dir string) Params {
	return Params{Client: fakeClient(f), URL: f.URL(), Dir: dir,
		Open:  OpenRequest{RunKey: NewRunKey(), Sandbox: "box", Ref: "ubuntu@x", Profile: "ubuntu", Host: "h", Runtime: "hull", BrigVersion: "t", Cols: 100, Rows: 30},
		Sleep: (&sleeps{}).sleep}
}

func TestSessionRecordsARunEndToEndAndRemovesTheSpool(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	outer, slave, err := ptyrelay.OpenPty()
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	defer slave.Close()
	var seen bytes.Buffer
	var mu sync.Mutex
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := outer.Read(b)
			mu.Lock()
			seen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	p := beginParams(f, dir)
	p.In, p.Out = slave, slave
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("begin: %v %v", s, err)
	}
	code, err := s.Run([]string{"sh", "-c", `printf '\033[31mred\033[0m\n'; echo second; exit 4`}, os.Environ())
	if err != nil || code != 4 {
		t.Fatalf("run: %d %v", code, err)
	}
	key := f.Keys()[0]
	raw := f.Recording(key).Raw
	fs := decodeAll(t, raw)
	if fs[0].Type != frame.Resize {
		t.Errorf("first frame %c", fs[0].Type)
	}
	if got := string(outputOf(fs)); got != "\x1b[31mred\x1b[0m\r\nsecond\r\n" {
		t.Errorf("output %q", got)
	}
	last := fs[len(fs)-1]
	if last.Type != frame.Exit || last.Payload[0] != 4 {
		t.Errorf("last frame %c %v", last.Type, last.Payload)
	}
	rec := f.Recording(key)
	if !rec.Closed || rec.End.EndReason != "exit" || rec.End.ExitCode == nil || *rec.End.ExitCode != 4 || rec.End.FinalOffset != int64(len(raw)) {
		t.Errorf("close %+v", rec.End)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(seen.Bytes(), []byte("second")) {
		t.Error("the user's terminal did not get the output")
	}
}

func TestSessionNeverRecordsInput(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	outer, slave, _ := ptyrelay.OpenPty()
	defer outer.Close()
	defer slave.Close()
	var mu sync.Mutex
	var seen bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := outer.Read(b)
			mu.Lock()
			seen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	p := beginParams(f, dir)
	p.In, p.Out = slave, slave
	s, _ := Begin(context.Background(), p)
	go func() {
		for i := 0; i < 400; i++ {
			mu.Lock()
			ok := bytes.Contains(seen.Bytes(), []byte("ready"))
			mu.Unlock()
			if ok {
				_, _ = outer.Write([]byte("hunter2-INPUT-MARKER\n"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	code, err := s.Run([]string{"sh", "-c", `stty -echo; echo ready; read x; echo done`}, os.Environ())
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	key := f.Keys()[0]
	raw := f.Recording(key).Raw
	if bytes.Contains(raw, []byte("INPUT-MARKER")) {
		t.Fatal("typed input reached the recording")
	}
	if !bytes.Contains(outputOf(decodeAll(t, raw)), []byte("done")) {
		t.Error("recording lacks the output")
	}
}

func TestSessionLeavesTheSpoolWhenTheNetworkIsDownAndFlushShipsIt(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	p := beginParams(f, dir)
	p.DrainTimeout = 300 * time.Millisecond
	p.Sleep = nil // real timer, but the context bounds it
	nullIn, _ := os.Open(os.DevNull)
	nullOut, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	p.In, p.Out = nullIn, nullOut
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	for i := 0; i < 200; i++ {
		f.FailNextPuts(503)
	}
	start := time.Now()
	code, err := s.Run([]string{"sh", "-c", "echo later; exit 2"}, os.Environ())
	if err != nil || code != 2 {
		t.Fatalf("%d %v", code, err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("a dead network held the run for %v", el)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 2 {
		t.Fatalf("spool and meta should stay: %v", files)
	}
	metaRaw, _ := os.ReadFile(filepath.Join(dir, p.Open.RunKey+metaExt))
	_ = metaRaw
	f.ClearFailures()
	res, err := Flush(context.Background(), dir, func(string) (string, bool) { return "tok-secret", true },
		func(url, tok string) *Client { return fakeClient(f) })
	if err != nil || res.Closed != 1 {
		t.Fatalf("flush: %+v %v", res, err)
	}
	key := f.Keys()[0]
	fs := decodeAll(t, f.Recording(key).Raw)
	if !bytes.Contains(outputOf(fs), []byte("later")) || fs[len(fs)-1].Type != frame.Exit {
		t.Errorf("flushed recording %q", outputOf(fs))
	}
	if r := f.Recording(key); !r.Closed || r.End.ExitCode == nil || *r.End.ExitCode != 2 || r.End.EndReason != "exit" {
		t.Errorf("close %+v", r.End)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

func TestFlushSkipsALiveRecorderAndAMissingLogin(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	p := beginParams(f, dir)
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	res, _ := Flush(context.Background(), dir, func(string) (string, bool) { return "tok-secret", true },
		func(url, tok string) *Client { return fakeClient(f) })
	if res.Skipped != 1 || res.Closed != 0 {
		t.Errorf("live: %+v", res)
	}
	s.rec.Abort() // the recorder dies without ending
	s.rec.Close()
	res, _ = Flush(context.Background(), dir, func(string) (string, bool) { return "", true },
		func(url, tok string) *Client { return fakeClient(f) })
	if res.Skipped != 1 {
		t.Errorf("no login: %+v", res)
	}
	res, _ = Flush(context.Background(), dir, func(string) (string, bool) { return "tok-secret", true },
		func(url, tok string) *Client { return fakeClient(f) })
	if res.Closed != 1 {
		t.Errorf("flush: %+v", res)
	}
	if r := f.Recording(p.Open.RunKey); r == nil || !r.Closed || r.End.EndReason != "recorder_error" || r.End.ExitCode != nil {
		t.Errorf("close %+v", r)
	}
}

func TestUnknownReasonIsNotAnErrorAndSizesAreAuthoritative(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	f.SetRecorded(false)
	for _, reason := range []string{"setting_off", "not_configured", "something_new"} {
		f.SetReason(reason)
		r, err := c.Open(context.Background(), OpenRequest{RunKey: "r-" + reason})
		if err != nil || r.Recorded {
			t.Errorf("%s: %+v %v", reason, r, err)
		}
	}
	f.SetRecorded(true)
	f.SetMaxBatch(frame.MaxFrame)
	f.SetWindow(4096)
	r, err := c.Open(context.Background(), OpenRequest{RunKey: "sized"})
	if err != nil || r.MaxBatchBytes != frame.MaxFrame || r.WindowBytes != 4096 {
		t.Errorf("server sizes not used: %+v %v", r, err)
	}
	for _, bad := range [][2]int{{0, 262144}, {frame.MaxFrame - 1, 262144}, {1 << 20, 0}, {4<<20 + 1, 262144}, {1 << 20, 1<<20 + 1}, {-1, 10}} {
		f.SetMaxBatch(bad[0])
		f.SetWindow(bad[1])
		if _, err := c.Open(context.Background(), OpenRequest{RunKey: "bad"}); err == nil {
			t.Errorf("sizes %v accepted", bad)
		}
	}
}

func TestAuthRefusalStopsAtOnceAndKeepsTheSpool(t *testing.T) {
	for _, st := range []int{401, 403} {
		f := newFake(t)
		c := fakeClient(f)
		open, _ := c.Open(context.Background(), OpenRequest{RunKey: "a"})
		sl := &sleeps{}
		u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spoolWith(t, "q"), MaxBatch: open.MaxBatchBytes,
			WindowBytes: open.WindowBytes, Sleep: sl.sleep}
		f.FailNextPuts(st)
		err := u.Pump(context.Background())
		if !errors.Is(err, ErrAuth) || len(sl.d) != 0 || len(f.Puts()) != 1 {
			t.Errorf("put %d: err %v sleeps %v puts %d", st, err, sl.d, len(f.Puts()))
		}
		if strings.Contains(err.Error(), "tok-secret") || strings.Contains(err.Error(), "injected") {
			t.Errorf("error leaks: %v", err)
		}
		f.FailNextCloses(st)
		if err := u.Close(context.Background(), CloseRequest{EndReason: "exit"}); !errors.Is(err, ErrAuth) || len(sl.d) != 0 {
			t.Errorf("close %d: %v sleeps %v", st, err, sl.d)
		}
	}
}

func waitingSpool(t *testing.T, f *recordingtest.Server, dir, text string) Params {
	t.Helper()
	p := beginParams(f, dir)
	p.DrainTimeout = 200 * time.Millisecond
	p.Sleep = nil
	in, _ := os.Open(os.DevNull)
	out, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	p.In, p.Out = in, out
	s, err := Begin(context.Background(), p)
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	for i := 0; i < 100; i++ {
		f.FailNextPuts(503)
	}
	if _, err := s.Run([]string{"sh", "-c", "echo " + text}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	f.ClearFailures()
	return p
}

func TestAuthRefusedInTheRunKeepsSpoolAndFlushStopsPerSpoolThenShips(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	waitingSpool(t, f, dir, "one")
	waitingSpool(t, f, dir, "two")
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 4 {
		t.Fatalf("files %v", files)
	}
	tok := func(string) (string, bool) { return "tok-secret", true }
	nc := func(url, tk string) *Client { return fakeClient(f) }
	f.FailNextPuts(401, 403)
	res, err := Flush(context.Background(), dir, tok, nc)
	if err != nil || res.AuthRefused != 2 || res.Closed != 0 {
		t.Fatalf("flush: %+v %v", res, err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 4 {
		t.Errorf("spools were removed: %v", files)
	}
	res, err = Flush(context.Background(), dir, tok, nc)
	if err != nil || res.Closed != 2 {
		t.Fatalf("second flush: %+v %v", res, err)
	}
}

func TestSessionStopsOnAuthRefusalWithoutRetrying(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	p := beginParams(f, dir)
	p.Sleep = nil
	in, _ := os.Open(os.DevNull)
	out, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	p.In, p.Out = in, out
	s, _ := Begin(context.Background(), p)
	f.FailNextPuts(401, 401, 401, 401)
	start := time.Now()
	if _, err := s.Run([]string{"sh", "-c", "echo x"}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("slow: the run retried")
	}
	if n := len(f.Puts()); n > 2 {
		t.Errorf("%d puts after a 401", n)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 2 {
		t.Errorf("files %v", files)
	}
}

func TestCloseSendsNullExitCodeWhenUnknown(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"state":"closed"}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: "t", HTTP: srv.Client()}
	_, _, _ = c.closeRecording(context.Background(), "x", CloseRequest{EndReason: "spool_cap"})
	if !strings.Contains(string(body), `"exit_code":null`) {
		t.Errorf("body %s", body)
	}
	zero := 0
	_, _, _ = c.closeRecording(context.Background(), "x", CloseRequest{EndReason: "exit", ExitCode: &zero})
	if !strings.Contains(string(body), `"exit_code":0`) {
		t.Errorf("body %s", body)
	}
}

// After a 409 offset_mismatch the window must be rebuilt for the offset the
// server holds: the server decodes the batch at M with the last bytes before M
// that it stored, not the bytes this client had sent up to N.
func TestResumeAfterMismatchRebuildsTheWindowForTheServersOffset(t *testing.T) {
	f := newFake(t)
	f.SetWindow(2048)
	c := fakeClient(f)
	open, _ := c.Open(context.Background(), OpenRequest{RunKey: "w"})
	start := time.Now()
	rec, _ := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), start, 0)
	for i := 0; i < 40; i++ {
		rec.Output(start.Add(time.Duration(i)*20*time.Millisecond), []byte(fmt.Sprintf("row %02d: the same words again and again\r\n", i%7)))
	}
	rec.Finish(start.Add(time.Second), 0)
	all, _ := os.ReadFile(rec.Path())
	m, _ := frame.WholePrefix(all[:len(all)/3])
	n, _ := frame.WholePrefix(all[:2*len(all)/3])
	if m == 0 || n <= m {
		t.Fatalf("boundaries %d %d", m, n)
	}
	f.Preload("w", all[:m])
	rd, _ := os.Open(rec.Path())
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: rd, Offset: int64(n), MaxBatch: open.MaxBatchBytes, WindowBytes: open.WindowBytes}
	if err := u.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.Recording("w").Raw; !bytes.Equal(got, all) {
		t.Fatalf("server holds %d bytes, want %d", len(got), len(all))
	}
	puts := f.Puts()
	if len(puts) != 2 || puts[0].Status != 409 || puts[0].Offset != int64(n) || puts[1].Offset != int64(m) || puts[1].Status != 200 {
		t.Errorf("puts %+v", puts)
	}
}

// A Begin that fails after the deployment opened the recording leaves no file
// behind and asks the deployment to close the recording, so no spool and no
// open recording are left for a run that never started.
func TestBeginLeavesNothingWhenItFailsAfterTheOpen(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	p := beginParams(f, dir)
	// A directory where the meta file goes makes writing it fail, after the
	// spool was created.
	blocker := filepath.Join(dir, p.Open.RunKey+metaExt)
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Begin(context.Background(), p)
	if err == nil || s != nil {
		t.Fatalf("Begin = %v, %v; want an error", s, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 1 || left[0] != blocker {
		t.Errorf("files left %v, want only the test's own %s", left, blocker)
	}
	if rec := f.Recording(p.Open.RunKey); rec == nil || !rec.Closed {
		t.Errorf("the recording was not closed at the deployment: %+v", rec)
	}
}
