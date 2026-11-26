// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
)

// DefaultSpoolCap is the most one run writes to its spool.
const DefaultSpoolCap = 512 << 20

// Recorder turns what the relay's Tap reports into frames in a spool file. It
// only appends to the file and never waits for the network, so it cannot slow
// the relay. The spool holds terminal output, sizes, markers and the exit
// status. It never holds input.
type Recorder struct {
	mu       sync.Mutex
	f        *os.File
	start    time.Time
	w        frame.Writer
	buf      []byte
	size     int64
	cap      int64
	capped   bool
	failed   bool
	cols     uint16
	rows     uint16
	timer    *time.Timer
	finished bool
	closed   bool
}

// How long OpenSpool waits for the lock on a spool it just created.
const (
	spoolLockTries = 10
	spoolLockPause = 10 * time.Millisecond
)

// afterSpoolCreate is called between creating a spool and locking it; a test
// uses it to hold a lock in that gap.
var afterSpoolCreate = func(string) {}

// OpenSpool creates the spool at path, mode 0600, append-only, and takes an
// exclusive lock on it for the life of the Recorder so `brig recordings flush`
// leaves a live recording alone. capBytes <= 0 means DefaultSpoolCap.
func OpenSpool(path string, start time.Time, capBytes int64) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	afterSpoolCreate(path)
	// The file is new, so nothing holds it for long. A sweep of orphan spools
	// takes a shared lock on one without a meta file for the moment it checks
	// its age, so a short wait covers that. When the lock still fails the
	// file is this call's and is removed, so no empty spool is left behind.
	var lerr error
	for i := 0; i < spoolLockTries; i++ {
		if lerr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr != syscall.EWOULDBLOCK {
			break
		}
		time.Sleep(spoolLockPause)
	}
	if lerr != nil {
		_ = os.Remove(path)
		_ = f.Close()
		return nil, lerr
	}
	if capBytes <= 0 {
		capBytes = DefaultSpoolCap
	}
	return &Recorder{f: f, start: start, cap: capBytes}, nil
}

// Path is the spool's path.
func (r *Recorder) Path() string { return r.f.Name() }

// Size is how many bytes the spool holds.
func (r *Recorder) Size() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

// Dims is the terminal size the recorder last saw, or 0 0 before any.
func (r *Recorder) Dims() (cols, rows uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cols, r.rows
}

// Capped reports that the spool cap ended the recording.
func (r *Recorder) Capped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capped
}

// Failed reports that a write to the spool failed, which ended the recording.
func (r *Recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

func (r *Recorder) elapsed(at time.Time) time.Duration { return at.Sub(r.start) }

// write appends r.buf to the spool, enforcing the cap. The caller holds mu.
func (r *Recorder) write() {
	if len(r.buf) == 0 {
		return
	}
	defer func() { r.buf = r.buf[:0] }()
	if r.capped || r.failed {
		return
	}
	if r.size+int64(len(r.buf)) > r.cap {
		// Stop here: one marker says why. The marker may take the spool a few
		// bytes past the cap, which is the only overshoot.
		r.capped = true
		m := frame.Append(nil, 0, frame.Marker, []byte("spool cap reached, recording stopped"))
		r.append(m)
		return
	}
	r.append(r.buf)
}

func (r *Recorder) append(b []byte) {
	n, err := r.f.Write(b)
	r.size += int64(n)
	if err != nil {
		r.failed = true
	}
}

// Output implements ptyrelay.Tap.
func (r *Recorder) Output(at time.Time, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.capped || r.failed {
		return
	}
	r.buf = r.w.Output(r.buf[:0], r.elapsed(at), p)
	r.write()
	r.armLocked()
}

// armLocked makes sure pending output is written once its merge window ends,
// even if no more output comes.
func (r *Recorder) armLocked() {
	if !r.w.Pending() || r.timer != nil {
		return
	}
	r.timer = time.AfterFunc(frame.MergeWindow, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.timer = nil
		if r.finished {
			return
		}
		r.buf = r.w.Tick(r.buf[:0], r.elapsed(time.Now()))
		r.write()
		r.armLocked()
	})
}

// Dropped implements ptyrelay.Tap: it writes a marker at this position.
func (r *Recorder) Dropped(at time.Time, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.capped || r.failed {
		return
	}
	r.buf = r.w.Marker(r.buf[:0], r.elapsed(at), fmt.Sprintf("dropped %d bytes", n))
	r.write()
}

// Resize implements ptyrelay.Tap. A size equal to the last one is not written
// again.
func (r *Recorder) Resize(at time.Time, cols, rows uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.capped || r.failed || (cols == r.cols && rows == r.rows) {
		return
	}
	r.cols, r.rows = cols, rows
	r.buf = r.w.Resize(r.buf[:0], r.elapsed(at), cols, rows)
	r.write()
}

// Finish writes what is pending and the exit frame. A capped or failed
// recording gets no exit frame. It does not close the spool: the exclusive lock
// stays with the Recorder until Close, so no other process sends the spool
// while the run is still draining it.
func (r *Recorder) Finish(at time.Time, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.capped || r.failed {
		r.buf = r.buf[:0]
	} else {
		r.buf = r.w.Exit(r.buf[:0], r.elapsed(at), status)
		r.write()
	}
}

// Close closes the spool, which drops the exclusive lock. The owner of the spool
// calls it once it has removed the spool or decided to leave it for a later
// flush. It is safe to call more than once.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	_ = r.f.Close()
}

// Abort stops recording without an exit frame. Like Finish it leaves the spool
// open and locked until Close.
func (r *Recorder) Abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	if r.timer != nil {
		r.timer.Stop()
	}
}
