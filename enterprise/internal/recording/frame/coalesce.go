// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package frame

import "time"

// Writer turns timed events into frame bytes. Time is whatever the caller
// passes in, as elapsed time since the start of the recording, so the same
// events give the same bytes. A frame's time is rounded down to whole
// milliseconds and dt is the difference of those, so rounding never drifts.
type Writer struct {
	last    int64 // ms of the previous frame
	pending []byte
	pendAt  time.Duration
}

func ms(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

func (w *Writer) emit(dst []byte, at time.Duration, typ byte, payload []byte) []byte {
	t := ms(at)
	if t < w.last {
		t = w.last
	}
	dst = Append(dst, uint64(t-w.last), typ, payload)
	w.last = t
	return dst
}

// Output adds terminal output seen at elapsed time at, and appends to dst any
// frames that are now complete. Output within MergeWindow of the first byte of
// the pending frame joins it, up to MaxOutput bytes per frame.
func (w *Writer) Output(dst []byte, at time.Duration, p []byte) []byte {
	if len(w.pending) > 0 && at-w.pendAt >= MergeWindow {
		dst = w.Flush(dst)
	}
	for len(p) > 0 {
		if len(w.pending) == 0 {
			w.pendAt = at
		}
		room := MaxOutput - len(w.pending)
		n := min(room, len(p))
		w.pending = append(w.pending, p[:n]...)
		p = p[n:]
		if len(w.pending) == MaxOutput {
			dst = w.Flush(dst)
		}
	}
	return dst
}

// Tick appends the pending frame to dst when its merge window has passed at
// elapsed time at.
func (w *Writer) Tick(dst []byte, at time.Duration) []byte {
	if len(w.pending) > 0 && at-w.pendAt >= MergeWindow {
		return w.Flush(dst)
	}
	return dst
}

// Pending reports whether output waits for its merge window.
func (w *Writer) Pending() bool { return len(w.pending) > 0 }

// Flush appends the pending output frame, stamped with the time of its first
// byte.
func (w *Writer) Flush(dst []byte) []byte {
	if len(w.pending) == 0 {
		return dst
	}
	dst = w.emit(dst, w.pendAt, Output, w.pending)
	w.pending = w.pending[:0]
	return dst
}

// Resize flushes pending output, then appends a resize frame.
func (w *Writer) Resize(dst []byte, at time.Duration, cols, rows uint16) []byte {
	dst = w.Flush(dst)
	return w.emit(dst, at, Resize, ResizePayload(cols, rows))
}

// Marker flushes pending output, then appends a marker frame. A label over
// MaxMarker bytes is cut.
func (w *Writer) Marker(dst []byte, at time.Duration, label string) []byte {
	dst = w.Flush(dst)
	if len(label) > MaxMarker {
		label = label[:MaxMarker]
	}
	return w.emit(dst, at, Marker, []byte(label))
}

// Exit flushes pending output, then appends the exit frame.
func (w *Writer) Exit(dst []byte, at time.Duration, status int) []byte {
	dst = w.Flush(dst)
	return w.emit(dst, at, Exit, ExitPayload(status))
}
