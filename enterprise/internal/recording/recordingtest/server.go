// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package recordingtest is a fake deployment that implements the recording
// upload routes of the wire contract, for tests. It checks what a real server
// would: a bearer token, contiguous offsets, a batch that decodes with the
// window of what it stored, whole valid frames.
package recordingtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
)

const base = "/api/v2/control-room/recordings"

// StaleAfter is how long a recording may get no request before the server
// closes it as stale.
const StaleAfter = 2 * time.Minute

// Server is the fake deployment.
type Server struct {
	srv *httptest.Server

	mu         sync.Mutex
	token      string
	recorded   bool
	maxBatch   int
	window     int
	reason     string
	recs       map[string]*Rec // by run key
	order      []string        // run keys in the order they were opened
	now        func() time.Time
	byID       map[string]*Rec
	requests   int
	puts       []Put
	failPuts   []int
	failCloses []int
	failOpens  []int
	closeOpens bool
	badAuth    int
}

// Rec is one recording the server holds.
type Rec struct {
	ID     string
	Raw    []byte
	Closed bool
	// Stale is true when the server closed the recording because it got no
	// request for StaleAfter, rather than the recorder closing it.
	Stale bool
	// OpenedAt is the server's clock when the recording was created.
	OpenedAt time.Time
	// LastUpload is when the last append or keepalive was accepted.
	LastUpload time.Time
	End        Close
	Open       map[string]any
}

// Close is what the close call carried.
type Close struct {
	EndReason   string `json:"end_reason"`
	ExitCode    *int   `json:"exit_code"`
	Signal      string `json:"signal"`
	FinalOffset int64  `json:"final_offset"`
}

// Put is one PUT the server saw.
type Put struct {
	Offset   int64
	Status   int
	HostTime string
	// At is the server's clock when the request arrived.
	At time.Time
	// ID is the recording the request was for.
	ID string
	// Keepalive is true for a request with an empty body.
	Keepalive bool
	// Dict is true when the batch's zstd frame names a dictionary.
	Dict bool
}

