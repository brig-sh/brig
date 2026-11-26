// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A 200 whose body is empty or is not JSON is a proxy's or a captive portal's
// answer, not the deployment's. It is retried after a wait, like a transport
// error, and never read as "the batch was not taken" (fatal, spool deleted) or
// as a confirmed close.
func TestA200ThatIsNotTheDeploymentsReplyIsRetried(t *testing.T) {
	for _, body := range []string{"", "<html>sign in</html>", "ok"} {
		var mu sync.Mutex
		var puts, closes int
		var next int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/close") {
				closes++
				if closes == 1 {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(`{"state":"closed"}`))
				return
			}
			puts++
			if puts <= 2 {
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = w.Write([]byte(`{"next_offset":` + strconv.FormatInt(next, 10) + `}`))
		}))
		spool := spoolWith(t, "hello\r\n")
		n, _ := (&Uploader{Spool: spool}).size()
		next = n
		sl := &limitedSleeps{n: 1000}
		u := &Uploader{C: &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}, RecordingID: "rec-1",
			Spool: spool, MaxBatch: 1 << 20, WindowBytes: 4096, Sleep: sl.sleep}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := u.Pump(ctx); err != nil {
			t.Errorf("body %q: Pump = %v, want nil after the retries", body, err)
		}
		if u.Offset != n || len(sl.d) != 2 {
			t.Errorf("body %q: offset %d (want %d), waits %v (want 2)", body, u.Offset, n, sl.d)
		}
		if err := u.Close(ctx, CloseRequest{EndReason: "exit"}); err != nil {
			t.Errorf("body %q: Close = %v", body, err)
		}
		mu.Lock()
		if closes != 2 || len(sl.d) != 3 {
			t.Errorf("body %q: %d close calls and waits %v; want a retry after a wait, not a confirmed close", body, closes, sl.d)
		}
		mu.Unlock()
		cancel()
		srv.Close()
	}
}

// A close that never gets a reply the deployment sent ends with an error that
// keeps the spool.
func TestACloseThatIsNeverAnsweredByTheDeploymentKeepsTheSpool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()
	sl := &limitedSleeps{n: 3}
	u := &Uploader{C: &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}, RecordingID: "rec-1",
		Spool: spoolWith(t, ""), MaxBatch: 1 << 20, WindowBytes: 4096, Sleep: sl.sleep}
	err := u.Close(context.Background(), CloseRequest{EndReason: "exit"})
	if err == nil || errors.Is(err, ErrFatal) {
		t.Errorf("Close = %v, want an error that keeps the spool", err)
	}
}
