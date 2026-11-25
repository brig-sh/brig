// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// hostile is a deployment that answers what its test says and records every
// request path it was sent, so a test can assert that a bad answer did not
// steer the next request anywhere.
type hostile struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	open  map[string]any
	put   map[string]any
	putSt int
}

func newHostile(t *testing.T) *hostile {
	t.Helper()
	h := &hostile{putSt: 200}
	h.open = map[string]any{"recorded": true, "recording_id": "rec-1", "next_offset": 0,
		"max_batch_bytes": 1 << 20, "window_bytes": 4096, "format": Format}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.EscapedPath())
		reply, st := h.open, 200
		if r.Method == http.MethodPut {
			reply, st = h.put, h.putSt
		}
		h.mu.Unlock()
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hostile) client() *Client {
	return &Client{Base: h.srv.URL, Token: "tok-secret", HTTP: h.srv.Client()}
}

func (h *hostile) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// A recording id goes into a URL path that carries the token, so an id that is
// more than one plain segment is refused at the open, and no request follows.
func TestOpenRefusesARecordingIDThatIsNotOnePlainSegment(t *testing.T) {
	long := strings.Repeat("a", maxIDLen+1)
	for _, id := range []string{"", "..", ".", "a/b", "../x", "a/../b", "a?b=1", "a#b", "a b", "a%2Fb",
		"a\nb", "a\x00b", "é", `a\b`, long} {
		h := newHostile(t)
		h.open["recording_id"] = id
		c := h.client()
		if _, err := c.Open(context.Background(), OpenRequest{RunKey: "k"}); err == nil {
			t.Errorf("recording id %q was accepted", id)
		}
		if got := h.requests(); len(got) != 1 {
			t.Errorf("recording id %q: %d requests, want only the open: %v", id, len(got), got)
		}
	}
	for _, id := range []string{"rec-1", "0b9f2c1e-6a1d-4c57-9d0e-1f0a9c3b7d21", "A_b-9", strings.Repeat("a", maxIDLen)} {
		h := newHostile(t)
		h.open["recording_id"] = id
		if r, err := h.client().Open(context.Background(), OpenRequest{RunKey: "k"}); err != nil || r.RecordingID != id {
			t.Errorf("recording id %q: %+v, %v", id, r, err)
		}
	}
}

// Defence beside the check at the open: whatever id reaches a request is
// escaped as one path segment.
func TestARecordingIDIsEscapedIntoOnePathSegment(t *testing.T) {
	h := newHostile(t)
	h.put = map[string]any{"next_offset": 0}
	c := h.client()
	if _, _, err := c.put(context.Background(), "a/../b?x", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.closeRecording(context.Background(), "a/../b?x", CloseRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT " + apiPrefix + "/a%2F..%2Fb%3Fx/data",
		"POST " + apiPrefix + "/a%2F..%2Fb%3Fx/close",
	}
	if got := h.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests %q, want %q", got, want)
	}
}

// The deployment's sizes bound what brig allocates: a batch or a window past
// the ceiling, zero, or negative is refused at the open.
func TestOpenRefusesHostileSizes(t *testing.T) {
	for _, sz := range [][2]int{
		{1 << 40, 4096}, {1<<63 - 1, 4096}, {-1 << 40, 4096}, {0, 4096},
		{1 << 20, 1 << 40}, {1 << 20, 1<<63 - 1}, {1 << 20, -5}, {1 << 20, 0},
		{maxBatchCeil + 1, 4096}, {1 << 20, maxWindow + 1},
	} {
		h := newHostile(t)
		h.open["max_batch_bytes"], h.open["window_bytes"] = sz[0], sz[1]
		if _, err := h.client().Open(context.Background(), OpenRequest{RunKey: "k"}); err == nil {
			t.Errorf("batch %d, window %d were accepted", sz[0], sz[1])
		}
	}
}