// New starts a server that accepts token. It records every run until
// SetRecorded(false).
func New(t *testing.T, token string) *Server {
	s := &Server{token: token, recorded: true, maxBatch: 1 << 20, window: 262144, reason: "setting_off", recs: map[string]*Rec{}, byID: map[string]*Rec{}, now: time.Now}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the server's origin. It is a loopback http URL.
func (s *Server) URL() string { return s.srv.URL }

// Client is an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Requests counts every request the server received.
func (s *Server) Requests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

// BadAuth counts requests that lacked the right bearer token or carried a cookie.
func (s *Server) BadAuth() int { s.mu.Lock(); defer s.mu.Unlock(); return s.badAuth }

// SetClock replaces the server's clock, which the 2 minute stale rule reads.
func (s *Server) SetClock(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// SetRecorded sets the answer to POST /recordings.
func (s *Server) SetRecorded(v bool) { s.mu.Lock(); s.recorded = v; s.mu.Unlock() }

// SetMaxBatch sets max_batch_bytes.
func (s *Server) SetMaxBatch(n int) { s.mu.Lock(); s.maxBatch = n; s.mu.Unlock() }

// SetWindow sets window_bytes.
func (s *Server) SetWindow(n int) { s.mu.Lock(); s.window = n; s.mu.Unlock() }

// SetReason sets the reason given with recorded false.
func (s *Server) SetReason(r string) { s.mu.Lock(); s.reason = r; s.mu.Unlock() }

// FailNextPuts makes the next PUTs answer with these statuses, in order.
func (s *Server) FailNextPuts(statuses ...int) {
	s.mu.Lock()
	s.failPuts = append(s.failPuts, statuses...)
	s.mu.Unlock()
}

// SetCloseOnOpen makes every recording created from now on look as if it had
// been quiet for longer than StaleAfter, so the server closes it at its first
// request, as a server that closes every recording at once would.
func (s *Server) SetCloseOnOpen(v bool) { s.mu.Lock(); s.closeOpens = v; s.mu.Unlock() }

// FailNextOpens makes the next POST /recordings calls answer with these
// statuses, in order. Status 0 drops the connection, as a transport error.
func (s *Server) FailNextOpens(statuses ...int) {
	s.mu.Lock()
	s.failOpens = append(s.failOpens, statuses...)
	s.mu.Unlock()
}

// FailNextCloses makes the next close calls answer with these statuses.
func (s *Server) FailNextCloses(statuses ...int) {
	s.mu.Lock()
	s.failCloses = append(s.failCloses, statuses...)
	s.mu.Unlock()
}

// ClearFailures drops the queued failures.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	s.failPuts, s.failCloses, s.failOpens = nil, nil, nil
	s.mu.Unlock()
}

// Puts returns the PUTs seen.
func (s *Server) Puts() []Put { s.mu.Lock(); defer s.mu.Unlock(); return append([]Put(nil), s.puts...) }

// Keys returns the run keys the server holds recordings for, in the order they
// were opened.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// Recording returns a copy of the recording for a run key, or nil.
func (s *Server) Recording(key string) *Rec {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[key]
	if r == nil {
		return nil
	}
	c := *r
	c.Raw = append([]byte(nil), r.Raw...)
	return &c
}

// Preload makes the server already hold raw bytes for the run key's recording,
// as after an upload whose answer was lost.
func (s *Server) Preload(key string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.recs[key]; r != nil {
		r.Raw = append([]byte(nil), raw...)
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// touchStale applies the stale rule to rec: an open recording that got nothing
// for StaleAfter is closed. The caller holds mu.
func (s *Server) touchStale(rec *Rec) {
	if rec != nil && !rec.Closed && s.now().Sub(rec.LastUpload) > StaleAfter {
		rec.Closed, rec.Stale = true, true
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if r.Header.Get("Authorization") != "Bearer "+s.token || len(r.Cookies()) != 0 {
		s.badAuth++
		reply(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == base:
		if len(s.failOpens) > 0 {
			st := s.failOpens[0]
			s.failOpens = s.failOpens[1:]
			if st == 0 {
				if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = c.Close()
				}
				return
			}
			reply(w, st, map[string]string{"error": "injected"})
			return
		}
		var req map[string]any
		key := ""
		if json.NewDecoder(r.Body).Decode(&req) == nil {
			key, _ = req["run_key"].(string)
		}
		if key == "" {
			reply(w, 400, map[string]string{"error": "bad"})
			return
		}
		if !s.recorded {
			reply(w, 200, map[string]any{"recorded": false, "reason": s.reason})
			return
		}
		rec := s.recs[key]
		s.touchStale(rec)
		if rec != nil && rec.Closed {
			reply(w, 200, map[string]any{"recorded": false, "reason": "closed"})
			return
		}
		if rec == nil {
			rec = &Rec{ID: "rec-" + strconv.Itoa(len(s.recs)+1), Open: req, LastUpload: s.now(), OpenedAt: s.now()}
			if s.closeOpens {
				rec.LastUpload = s.now().Add(-2 * StaleAfter)
			}
			s.recs[key] = rec
			s.byID[rec.ID] = rec
			s.order = append(s.order, key)
		}
		reply(w, 200, map[string]any{"recorded": true, "recording_id": rec.ID, "next_offset": len(rec.Raw),
			"max_batch_bytes": s.maxBatch, "window_bytes": s.window, "format": "nofire.recording/v1"})
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/data"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/"), "/data")
		rec := s.byID[id]
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		call := Put{Offset: off, HostTime: r.Header.Get("X-Recording-Host-Time"), At: s.now(), ID: id}
		defer func() { s.puts = append(s.puts, call) }()
		if len(s.failPuts) > 0 {
			call.Status = s.failPuts[0]
			s.failPuts = s.failPuts[1:]
			reply(w, call.Status, map[string]string{"error": "injected"})
			return
		}
		if rec == nil {
			call.Status = 404
			reply(w, 404, map[string]string{"error": "not_found"})
			return
		}
		s.touchStale(rec)
		if rec.Closed {
			call.Status = 409
			reply(w, 409, map[string]string{"error": "recording_closed"})
			return
		}
		if off != int64(len(rec.Raw)) {
			call.Status = 409
			reply(w, 409, map[string]any{"error": "offset_mismatch", "next_offset": len(rec.Raw)})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			// A keepalive: nothing is stored, the quiet clock restarts.
			rec.LastUpload = s.now()
			call.Keepalive, call.Status = true, 200
			reply(w, 200, map[string]any{"next_offset": len(rec.Raw)})
			return
		}
		call.Dict = len(body) > 4 && body[4]&3 != 0
		win := rec.Raw
		if len(win) > s.window {
			win = win[len(win)-s.window:]
		}
		dec, err := frame.DecodeBatch(body, win, s.maxBatch)
		if n, ferr := frame.WholePrefix(dec); err != nil || ferr != nil || n != len(dec) || len(dec) == 0 {
			call.Status = 422
			reply(w, 422, map[string]string{"error": "invalid_batch"})
			return
		}
		rec.Raw = append(rec.Raw, dec...)
		rec.LastUpload = s.now()
		call.Status = 200
		reply(w, 200, map[string]any{"next_offset": len(rec.Raw)})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/close"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/"), "/close")
		rec := s.byID[id]
		if len(s.failCloses) > 0 {
			st := s.failCloses[0]
			s.failCloses = s.failCloses[1:]
			reply(w, st, map[string]string{"error": "injected"})
			return
		}
		var req Close
		_ = json.NewDecoder(r.Body).Decode(&req)
		if rec == nil {
			reply(w, 404, map[string]string{"error": "not_found"})
			return
		}
		s.touchStale(rec)
		if rec.Closed && rec.Stale {
			reply(w, 409, map[string]string{"error": "recording_closed"})
			return
		}
		if req.FinalOffset != int64(len(rec.Raw)) {
			reply(w, 409, map[string]any{"error": "offset_mismatch", "next_offset": len(rec.Raw)})
			return
		}
		rec.Closed, rec.End = true, req
		reply(w, 200, map[string]string{"state": "closed"})
	default:
		reply(w, 404, map[string]string{"error": "no route"})
	}
}
