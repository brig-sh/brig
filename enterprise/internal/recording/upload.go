// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
)

// Backoff and wait values from the contract.
const (
	rateLimitWait = 2 * time.Second
	backoffMin    = time.Second
	backoffMax    = 30 * time.Second
	// KeepaliveEvery is how long the uploader waits after its last good
	// request before it sends an empty one. The server closes a recording that
	// is quiet for 2 minutes, so 45 s leaves room for one lost keepalive.
	KeepaliveEvery = 45 * time.Second
)

// ErrFatal means the server refused this recording for good: the upload stops
// and a retry cannot help.
var ErrFatal = errors.New("recording: the deployment refused the recording")

// ErrRecordingClosed means the server closed the recording and this uploader
// did not ask it to: it got no request for 2 minutes. The caller may open a new
// recording and continue with Switch.
var ErrRecordingClosed = errors.New("recording: the deployment closed the recording")

// Uploader sends a spool to an open recording in order. It reads the spool
// through its own descriptor, so it never touches the recorder's.
//
// The recording's raw stream is Prefix followed by the spool from byte Base on.
// For the first recording of a run Prefix is empty and Base is 0, so a recording
// offset is a spool position. For a recording that continues an earlier one,
// Prefix is the resize frame written ahead of the frames the earlier recording
// did not take, and Base is the spool position of the first of those frames.
// Offset, the server's answers and the dictionary window all count in recording
// offsets. SpoolPos converts one to a spool position.
type Uploader struct {
	C           *Client
	RecordingID string
	Spool       *os.File
	// Offset is the raw length the server holds.
	Offset      int64
	MaxBatch    int
	WindowBytes int
	// Base is the spool position where this recording's copy of the spool
	// starts. Prefix is the bytes of this recording that come before it.
	Base   int64
	Prefix []byte
	// Now is the clock the keepalive reads; nil is time.Now. KeepaliveEvery
	// is the quiet time before a keepalive; zero means the package constant.
	Now            func() time.Time
	KeepaliveEvery time.Duration
	lastOK         time.Time
	// mismatches counts the 409 offset_mismatch answers since a batch was
	// last accepted or the uploader was pointed at a new recording.
	mismatches int
	// mismatchWait is the delay before the next resend once mismatches say the
	// deployment is not moving. It doubles from backoffMin to backoffMax and is
	// reset wherever mismatches is.
	mismatchWait time.Duration
	// Progress, when set, is called after the server accepted a batch and
	// Offset moved.
	Progress func()
	// Sleep waits d or until ctx ends; nil is a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (u *Uploader) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// SpoolPos is the spool position that recording offset off stands for. An
// offset inside Prefix stands for Base.
func (u *Uploader) SpoolPos(off int64) int64 {
	return u.Base + max(0, off-int64(len(u.Prefix)))
}

// Switch points the uploader at a new recording that starts at offset 0 with
// the given prefix and spool base.
func (u *Uploader) Switch(id string, base int64, prefix []byte, maxBatch, window int) {
	u.RecordingID, u.Base, u.Prefix, u.Offset = id, base, prefix, 0
	u.mismatches, u.mismatchWait = 0, 0
	u.MaxBatch, u.WindowBytes = maxBatch, window
	u.lastOK = u.now()
}

// readAt fills p with the recording's raw bytes from offset off.
func (u *Uploader) readAt(p []byte, off int64) error {
	pl := int64(len(u.Prefix))
	for len(p) > 0 {
		if off < pl {
			n := copy(p, u.Prefix[off:])
			p, off = p[n:], off+int64(n)
			continue
		}
		n, err := u.Spool.ReadAt(p, u.Base+off-pl)
		if n == len(p) {
			return nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func (u *Uploader) sleep(ctx context.Context, d time.Duration) error {
	if u.Sleep != nil {
		return u.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pump sends every whole frame the spool holds past Offset, in batches. It
// returns nil when it has caught up with the spool, ctx's error when ctx ends
// first, and ErrFatal when the server refuses. Network errors and 5xx answers
// are retried with a back-off of 1 s doubling to 30 s until ctx ends.
func (u *Uploader) Pump(ctx context.Context) error {
	for {
		size, err := u.size()
		if err != nil {
			return err
		}
		if u.Offset > size {
			// The deployment said it holds more than the run has written.
			// Taking that at its word would drop the rest of the run as
			// "already sent" and delete the spool.
			return fmt.Errorf("%w: the deployment holds more than the spool", ErrFatal)
		}
		if u.Offset == size {
			return nil
		}
		n := min(size-u.Offset, int64(u.MaxBatch))
		buf := make([]byte, n)
		if err := u.readAt(buf, u.Offset); err != nil {
			return err
		}
		whole, ferr := frame.WholePrefix(buf)
		if ferr != nil {
			return fmt.Errorf("%w: the spool holds an invalid frame: %v", ErrFatal, ferr)
		}
		if whole == 0 {
			if size-u.Offset > int64(u.MaxBatch) {
				// A full batch holds no whole frame, so none ever will:
				// the deployment's batch size is smaller than a frame.
				return fmt.Errorf("%w: a frame is larger than the deployment's batch size", ErrFatal)
			}
			return nil // a frame still being written
		}
		batch := buf[:whole]
		if err := u.send(ctx, batch); err != nil {
			return err
		}
	}
}

// window reads the dictionary window for a batch at offset.
func (u *Uploader) window(offset int64) ([]byte, error) {
	if offset == 0 {
		return nil, nil
	}
	n := min(offset, int64(u.WindowBytes))
	w := make([]byte, n)
	if err := u.readAt(w, offset-n); err != nil {
		return nil, err
	}
	return w, nil
}

type offsetReply struct {
	NextOffset int64  `json:"next_offset"`
	Error      string `json:"error"`
}

// send delivers one batch, retrying as the contract says. On return Offset is
// the server's position.
func (u *Uploader) send(ctx context.Context, batch []byte) error {
	backoff := backoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		win, err := u.window(u.Offset)
		if err != nil {
			return err
		}
		body, err := frame.EncodeBatch(batch, win)
		if err != nil {
			return err
		}
		st, b, err := u.C.put(ctx, u.RecordingID, u.Offset, body)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		var r offsetReply
		_ = json.Unmarshal(b, &r)
		switch {
		case st == http.StatusUnauthorized, st == http.StatusForbidden:
			return ErrAuth
		case st == http.StatusOK:
			if r.NextOffset <= u.Offset {
				return fmt.Errorf("%w: the deployment did not advance", ErrFatal)
			}
			if r.NextOffset > u.Offset+int64(len(batch)) {
				// It cannot hold more than this batch brought.
				return fmt.Errorf("%w: the deployment holds more than was sent", ErrFatal)
			}
			u.Offset = r.NextOffset
			u.lastOK = u.now()
			u.mismatches, u.mismatchWait = 0, 0
			if u.Progress != nil {
				u.Progress()
			}
			return nil
		case st == http.StatusConflict && r.Error == "offset_mismatch":
			size, err := u.size()
			if err != nil {
				return err
			}
			if r.NextOffset < 0 || r.NextOffset > size {
				return fmt.Errorf("%w: the deployment holds more than the spool", ErrFatal)
			}
			u.mismatches++
			if r.NextOffset == u.Offset || u.mismatches >= 3 {
				// The deployment names the offset this batch was sent at and
				// still calls it a mismatch, or it has answered mismatch three
				// times with no batch accepted in between, as two replicas that
				// hold different offsets do: it is not letting the upload
				// move. Treat it as a link that does not work: adopt the
				// offset, back off as for a transport error, and keep the
				// spool. The first two mismatches that name another offset are
				// ordinary and are sent from at once.
				moved := r.NextOffset != u.Offset
				u.Offset = r.NextOffset
				d := u.mismatchWait
				if d == 0 {
					d = backoffMin
				}
				u.mismatchWait = min(d*2, backoffMax)
				if err := u.sleep(ctx, d); err != nil {
					return err
				}
				if moved {
					// The batch was read at the old offset. Return, so the
					// caller reads it again from the new one.
					return nil
				}
				continue
			}
			u.Offset = r.NextOffset
			return nil // the caller's loop re-reads from the new offset
		case st == http.StatusConflict && r.Error == "recording_closed":
			return ErrRecordingClosed
		case st == http.StatusConflict, st == http.StatusNotFound,
			st == http.StatusRequestEntityTooLarge, st == http.StatusUnprocessableEntity:
			return fmt.Errorf("%w (%d %s)", ErrFatal, st, r.Error)
		case st == http.StatusTooManyRequests:
			if err := u.sleep(ctx, rateLimitWait); err != nil {
				return err
			}
		default: // 5xx and anything the contract does not name
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
		}
	}
}

// size is the length of the raw stream this recording would hold once it has
// every whole frame the spool has.
func (u *Uploader) size() (int64, error) {
	st, err := u.Spool.Stat()
	if err != nil {
		return 0, err
	}
	return int64(len(u.Prefix)) + max(0, st.Size()-u.Base), nil
}

// Tick sends what the spool holds past Offset and, when nothing good has gone
// to the server for KeepaliveEvery, a keepalive. It is the one call the live
// loop makes, so only one request is in flight for the recording.
func (u *Uploader) Tick(ctx context.Context) error {
	if err := u.Pump(ctx); err != nil {
		return err
	}
	every := u.KeepaliveEvery
	if every <= 0 {
		every = KeepaliveEvery
	}
	now := u.now()
	if u.lastOK.IsZero() {
		u.lastOK = now
		return nil
	}
	if now.Sub(u.lastOK) < every {
		return nil
	}
	return u.Keepalive(ctx)
}

// Keepalive sends an empty batch at Offset. It stores nothing; it tells the
// server the recorder is alive. Failures follow the rules for an append.
func (u *Uploader) Keepalive(ctx context.Context) error {
	backoff := backoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, b, err := u.C.put(ctx, u.RecordingID, u.Offset, []byte{})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		var r offsetReply
		_ = json.Unmarshal(b, &r)
		switch {
		case st == http.StatusUnauthorized, st == http.StatusForbidden:
			return ErrAuth
		case st == http.StatusOK:
			if r.NextOffset != u.Offset {
				return u.resync(r.NextOffset)
			}
			u.lastOK = u.now()
			return nil
		case st == http.StatusConflict && r.Error == "offset_mismatch":
			return u.resync(r.NextOffset)
		case st == http.StatusConflict && r.Error == "recording_closed":
			return ErrRecordingClosed
		case st == http.StatusConflict, st == http.StatusNotFound,
			st == http.StatusRequestEntityTooLarge, st == http.StatusUnprocessableEntity:
			return fmt.Errorf("%w (%d %s)", ErrFatal, st, r.Error)
		case st == http.StatusTooManyRequests:
			if err := u.sleep(ctx, rateLimitWait); err != nil {
				return err
			}
		default:
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
		}
	}
}

// ErrNotOwner means the deployment answered 404 to the ownership check: it has
// no recording by that id for the login that asked. The recording is another
// login's, or is gone. Nothing was sent.
var ErrNotOwner = errors.New("recording: the deployment has no such recording for this login")

// errProbe means the ownership check got no answer it can act on: the
// deployment could not be reached, answered 429 or 5xx, or said something the
// contract does not name. The spool stays for a later flush.
var errProbe = errors.New("recording: the ownership check was not answered")

// Probe asks the deployment, with one empty-body request at Offset, whether the
// recording is this login's and open. It sends no data and opens nothing. It
// returns nil when it is, with Offset set to the position the deployment holds
// (a 409 offset_mismatch names it); ErrRecordingClosed when the recording is
// this login's and ended; ErrNotOwner when the deployment answers 404; ErrAuth
// for 401 or 403; and errProbe for anything else, without retrying.
func (u *Uploader) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	st, b, err := u.C.put(ctx, u.RecordingID, u.Offset, []byte{})
	if err != nil {
		return errProbe
	}
	var r offsetReply
	_ = json.Unmarshal(b, &r)
	switch {
	case st == http.StatusUnauthorized, st == http.StatusForbidden:
		return ErrAuth
	case st == http.StatusNotFound:
		return ErrNotOwner
	case st == http.StatusOK:
		if r.NextOffset == u.Offset {
			return nil
		}
	case st == http.StatusConflict && r.Error == "offset_mismatch":
	case st == http.StatusConflict && r.Error == "recording_closed":
		return ErrRecordingClosed
	default:
		return errProbe
	}
	if u.resync(r.NextOffset) != nil {
		return errProbe
	}
	return nil
}

// resync takes the offset the server says it holds.
func (u *Uploader) resync(next int64) error {
	size, err := u.size()
	if err != nil {
		return err
	}
	if next < 0 || next > size {
		return fmt.Errorf("%w: the deployment holds more than the spool", ErrFatal)
	}
	u.Offset = next
	return nil
}

// Close ends the recording once the server holds the whole spool. It pumps
// first, so a 409 offset_mismatch from the close call (send the rest, then
// close again) is handled by looping.
func (u *Uploader) Close(ctx context.Context, req CloseRequest) error {
	backoff := backoffMin
	for attempt := 0; attempt < 8; attempt++ {
		if err := u.Pump(ctx); err != nil {
			return err
		}
		size, err := u.size()
		if err != nil {
			return err
		}
		// Pump caught up with every whole frame. A partial frame at the tail
		// (a recorder that died mid-write) is not part of the recording.
		req.FinalOffset = u.Offset
		st, b, err := u.C.closeRecording(ctx, u.RecordingID, req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
			attempt--
			continue
		}
		var r offsetReply
		_ = json.Unmarshal(b, &r)
		switch {
		case st == http.StatusOK:
			return nil
		case st == http.StatusUnauthorized, st == http.StatusForbidden:
			return ErrAuth
		case st == http.StatusConflict && r.Error == "offset_mismatch":
			if r.NextOffset < 0 || r.NextOffset > size {
				return fmt.Errorf("%w: the deployment holds more than the spool", ErrFatal)
			}
			u.Offset = r.NextOffset
		case st == http.StatusTooManyRequests:
			if err := u.sleep(ctx, rateLimitWait); err != nil {
				return err
			}
			attempt--
		case st == http.StatusConflict && r.Error == "recording_closed":
			return ErrRecordingClosed
		case st == http.StatusNotFound, st == http.StatusConflict, st == http.StatusUnprocessableEntity:
			return fmt.Errorf("%w (%d %s)", ErrFatal, st, r.Error)
		default:
			if err := u.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, backoffMax)
			attempt--
		}
	}
	return errors.New("recording: close did not settle")
}
