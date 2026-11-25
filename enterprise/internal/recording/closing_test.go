// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
)

// limitedSleeps records every wait and ends the call after n of them, as a
// context that ran out would.
type limitedSleeps struct {
	mu sync.Mutex
	d  []time.Duration
	n  int
}

func (s *limitedSleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	if len(s.d) >= s.n {
		return context.Canceled
	}
	return nil
}

// A deployment that answers every batch with 409 offset_mismatch naming the
// offset the batch was sent at is not moving. The uploader backs off like it
// does for a transport error and keeps the spool, and a later call backs off at
// its first answer, so it does not send the same batch in a tight loop.
func TestAServerThatNeverLetsTheUploadMoveIsNotAskedInATightLoop(t *testing.T) {
	h := newHostile(t)
	h.put, h.putSt = map[string]any{"error": "offset_mismatch", "next_offset": 0}, 409
	sl := &limitedSleeps{n: 4}
	u := &Uploader{C: h.client(), RecordingID: "rec-1", Spool: spoolWith(t, "hello\r\n"),
		MaxBatch: 1 << 20, WindowBytes: 4096, Sleep: sl.sleep}
	// The deadline only matters to an uploader that never waits.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := u.Pump(ctx)
	if err == nil || errors.Is(err, ErrFatal) {
		t.Fatalf("Pump = %v, want an error that keeps the spool", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(sl.d) != len(want) {
		t.Fatalf("waits %v, want %v", sl.d, want)
	}
	for i := range want {
		if sl.d[i] != want[i] {
			t.Errorf("waits %v, want %v", sl.d, want)
			break
		}
	}
	// One request per wait, and the one that ended the call.
	if n := len(h.requests()); n > len(want)+1 {
		t.Errorf("%d requests for 4 waits: %v", n, h.requests())
	}

	// The next call, as the live loop makes it a tick later, backs off at its
	// first answer instead of sending again.
	before := len(h.requests())
	sl2 := &limitedSleeps{n: 1}
	u.Sleep = sl2.sleep
	if err := u.Pump(context.Background()); err == nil || errors.Is(err, ErrFatal) {
		t.Fatalf("second Pump = %v", err)
	}
	if got := len(h.requests()) - before; got != 1 || len(sl2.d) != 1 {
		t.Errorf("second call sent %d requests and waited %d times; want 1 and 1", got, len(sl2.d))
	}
	if u.Offset != 0 {
		t.Errorf("offset %d", u.Offset)
	}
}

// An honest mismatch, one that names a different offset, is adopted and the
// upload goes on from it without any wait.
func TestOneHonestMismatchIsAdoptedWithoutAWait(t *testing.T) {
	f := newFake(t)
	c := fakeClient(f)
	open, err := c.Open(context.Background(), OpenRequest{RunKey: "k", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	spool := spoolWith(t, "hello\r\n")
	size, _ := (&Uploader{Spool: spool}).size()
	sl := &limitedSleeps{n: 1}
	u := &Uploader{C: c, RecordingID: open.RecordingID, Spool: spool, MaxBatch: open.MaxBatchBytes,
		WindowBytes: open.WindowBytes, Sleep: sl.sleep}
	// The deployment holds the first frame already; the uploader thinks it holds none.
	raw := mustRead(t, spool)
	_, first, err := frame.Next(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.Preload("k", raw[:first])
	if err := u.Pump(context.Background()); err != nil {
		t.Fatalf("Pump = %v", err)
	}
	if u.Offset != size || len(sl.d) != 0 {
		t.Errorf("offset %d of %d, waits %v", u.Offset, size, sl.d)
	}
}

func mustRead(t *testing.T, f *os.File) []byte {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A run whose recording the deployment refused for good tells the person when
// it ends that the recording stopped and the rest was not sent.
func TestARunWhoseRecordingWasRefusedSaysSo(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	for i := 0; i < 6; i++ {
		r.f.FailNextPuts(422)
	}
	r.end()
	if got := r.notice.String(); got != fatalNotice+"\n" {
		t.Errorf("closing lines %q", got)
	}
	if files := r.dirFiles(); len(files) != 0 {
		t.Errorf("files left %v", files)
	}
}

// A login the deployment refuses at the end of the run has its own line, which
// says to sign in again, and not the line that promises the next run will send.
func TestARunWhoseLoginWasRefusedTellsThePersonToSignInAgain(t *testing.T) {
	r := newRun(t)
	r.say("one\r\n")
	r.mustTick()
	r.say("two\r\n")
	for i := 0; i < 6; i++ {
		r.f.FailNextPuts(401)
	}
	r.end()
	got := r.notice.String()
	if got != authRefusedNotice+"\n" || !strings.Contains(got, "brig login") || strings.Contains(got, "next recorded run") {
		t.Errorf("closing lines %q", got)
	}
	if files := r.dirFiles(); len(files) != 2 {
		t.Errorf("the spool must stay: %v", files)
	}
}

// A flush counts spools it removed without sending them apart from the ones it
// sent, and so does the run-end line about earlier recordings.
func TestFlushCountsSpoolsRemovedWithoutBeingSentApartFromSentOnes(t *testing.T) {
	r, _ := leftoverRun(t)
	if err := os.Remove(strings.TrimSuffix(r.s.paths[1], metaExt) + spoolExt); err != nil {
		t.Fatal(err)
	}
	old := r.clk.Now().Add(-time.Hour)
	_ = os.Chtimes(r.s.paths[1], old, old)
	res := r.flush()
	if res.Dropped != 1 || res.Closed != 0 {
		t.Errorf("flush %+v, want 1 removed without being sent and 0 sent", res)
	}
	if files := r.dirFiles(); len(files) != 0 {
		t.Errorf("files left %v", files)
	}

	s := &Session{earlier: 2, earlierDropped: 1}
	want := []string{"brig: sent the rest of 2 earlier recordings", "brig: removed 1 earlier recording without sending it"}
	if got := s.Notices(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("notices %q, want %q", got, want)
	}
	s = &Session{earlierExpired: 1}
	if got := s.Notices(); len(got) != 1 || got[0] != "brig: removed 1 earlier recording that was more than 30 days old and could not be sent" {
		t.Errorf("notices %q", got)
	}
	s = &Session{earlierExpired: 2}
	if got := s.Notices(); len(got) != 1 || got[0] != "brig: removed 2 earlier recordings that were more than 30 days old and could not be sent" {
		t.Errorf("notices %q", got)
	}
	s = &Session{earlierDropped: 3}
	if got := s.Notices(); len(got) != 1 || got[0] != "brig: removed 3 earlier recordings without sending them" {
		t.Errorf("notices %q", got)
	}
}

// Every place that removes a recording's pair removes the meta file first and
// the spool next, so a flush that finds a meta file with no spool is not looking
// at a run that is tidying up.
func TestEveryRemovalOfAPairTakesTheMetaFileBeforeTheSpool(t *testing.T) {
	record := func(t *testing.T) *[]string {
		var calls []string
		old := removeFile
		removeFile = func(p string) error { calls = append(calls, filepath.Ext(p)); return old(p) }
		t.Cleanup(func() { removeFile = old })
		return &calls
	}
	want := []string{metaExt, spoolExt}
	check := func(t *testing.T, name string, calls *[]string) {
		t.Helper()
		if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
			t.Errorf("%s removed %v, want the meta file and then the spool", name, *calls)
		}
	}

	t.Run("a run that ends cleanly", func(t *testing.T) {
		r := newRun(t)
		r.say("one\r\n")
		r.mustTick()
		calls := record(t)
		r.end()
		check(t, "finish", calls)
	})
	t.Run("a run that stops being recorded", func(t *testing.T) {
		r := newRun(t)
		calls := record(t)
		r.s.stop("the deployment said not to record")
		check(t, "stop", calls)
	})
	t.Run("a flush that finishes a leftover", func(t *testing.T) {
		r, _ := leftoverRun(t)
		calls := record(t)
		if res := r.flush(); res.Closed != 1 {
			t.Fatalf("flush %+v", res)
		}
		check(t, "flush", calls)
	})
	t.Run("a Begin that fails", func(t *testing.T) {
		f := newFake(t)
		dir := t.TempDir()
		p := beginParams(f, dir)
		// Fail after the meta file was written: the spool cannot be opened for
		// the uploader.
		calls := record(t)
		old := openForUpload
		openForUpload = func(string) (*os.File, error) { return nil, errors.New("injected") }
		t.Cleanup(func() { openForUpload = old })
		if s, err := Begin(context.Background(), p); err == nil || s != nil {
			t.Fatalf("Begin = %v, %v", s, err)
		}
		check(t, "Begin", calls)
	})
}

// A meta file whose spool is gone was not removed by its own run, which takes
// the meta file first. It is removed and counted as removed without being sent.
// A pair that its own run removed between the listing and the open is not
// counted.
func TestAMetaFileWithNoSpoolIsCountedOnceAndAGonePairIsNot(t *testing.T) {
	r, _ := leftoverRun(t)
	if out, err := flushOne(context.Background(), strings.TrimSuffix(r.s.paths[1], metaExt)+"-gone", tokenOK, nil); out != outSkipped || err != nil {
		t.Errorf("a pair that is gone: %v %v", out, err)
	}
	if err := os.Remove(strings.TrimSuffix(r.s.paths[1], metaExt) + spoolExt); err != nil {
		t.Fatal(err)
	}
	if res := r.flush(); res.Dropped != 1 || res.Skipped != 0 || len(r.dirFiles()) != 0 {
		t.Errorf("meta file with no spool: %+v files %v", res, r.dirFiles())
	}
}

// A mismatch that names an offset other than the uploader's own is an ordinary
// resync: it is adopted and the upload goes on at once, with no wait. That holds
// again after the uploader was pointed at a new recording, however often the
// deployment named the same offset before.
func TestAMismatchNamingAnotherOffsetIsAdoptedAtOnceAlsoAfterASwitch(t *testing.T) {
	h := newHostile(t)
	h.put, h.putSt = map[string]any{"error": "offset_mismatch", "next_offset": 3}, 409
	sl := &limitedSleeps{n: 1}
	u := &Uploader{C: h.client(), RecordingID: "rec-1", Spool: spoolWith(t, "hello\r\n"),
		MaxBatch: 1 << 20, WindowBytes: 4096, Sleep: sl.sleep}
	batch := mustRead(t, u.Spool)
	for round := 0; round < 2; round++ {
		if err := u.send(context.Background(), batch); err != nil {
			t.Fatalf("round %d: send = %v", round, err)
		}
		if u.Offset != 3 || len(sl.d) != 0 {
			t.Fatalf("round %d: offset %d, waits %v; want the offset adopted with no wait", round, u.Offset, sl.d)
		}
		// The recording was closed and the run continues in a new one.
		u.Switch("rec-2", 0, nil, 1<<20, 4096)
	}
}

// Two replicas that hold different offsets answer every batch with a mismatch
// that names the other's offset. The first two answers are sent from at once;
// from the third on the uploader backs off, whatever offset is named, so it does
// not send at round-trip speed for the whole run. Driven through Pump, every
// batch is read again at the offset it is sent at: each body, decoded with the
// window of that offset, is exactly the spool's bytes there. When the deployment
// then accepts, the recording equals the spool byte for byte.
func TestADeploymentThatAlternatesBetweenTwoOffsetsIsBackedOffFrom(t *testing.T) {
	start := time.Now()
	rec, err := OpenSpool(filepath.Join(t.TempDir(), "a.spool"), start, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		rec.Output(start.Add(time.Duration(i)*40*time.Millisecond), []byte(fmt.Sprintf("distinct line %02d of output\r\n", i)))
		rec.Output(start.Add(time.Duration(i)*40*time.Millisecond+20*time.Millisecond), nil)
	}
	rec.Finish(start.Add(time.Second), 0)
	spool, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	// Frame boundaries the two replicas hold.
	var bounds []int64
	for pos := 0; pos < len(spool); {
		_, n, ferr := frame.Next(spool[pos:])
		if ferr != nil {
			t.Fatal(ferr)
		}
		pos += n
		bounds = append(bounds, int64(pos))
	}
	b1, b2 := bounds[1], bounds[3]

	const window = 4096
	var (
		mu       sync.Mutex
		puts     int
		accept   bool
		raw      = append([]byte(nil), spool[:b2]...)
		badBody  []string
		accepted bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		puts++
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		body, _ := io.ReadAll(r.Body)
		if off < 0 || off > int64(len(spool)) {
			badBody = append(badBody, fmt.Sprintf("offset %d outside the spool", off))
		} else {
			win := spool[max(0, off-window):off]
			dec, derr := frame.DecodeBatch(body, win, 1<<20)
			if derr != nil || len(dec) == 0 || !bytes.Equal(dec, spool[off:off+int64(len(dec))]) {
				badBody = append(badBody, fmt.Sprintf("body at offset %d is not the spool's bytes there (%v)", off, derr))
			} else if accept && off == int64(len(raw)) {
				raw = append(raw, dec...)
				accepted = true
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"next_offset": len(raw)})
				return
			}
		}
		next := b2
		if accept {
			next = int64(len(raw))
		} else if puts%2 == 0 {
			next = b1
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "offset_mismatch", "next_offset": next})
	}))
	t.Cleanup(srv.Close)

	rd, err := os.Open(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var waits []time.Duration
	u := &Uploader{C: &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}, RecordingID: "rec-1",
		Spool: rd, MaxBatch: 1 << 20, WindowBytes: window}
	u.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		if len(waits) == 5 {
			accept = true // the replicas agree from here on
		}
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	perr := u.Pump(ctx)
	mu.Lock()
	defer mu.Unlock()
	for _, b := range badBody {
		t.Error(b)
	}
	if perr != nil {
		t.Fatalf("Pump = %v", perr)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("waits %v, want %v", waits, want)
			break
		}
	}
	// Two requests before the first wait, then one per wait, then the batches
	// that finish the spool once the replicas agree.
	if puts > 2+len(want)+4 {
		t.Errorf("%d requests across %v of waiting", puts, waits)
	}
	if !accepted || !bytes.Equal(raw, spool) {
		t.Errorf("the recording holds %d bytes and equals the spool: %v; the spool is %d", len(raw), bytes.Equal(raw, spool), len(spool))
	}
}