// A next_offset that is negative is refused at the open. One past what the
// spool holds is refused before anything is sent: believing it would mark the
// whole run as already uploaded and delete the spool.
func TestAHostileNextOffsetIsNotBelieved(t *testing.T) {
	h := newHostile(t)
	h.open["next_offset"] = -1
	if _, err := h.client().Open(context.Background(), OpenRequest{RunKey: "k"}); err == nil {
		t.Error("a negative next_offset was accepted at the open")
	}

	for _, off := range []int64{1 << 62, 1<<63 - 1, 1 << 20} {
		h := newHostile(t)
		h.put = map[string]any{"next_offset": 0}
		u := &Uploader{C: h.client(), RecordingID: "rec-1", Spool: spoolWith(t, "hello\r\n"), Offset: off,
			MaxBatch: 1 << 20, WindowBytes: 4096}
		if err := u.Pump(context.Background()); !errors.Is(err, ErrFatal) {
			t.Errorf("offset %d: Pump = %v, want ErrFatal", off, err)
		}
		if err := u.Close(context.Background(), CloseRequest{EndReason: "exit"}); !errors.Is(err, ErrFatal) {
			t.Errorf("offset %d: Close = %v, want ErrFatal", off, err)
		}
		if got := h.requests(); len(got) != 0 {
			t.Errorf("offset %d: requests were sent: %v", off, got)
		}
	}
}

// An answer to a batch that claims more than the batch brought, or an offset
// outside the spool in a 409, is refused, and the uploader's offset is not
// moved to it.
func TestAHostileAnswerToABatchDoesNotMoveTheOffset(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  map[string]any
	}{
		{"huge 200", 200, map[string]any{"next_offset": int64(1 << 62)}},
		{"past the batch 200", 200, map[string]any{"next_offset": 1 << 20}},
		{"huge mismatch", 409, map[string]any{"error": "offset_mismatch", "next_offset": int64(1 << 62)}},
		{"negative mismatch", 409, map[string]any{"error": "offset_mismatch", "next_offset": -7}},
	}
	for _, tc := range cases {
		h := newHostile(t)
		h.put, h.putSt = tc.reply, tc.status
		u := &Uploader{C: h.client(), RecordingID: "rec-1", Spool: spoolWith(t, "hello\r\n"),
			MaxBatch: 1 << 20, WindowBytes: 4096}
		if err := u.Pump(context.Background()); !errors.Is(err, ErrFatal) {
			t.Errorf("%s: Pump = %v, want ErrFatal", tc.name, err)
		}
		if u.Offset != 0 {
			t.Errorf("%s: the offset moved to %d", tc.name, u.Offset)
		}
	}
}

// A batch size smaller than a frame cannot be satisfied. The upload stops
// with an error rather than reporting that it caught up.
func TestABatchSizeSmallerThanAFrameIsFatal(t *testing.T) {
	h := newHostile(t)
	h.put = map[string]any{"next_offset": 1}
	u := &Uploader{C: h.client(), RecordingID: "rec-1", Spool: spoolWith(t, "hello\r\n"), MaxBatch: 1, WindowBytes: 4096}
	if err := u.Pump(context.Background()); !errors.Is(err, ErrFatal) {
		t.Errorf("Pump = %v, want ErrFatal", err)
	}
	if got := h.requests(); len(got) != 0 {
		t.Errorf("requests were sent: %v", got)
	}
}

// The spool directory is 0700, and the spool and its meta file are 0600, also
// when the directory was there already with wider modes.
func TestTheSpoolIsPrivate(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f := newFake(t)
		dir := filepath.Join(t.TempDir(), "recordings")
		if existing {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		old := mask(0)
		s, err := Begin(context.Background(), beginParams(f, dir))
		mask(old)
		if err != nil || s == nil {
			t.Fatalf("begin: %v %v", s, err)
		}
		s.rec.Abort()
		check := func(path string, want os.FileMode) {
			t.Helper()
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != want {
				t.Errorf("existing=%v: %s is %v, want %v", existing, filepath.Base(path), got, want)
			}
		}
		check(dir, 0o700)
		check(s.paths[0], 0o600)
		check(s.paths[1], 0o600)
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 {
			t.Errorf("existing=%v: the directory holds %d entries, want the spool and its meta file", existing, len(entries))
		}
	}
}
