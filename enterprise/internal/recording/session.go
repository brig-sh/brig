// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/brig/enterprise/internal/ptyrelay"
	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
)

const (
	flushEvery   = 250 * time.Millisecond
	drainTimeout = 5 * time.Second
	spoolExt     = ".spool"
	metaExt      = ".json"
	// ackedEvery is how often the live loop saves how much the server has
	// taken, for a later flush.
	ackedEvery = 2 * time.Second
)

// NewRunKey returns a random UUID (version 4) for one run.
func NewRunKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Meta is what a later `brig recordings flush` needs to finish a recording:
// where it is, how it was opened, and how the run ended. It holds no token.
//
// RunKey, RecordingID and Open describe the recording the run is in now. The
// spool is the run's whole frame stream. The current recording's raw stream is
// Prefix followed by the spool from SpoolBase on, so recording offset N is spool
// position SpoolBase + N - len(Prefix). A run that never lost a recording has
// no Prefix and SpoolBase 0.
type Meta struct {
	RunKey      string      `json:"run_key"`
	URL         string      `json:"url"`
	RecordingID string      `json:"recording_id"`
	Open        OpenRequest `json:"open"`
	Ended       bool        `json:"ended"`
	EndReason   string      `json:"end_reason,omitempty"`
	ExitCode    *int        `json:"exit_code,omitempty"`
	Signal      string      `json:"signal,omitempty"`

	// Recordings is how many recordings the run has opened, 1 or more. Zero
	// in a file an older brig wrote means 1.
	Recordings int `json:"recordings,omitempty"`
	// SpoolBase is the spool position of the first frame the current
	// recording holds from the spool.
	SpoolBase int64 `json:"spool_base,omitempty"`
	// Prefix is the resize frame at the start of the current recording, ahead
	// of the spool frames. Empty for the first recording.
	Prefix []byte `json:"prefix,omitempty"`
	// Acked is a spool position the server is known to hold. It may lag the
	// truth, never lead it. A flush uses it when the current recording was
	// closed by the server and so cannot say how much it took.
	Acked int64 `json:"acked,omitempty"`
	// OpenedAt is when the current recording was opened. Wait is the pause the
	// loop guard put before opening it. Both feed the guard for the next one.
	OpenedAt time.Time     `json:"opened_at,omitzero"`
	Wait     time.Duration `json:"wait,omitempty"`
}

// Guard keeps a server that closes every recording at once from making the
// recorder open recordings in a tight loop. It limits how often a continuation
// recording is opened, never how many a run may use. The zero value means the
// defaults.
type Guard struct {
	// Initial is the first pause after a continuation that was closed less than
	// Initial after it was opened. Each further early closure doubles it.
	Initial time.Duration
	// Max caps the pause.
	Max time.Duration
	// Reset is how long a continuation must stay open to clear the pause.
	Reset time.Duration
}

func (g Guard) withDefaults() Guard {
	if g.Initial <= 0 {
		g.Initial = time.Minute
	}
	if g.Max <= 0 {
		g.Max = 5 * time.Minute
	}
	if g.Reset <= 0 {
		g.Reset = 10 * time.Minute
	}
	return g
}

// plan says how long the next continuation must be put off, given the meta of
// the recording the server just closed, and the earliest time to open it. The
// first continuation of a run is opened at once.
func (g Guard) plan(m Meta, now time.Time) (wait time.Duration, notBefore time.Time) {
	g = g.withDefaults()
	if m.count() <= 1 || m.OpenedAt.IsZero() {
		return 0, now
	}
	life := now.Sub(m.OpenedAt)
	switch {
	case life >= g.Reset:
		wait = 0
	case life < g.Initial:
		wait = g.Initial
		if m.Wait > 0 {
			wait = min(2*m.Wait, g.Max)
		}
	default:
		wait = m.Wait
	}
	return wait, m.OpenedAt.Add(wait)
}

// flushClock is the clock Flush reads; tests replace it.
var flushClock = time.Now

func (m Meta) count() int { return max(m.Recordings, 1) }

func writeMeta(path string, m Meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Params says how to record one run.
type Params struct {
	Client *Client
	// URL is the deployment's normalised URL, kept in the meta file so a later
	// flush finds the right token.
	URL string
	// Dir is the directory the spool and meta file go in.
	Dir string
	// Open describes the run. RunKey and StartedAt are filled in by Begin when
	// empty.
	Open OpenRequest
	// SpoolCap is the spool's byte limit; zero means DefaultSpoolCap.
	SpoolCap int64
	// DrainTimeout bounds how long the end of a run waits for the network;
	// zero means 5 s.
	DrainTimeout time.Duration
	// FlushEvery is the pause between uploads while the run is live; zero means
	// 250 ms.
	FlushEvery time.Duration
	// Sleep replaces the uploader's timer in tests.
	Sleep func(ctx context.Context, d time.Duration) error
	// In, Out and Err are the user's streams; nil means os.Stdin, os.Stdout and
	// os.Stderr. See ptyrelay.Options.
	In, Out, Err *os.File
	// Now is the clock; nil means time.Now. KeepaliveEvery is the quiet time
	// before a keepalive; zero means KeepaliveEvery (45 s).
	Now            func() time.Time
	KeepaliveEvery time.Duration
	// Token and NewClient let the run send what earlier runs left in Dir, in the
	// background. When either is nil nothing is sent.
	Token     TokenFunc
	NewClient func(url, token string) *Client
	// Guard sets the pauses between continuation recordings; zero means the
	// defaults (60 s, doubling to 5 min, cleared after 10 min open).
	Guard Guard
	// Notice receives the lines printed when the run ends; nil means
	// os.Stderr. Nothing is written to it while the run goes on.
	Notice io.Writer
}

// Session is one recorded run.
type Session struct {
	p     Params
	meta  Meta
	rec   *Recorder
	up    *Uploader
	paths [2]string // spool, meta

	// interrupted: a recording was closed by the server and the run went on
	// in another. stopped: recording ended before the run did.
	interrupted bool
	// leftover: when the run ended, some of its recording was still on this
	// machine, so the spool stays for the next recorded run or `brig
	// recordings flush`. earlier counts the earlier runs' recordings the
	// background flush finished.
	leftover bool
	// fatal: the deployment refused the recording for good while the run ended,
	// so the rest was not sent and the spool is gone. authRefused: the
	// deployment refused the login at the end, so the spool waits for
	// `brig login`.
	fatal, authRefused bool
	// interrupt is closed when a signal arrives after the command has ended.
	interrupt chan struct{}
	// notStarted: the command could not be started on the relay, so the run is
	// not recorded after all.
	notStarted bool
	earlier    int
	// earlierDropped counts earlier spools the background flush removed
	// without sending them.
	earlierDropped int
	// earlierExpired counts earlier spools the background flush removed for
	// their age.
	earlierExpired int
	stopped        bool
	stoppedAt      time.Time
	stopReason     string
	savedAt        time.Time
	savedAcked     int64
}

func (s *Session) now() time.Time {
	if s.p.Now != nil {
		return s.p.Now()
	}
	return time.Now()
}

// Begin asks the deployment whether the run is recorded. It returns nil with a
// nil error when it is not, and an error when the answer could not be had. A
// non-nil Session has opened a recording and created its spool. When Begin
// fails after the deployment opened a recording, it removes every file it
// created and asks the deployment to close the recording.
func Begin(ctx context.Context, p Params) (*Session, error) {
	if p.Open.RunKey == "" {
		p.Open.RunKey = NewRunKey()
	}
	start := time.Now()
	if p.Open.StartedAt == "" {
		p.Open.StartedAt = start.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	resp, err := p.Client.Open(ctx, p.Open)
	if err != nil {
		return nil, err
	}
	if !resp.Recorded {
		return nil, nil
	}
	s := &Session{p: p}
	s.paths = [2]string{
		filepath.Join(p.Dir, p.Open.RunKey+spoolExt),
		filepath.Join(p.Dir, p.Open.RunKey+metaExt),
	}
	// fail undoes what Begin did so far. The spool file is removed only when
	// this call created it, and the meta file only when this call wrote it.
	metaWritten := false
	fail := func(err error) (*Session, error) {
		if metaWritten {
			_ = removeFile(s.paths[1])
		}
		if s.rec != nil {
			s.rec.Abort()
			_ = removeFile(s.paths[0])
			s.rec.Close()
		}
		cctx, cancel := context.WithTimeout(context.Background(), OpenTimeout)
		defer cancel()
		_, _, _ = p.Client.closeRecording(cctx, resp.RecordingID, CloseRequest{EndReason: "recorder_error"})
		return nil, err
	}
	if err := privateDir(p.Dir); err != nil {
		return fail(err)
	}
	rec, err := OpenSpool(s.paths[0], start, p.SpoolCap)
	if err != nil {
		return fail(err)
	}
	s.rec = rec
	s.meta = Meta{RunKey: p.Open.RunKey, URL: p.URL, RecordingID: resp.RecordingID, Open: p.Open, Recordings: 1, OpenedAt: s.now()}
	if err := writeMeta(s.paths[1], s.meta); err != nil {
		return fail(err)
	}
	metaWritten = true
	rd, err := openForUpload(s.paths[0])
	if err != nil {
		return fail(err)
	}
	s.up = &Uploader{C: p.Client, RecordingID: resp.RecordingID, Spool: rd, Offset: resp.NextOffset,
		MaxBatch: resp.MaxBatchBytes, WindowBytes: resp.WindowBytes, Sleep: p.Sleep,
		Now: p.Now, KeepaliveEvery: p.KeepaliveEvery}
	// The first frame is the size the run starts at.
	rec.Resize(start, uint16(p.Open.Cols), uint16(p.Open.Rows))
	return s, nil
}

// Run runs argv on the relay, recording its output, and uploads while it runs.
// When the run ends it drains what is left for at most the drain timeout, then
// closes the recording. What the network did not take stays in the spool for
// `brig recordings flush`.
func (s *Session) Run(argv, env []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env

	liveCtx, stopLive := context.WithCancel(context.Background())
	liveDone := make(chan struct{})
	every := s.p.FlushEvery
	if every <= 0 {
		every = flushEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	stopEarlier := s.startEarlier()
	go func() {
		defer close(liveDone)
		s.liveLoop(liveCtx, ticker.C)
	}()

	// The relay takes SIGINT, SIGTERM and SIGHUP while the command runs and
	// forwards them. When the command has ended it stops, and brig stays up for
	// the drain. Signals are taken here from before the command starts, so no
	// window is left where the default action would end brig, and what arrived
	// while the command ran, which the relay forwarded, is dropped when it ends.
	// A signal that was inherited as ignored stays ignored.
	sigs := make(chan os.Signal, 8)
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)

	res, err := ptyrelay.Run(cmd, ptyrelay.Options{In: s.p.In, Out: s.p.Out, Err: s.p.Err, Tap: s.rec})
	end := time.Now()
	for drained := false; !drained; {
		select {
		case <-sigs:
		default:
			drained = true
		}
	}
	interrupt := make(chan struct{})
	s.interrupt = interrupt
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-sigs:
			close(interrupt)
		case <-stopWatch:
		}
	}()

	stopLive()
	<-liveDone
	// The background flush ends with the run, before this run's spool closes.
	e := stopEarlier()
	s.earlier, s.earlierDropped, s.earlierExpired = e.Closed, e.Dropped, e.Expired

	if err != nil {
		s.rec.Abort()
		s.finish("recorder_error", nil, "")
		// A failure before the command started (ptyrelay.ErrNotStarted) means
		// nothing ran and the caller can run the command without the relay. The
		// recording opened for it is closed above and its files are removed, and
		// the run says it is not recorded after all. A failure after the command
		// ran cannot be undone, and is returned as it was.
		s.notStarted = errors.Is(err, ptyrelay.ErrNotStarted)
		s.printNotices()
		return 0, err
	}
	reason, sig := "exit", ""
	if res.Signal != 0 {
		reason, sig = "signal", unix.SignalName(res.Signal)
	}
	s.rec.Finish(end, res.ExitCode)
	switch {
	case s.rec.Failed():
		reason = "recorder_error"
	case s.rec.Capped():
		reason = "spool_cap"
	}
	// The status is known only when the run ended with an exit frame.
	var code *int
	if reason == "exit" || reason == "signal" {
		code = &res.ExitCode
	}
	s.finish(reason, code, sig)
	s.printNotices()
	return res.ExitCode, nil
}

// liveLoop uploads on every tick until ctx ends or uploading stops for good: the
// server refused the recording, the login was refused, or recording stopped.
func (s *Session) liveLoop(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := s.tick(ctx); errors.Is(err, ErrFatal) || errors.Is(err, ErrAuth) || errors.Is(err, errStopped) || errors.Is(err, errGiveUp) {
				return
			}
		}
	}
}

// finish notes how the run ended, then drains and closes within the bound.
//
// The spool stays locked until finish has removed it or decided to leave it for
// a later flush, so no other process sends it while this run still drains it.
func (s *Session) finish(reason string, code *int, sig string) {
	defer s.rec.Close()
	if s.stopped {
		_ = s.up.Spool.Close()
		return // the spool is already gone
	}
	s.meta.Ended, s.meta.EndReason, s.meta.ExitCode, s.meta.Signal = true, reason, code, sig
	_ = writeMeta(s.paths[1], s.meta)
	d := s.p.DrainTimeout
	if d <= 0 {
		d = drainTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	// A signal that arrives after the command has ended ends the drain early.
	// The spool stays for a later flush, with the usual closing line.
	if s.interrupt != nil {
		go func() {
			select {
			case <-s.interrupt:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	req := CloseRequest{EndReason: reason, ExitCode: code, Signal: sig}
	var err error
	for {
		// Catch up first and save how far the server got, so a flush after a
		// crash in the close call does not send the same frames again. The
		// position is saved whatever Pump returned: the offset moves only on
		// an answer from the server, so after a drain cut off by its deadline it
		// is still what the server holds.
		_ = s.up.Pump(ctx)
		s.save(true)
		err = s.up.Close(ctx, req)
		if !errors.Is(err, ErrRecordingClosed) {
			break
		}
		if err = s.recover(ctx, true); err != nil {
			break
		}
	}
	_ = s.up.Spool.Close()
	if s.stopped {
		return
	}
	s.authRefused = errors.Is(err, ErrAuth)
	s.fatal = errors.Is(err, ErrFatal)
	if err == nil || errors.Is(err, ErrFatal) {
		s.remove()
		return
	}
	// Close pumps too, so it may have moved the offset before it failed.
	s.save(true)
	s.leftover = true
}

// errStopped means the run is no longer recorded.
var errStopped = errors.New("recording: recording stopped")

// tick is one turn of the live loop.
func (s *Session) tick(ctx context.Context) error {
	if s.stopped {
		return errStopped
	}
	err := s.up.Tick(ctx)
	if errors.Is(err, ErrRecordingClosed) {
		err = s.recover(ctx, false)
	}
	if err == nil {
		s.save(false)
	}
	return err
}

// save writes the meta file with the spool position the server holds, at most
// every ackedEvery unless force is set.
func (s *Session) save(force bool) {
	pos := s.up.SpoolPos(s.up.Offset)
	if s.stopped || pos == s.savedAcked || (!force && s.now().Sub(s.savedAt) < ackedEvery) {
		return
	}
	s.meta.Acked = pos
	if writeMeta(s.paths[1], s.meta) == nil {
		s.savedAt, s.savedAcked = s.now(), pos
	}
}

// recover continues the run in a new recording after the server closed the
// current one. It returns nil when the new recording is open and the uploader
// points at it, ErrAuth when the login was refused (the spool stays), the
// context's error when ctx ended first (the spool stays, and a flush recovers),
// errGiveUp when the meta file for the new recording could not be written (the
// uploader stays on the old recording and the spool stays), and errStopped when
// the run is no longer recorded.
//
// The loop guard may put the open off. While it waits the spool keeps growing.
// With final set, which the end of the run uses, a wait is not served: the spool
// stays for `brig recordings flush` and errWaiting is returned.
func (s *Session) recover(ctx context.Context, final bool) error {
	cols, rows := s.rec.Dims()
	backoff := backoffMin
	wait, notBefore := s.p.Guard.plan(s.meta, s.now())
	if d := notBefore.Sub(s.now()); d > 0 {
		if final {
			return errWaiting
		}
		if err := s.up.sleep(ctx, d); err != nil {
			return err
		}
	}
	for {
		m, resp, err := nextRecording(ctx, s.p.Client, s.meta, s.up.SpoolPos(s.up.Offset), cols, rows, s.now(), wait)
		var retry openRetry
		switch {
		case err == nil:
			if werr := writeMeta(s.paths[1], m); werr != nil {
				// The file on disk still names the old recording, so the
				// uploader must not send into the new one: a later flush
				// would send the same output again. The new recording is
				// left to close on the server when it goes quiet. The run
				// goes on and is not stopped; live upload ends and the spool
				// stays for a flush.
				return fmt.Errorf("%w: the meta file could not be written: %v", errGiveUp, werr)
			}
			s.meta = m
			s.up.Switch(resp.RecordingID, m.SpoolBase, m.Prefix, resp.MaxBatchBytes, resp.WindowBytes)
			s.savedAcked, s.savedAt = m.SpoolBase, s.now()
			s.interrupted = true
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrAuth):
			return err
		case errors.As(err, &retry):
			// The run is recorded and the spool holds output nobody else has.
			// Keep trying for as long as the run lives; the spool keeps growing.
			wait := backoff
			if retry.rateLimited {
				wait = rateLimitWait
			} else {
				backoff = min(backoff*2, backoffMax)
			}
			if err := s.up.sleep(ctx, wait); err != nil {
				return err
			}
		case errors.Is(err, errGiveUp):
			return err
		default:
			s.stop(stopWords(err))
			return errStopped
		}
	}
}

// stop ends recording for the rest of the run and removes the spool, as for a
// run that was never recorded.
func (s *Session) stop(reason string) {
	s.stopped, s.stoppedAt, s.stopReason = true, s.now(), reason
	s.remove()
	s.rec.Abort()
}

// errWaiting means the loop guard puts the next recording off past the end of
// the run. The spool stays.
var errWaiting = errors.New("recording: the next recording waits on the loop guard")

// errGiveUp means the deployment answered the open in a way no retry fixes but
// did not say the run is unrecorded. The spool stays.
var errGiveUp = errors.New("recording: the deployment's answer to the open cannot be continued from")

// errUnrecorded is why a continuation could not be opened.
type errUnrecorded struct{ words string }

func (e errUnrecorded) Error() string { return "recording: not continued: " + e.words }

func stopWords(err error) string {
	var u errUnrecorded
	if errors.As(err, &u) {
		return u.words
	}
	return "the deployment said not to record"
}

// nextRecording opens the recording that continues m's run after the server
// closed m's recording. It uses a new run key and the terminal's current size.
// acked is the spool position the old recording holds: the new one starts with
// one resize frame, then the spool from acked on.
func nextRecording(ctx context.Context, c *Client, m Meta, acked int64, cols, rows uint16, now time.Time, wait time.Duration) (Meta, OpenResponse, error) {
	open := m.Open
	open.RunKey = NewRunKey()
	if cols > 0 && rows > 0 {
		open.Cols, open.Rows = int(cols), int(rows)
	}
	open.StartedAt = now.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	resp, err := c.Open(ctx, open)
	if err != nil {
		var retry openRetry
		if errors.Is(err, ErrAuth) || errors.As(err, &retry) || ctx.Err() != nil {
			return m, resp, err
		}
		return m, resp, errGiveUp
	}
	if !resp.Recorded {
		return m, resp, errUnrecorded{unrecordedWords(resp.Reason)}
	}
	if resp.NextOffset != 0 {
		return m, resp, errGiveUp
	}
	m.RunKey, m.RecordingID, m.Open = open.RunKey, resp.RecordingID, open
	m.Recordings = m.count() + 1
	m.OpenedAt, m.Wait = now, wait
	m.SpoolBase = acked
	m.Acked = acked
	m.Prefix = frame.Append(nil, 0, frame.Resize, frame.ResizePayload(uint16(open.Cols), uint16(open.Rows)))
	return m, resp, nil
}

func unrecordedWords(reason string) string {
	switch reason {
	case "setting_off":
		return "your organization turned recording off"
	case "not_configured":
		return "recording is not set up on the deployment"
	case "closed":
		return "the deployment closed the new recording as well"
	}
	return "the deployment said not to record"
}

// ordinal names the n-th recording in words up to ten.
func ordinal(n int) string {
	names := [...]string{"", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth"}
	if n > 0 && n < len(names) {
		return names[n]
	}
	return ""
}

// The closing lines that are the same every run.
const (
	leftoverNotice    = "brig: the end of this run's recording is still on this machine; the next recorded run or `brig recordings flush` will send it"
	fatalNotice       = "brig: this run's recording stopped and the rest of it was not sent"
	authRefusedNotice = "brig: the deployment refused your login, so the rest of this run's recording was not sent; sign in again with `brig login`, then run `brig recordings flush`"
)

// Notices are the lines the run's end prints to stderr, one for each thing
// that happened.
func (s *Session) Notices() []string {
	var out []string
	if s.interrupted {
		if w := ordinal(s.meta.count()); w != "" {
			out = append(out, fmt.Sprintf("brig: this run's recording was interrupted and continues in a %s recording", w))
		} else {
			out = append(out, fmt.Sprintf("brig: this run's recording was interrupted and the run used %d recordings", s.meta.count()))
		}
	}
	if s.notStarted {
		out = append(out, "brig: this run is not recorded after all: Brig could not set up the recorder")
	}
	switch {
	case s.authRefused:
		out = append(out, authRefusedNotice)
	case s.leftover:
		out = append(out, leftoverNotice)
	}
	if s.fatal {
		out = append(out, fatalNotice)
	}
	if s.stopped {
		out = append(out, fmt.Sprintf("brig: recording stopped before the run ended (%s); output after %s is not recorded",
			s.stopReason, s.stoppedAt.Local().Format("15:04:05")))
	}
	if s.rec != nil && s.rec.Capped() {
		limit := s.p.SpoolCap
		if limit <= 0 {
			limit = DefaultSpoolCap
		}
		out = append(out, fmt.Sprintf("brig: recording stopped part way through this run because it reached the limit of %d MiB; the rest of its output is not recorded", limit>>20))
	}
	if s.rec != nil && s.rec.Failed() {
		out = append(out, "brig: recording stopped part way through this run because Brig could not write the recording to disk; the rest of its output is not recorded")
	}
	switch {
	case s.earlier == 1:
		out = append(out, "brig: sent the rest of 1 earlier recording")
	case s.earlier > 1:
		out = append(out, fmt.Sprintf("brig: sent the rest of %d earlier recordings", s.earlier))
	}
	switch {
	case s.earlierDropped == 1:
		out = append(out, "brig: removed 1 earlier recording without sending it")
	case s.earlierDropped > 1:
		out = append(out, fmt.Sprintf("brig: removed %d earlier recordings without sending them", s.earlierDropped))
	}
	switch {
	case s.earlierExpired == 1:
		out = append(out, "brig: removed 1 earlier recording that was more than 30 days old and could not be sent")
	case s.earlierExpired > 1:
		out = append(out, fmt.Sprintf("brig: removed %d earlier recordings that were more than 30 days old and could not be sent", s.earlierExpired))
	}
	return out
}

func (s *Session) printNotices() {
	w := s.p.Notice
	if w == nil {
		w = os.Stderr
	}
	for _, l := range s.Notices() {
		fmt.Fprintln(w, l)
	}
}

func (s *Session) remove() {
	removePair(s.paths[1], s.paths[0])
}

// openForUpload opens the spool for the uploader; a test replaces it.
var openForUpload = os.Open

// removeFile removes one file; a test replaces it to see the order.
var removeFile = os.Remove

// removePair removes a recording's meta file and then its spool. The order is
// the contract: a flush finds a recording by its meta file, so a meta file that
// is gone while its spool is still there is a recording being tidied, and a meta
// file whose spool is gone is a spool somebody removed.
func removePair(meta, spool string) {
	_ = removeFile(meta)
	_ = removeFile(spool)
}

// privateDir creates dir, mode 0700, and takes group and other access away
// from one that already exists with it: the spool holds the terminal output
// of the person's runs, and a directory somebody else can list is a way to
// reach it.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// Dir returns the recordings directory under a brig state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "recordings") }

// TokenFunc returns the access token to use for a deployment URL, or "" when
// there is none that works. Its second result says whether the URL names a
// deployment a login could ever exist for: false for a URL that does not parse,
// which no sign-in fixes. A token that is missing for a URL that is usable may be
// there after the next sign-in.
type TokenFunc func(url string) (token string, usable bool)

// FlushResult counts what Flush did.
type FlushResult struct {
	Closed int // recordings finished and removed
	// Dropped counts spools removed without being sent: the deployment said
	// not to record, refused the recording for good, or the spool was gone.
	Dropped int
	Skipped int // live, or no usable login for the deployment
	Pending int // still on disk because the network did not take them
	// AuthRefused counts spools left because the deployment answered 401 or
	// 403. The user must sign in again.
	AuthRefused int
	// NotOurs counts spools left because the deployment does not know their
	// recording for the login that is stored now: another login recorded them.
	// Nothing was sent for them.
	NotOurs int
	// Expired counts spools removed unsent because they were older than
	// MaxSpoolAge and could not be sent for a reason that does not pass: the
	// deployment answered 404, so the recording is another login's or gone, or
	// the meta file cannot be used (it does not parse, or names no usable URL). A spool that only lacks a usable login now
	// (none stored, expired, an unaccepted trust store, no keychain) is never
	// removed for its age, because a later sign-in sends it.
	Expired int
}

// MaxSpoolAge is how old a spool that cannot be sent may get before a flush
// removes it. Age is the time since the spool was last written.
const MaxSpoolAge = 30 * 24 * time.Hour

// flushOpts says how a flush differs from the one the user asks for.
type flushOpts struct {
	// skip is the base name (without extension) of a spool to leave alone.
	skip string
	// stopOnAuth ends the flush at the first spool the deployment refuses the
	// login for, instead of trying the rest.
	stopOnAuth bool
}

// Flush ships every spool in dir that has an open recording and closes it. A
// spool held by a live recorder is skipped. newClient builds the client for a
// deployment URL and token. Only one flush runs at a time on a machine: when
// another holds the lock in dir, Flush sends nothing and counts every spool as
// skipped.
func Flush(ctx context.Context, dir string, token TokenFunc, newClient func(url, token string) *Client) (FlushResult, error) {
	return flushDir(ctx, dir, token, newClient, flushOpts{})
}

func flushDir(ctx context.Context, dir string, token TokenFunc, newClient func(url, token string) *Client, o flushOpts) (FlushResult, error) {
	var res FlushResult
	metas, err := filepath.Glob(filepath.Join(dir, "*"+metaExt))
	if err != nil {
		return res, err
	}
	spools, _ := filepath.Glob(filepath.Join(dir, "*"+spoolExt))
	if len(metas) == 0 && len(spools) == 0 {
		return res, nil
	}
	// The lock is on the spool directory itself, so it leaves no file behind.
	lock, err := os.Open(dir)
	if err != nil {
		return res, err
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		res.Skipped = len(metas)
		return res, nil
	}
	res.Expired += sweepOrphans(spools)
	for _, mp := range metas {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		base := strings.TrimSuffix(mp, metaExt)
		if o.skip != "" && filepath.Base(base) == o.skip {
			res.Skipped++
			continue
		}
		out, err := flushOne(ctx, base, token, newClient)
		switch {
		case out == outAuth || errors.Is(err, ErrAuth):
			res.AuthRefused++
			if o.stopOnAuth {
				return res, nil
			}
		case out == outSkipped:
			res.Skipped++
		case out == outSent:
			res.Closed++
		case out == outDropped:
			res.Dropped++
		case out == outNotOurs:
			res.NotOurs++
		case out == outExpired:
			res.Expired++
		case err != nil || out == outPending:
			res.Pending++
		}
	}
	return res, nil
}

// beforeSpoolLock is called between opening a spool and locking it; a test
// uses it to let the run that owns the spool finish in that gap.
var beforeSpoolLock = func() {}

// outcome is what flushOne did with one spool.
type outcome int

const (
	outPending outcome = iota // left on disk, a later flush tries again
	outSent                   // the deployment has it all and the files are gone
	outDropped                // removed without being sent
	outSkipped                // live, no login, or not ours to touch
	outNotOurs                // the deployment does not know the recording for this login
	outExpired                // too old and could not be sent: removed
	outAuth                   // the deployment refused the login
)

// sweepOrphans removes spools that have no meta file, belong to no live
// recorder and are older than MaxSpoolAge. Nothing can send such a spool. The
// caller holds the directory lock. It returns how many it removed.
func sweepOrphans(spools []string) int {
	n := 0
	for _, sp := range spools {
		base := strings.TrimSuffix(sp, spoolExt)
		if _, err := os.Stat(base + metaExt); err == nil {
			continue
		}
		f, err := os.Open(sp)
		if err != nil {
			continue
		}
		if unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB) == nil && sameFile(f, sp) && spoolExpired(f) {
			_ = os.Remove(sp)
			n++
		}
		f.Close()
	}
	return n
}

// sameFile reports whether path still names the file f has open.
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
}

// spoolExpired reports whether the spool in f was last written more than
// MaxSpoolAge ago.
func spoolExpired(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && flushClock().Sub(st.ModTime()) > MaxSpoolAge
}

// startEarlier starts one background flush of the spools earlier runs left in
// the spool directory, and returns the function that ends it. That function
// cancels the flush, waits for it to stop and returns how many recordings it
// finished and how many spools it removed without sending them. The flush never prints. It leaves alone this run's spool and any
// spool a live recorder holds (a recorder keeps an exclusive lock on its spool,
// which flushOne finds by taking a shared one). A cancelled flush leaves each
// spool as resumable as it was.
// earlierResult is what the background flush did: recordings finished, spools
// removed without being sent, and spools removed for their age.
type earlierResult struct{ Closed, Dropped, Expired int }

func (s *Session) startEarlier() func() earlierResult {
	if s.p.Token == nil || s.p.NewClient == nil {
		return func() earlierResult { return earlierResult{} }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan earlierResult, 1)
	own := strings.TrimSuffix(filepath.Base(s.paths[1]), metaExt)
	go func() {
		res, _ := flushDir(ctx, s.p.Dir, s.p.Token, s.p.NewClient, flushOpts{skip: own, stopOnAuth: true})
		done <- earlierResult{res.Closed, res.Dropped, res.Expired}
	}()
	return func() earlierResult {
		cancel()
		select {
		case n := <-done:
			return n
		case <-time.After(2 * time.Second):
			return earlierResult{}
		}
	}
}

// missingSpool settles a meta file whose spool cannot be opened. A run removes
// its meta file before its spool, so a meta file with no spool is never a
// recording that is being tidied. If the meta file is gone too, the pair was
// removed by its own run, between the listing and this open, and nothing is
// counted. Otherwise the spool was removed by something else: the meta file is
// removed and counted as removed without being sent.
func missingSpool(base string, openErr error) (outcome, error) {
	if !errors.Is(openErr, fs.ErrNotExist) {
		return outSkipped, nil
	}
	if _, err := os.Stat(base + metaExt); err != nil {
		return outSkipped, nil
	}
	_ = removeFile(base + metaExt)
	return outDropped, nil
}

// flushOne sends one leftover spool, written by an earlier process.
//
// Nothing about the spool is trusted before it is locked: the lock is taken on
// the open file, the path is checked to still name that file, and only then is
// the meta file read, so a run that is finishing and removing its spool cannot
// leave a stale meta file here.
//
// The login stored for the deployment now need not be the login that recorded
// the spool. So the flush first asks the deployment about the recording the
// meta file names, with an empty-body keepalive and the current token, and
// sends or opens nothing until the answer says the recording is this login's.
func flushOne(ctx context.Context, base string, token TokenFunc, newClient func(url, token string) *Client) (outcome, error) {
	f, err := os.Open(base + spoolExt)
	if err != nil {
		return missingSpool(base, err)
	}
	defer f.Close()
	beforeSpoolLock()
	// A live recorder holds an exclusive lock on its spool.
	if unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil || !sameFile(f, base+spoolExt) {
		return outSkipped, nil
	}
	drop := func() { removePair(base+metaExt, base+spoolExt) }
	// unsendable is the answer for a spool that cannot be sent, and the reason
	// does not pass with a later sign-in: it goes once it is old enough.
	unsendable := func(o outcome) (outcome, error) {
		if spoolExpired(f) {
			drop()
			return outExpired, nil
		}
		return o, nil
	}
	raw, err := os.ReadFile(base + metaExt)
	if err != nil {
		return outSkipped, nil
	}
	var m Meta
	if json.Unmarshal(raw, &m) != nil || !validRecordingID(m.RecordingID) || m.URL == "" {
		return unsendable(outSkipped)
	}
	tok, usableURL := token(m.URL)
	if tok == "" {
		if !usableURL {
			// A URL that does not parse can never have a token: the meta file
			// cannot be used, and the spool goes once it is old enough.
			return unsendable(outSkipped)
		}
		// No usable login right now: none stored, expired, a trust store that
		// was not accepted, or a keychain that cannot be opened. None of that
		// is durable, so the spool waits however old it is.
		return outSkipped, nil
	}
	c := newClient(m.URL, tok)
	st, err := f.Stat()
	if err != nil {
		return outPending, err
	}
	up := &Uploader{C: c, RecordingID: m.RecordingID, Spool: f, Base: m.SpoolBase, Prefix: m.Prefix}
	acked := max(m.Acked, m.SpoolBase)
	// The probe's offset is a recording offset; the meta file's is a spool
	// position.
	size, _ := up.size()
	up.Offset = min(int64(len(m.Prefix))+(acked-m.SpoolBase), size)

	switch perr := up.Probe(ctx); {
	case perr == nil:
		// The recording is this login's and open. The open call is idempotent
		// for the run key, and it is made only now, to learn the sizes the
		// deployment wants for this recording.
		resp, err := c.Open(ctx, m.Open)
		if err != nil {
			return outPending, err // includes ErrAuth
		}
		if !resp.Recorded || resp.RecordingID != m.RecordingID {
			return outPending, errors.New("recording: the deployment's answer changed during the flush")
		}
		up.MaxBatch, up.WindowBytes = resp.MaxBatchBytes, resp.WindowBytes
	case errors.Is(perr, ErrRecordingClosed):
		// This login's recording was closed while no recorder was there.
		// Continue in a new one from the last position known to be taken,
		// unless that is the end of the spool.
		if acked >= st.Size() {
			drop() // everything was taken before the close
			return outSent, nil
		}
		if err := continueRun(ctx, up, &m, base, acked); err != nil {
			return flushStopped(err, drop)
		}
	case errors.Is(perr, ErrNotOwner):
		return unsendable(outNotOurs)
	case errors.Is(perr, ErrAuth):
		return outAuth, ErrAuth
	default:
		return outPending, perr
	}

	// Save the position the deployment holds as the flush advances, so a flush
	// that is cut short, and the one after it, do not send the same frames
	// again into a continuation.
	up.Progress = func() {
		if pos := up.SpoolPos(up.Offset); pos > m.Acked {
			m.Acked = pos
			_ = writeMeta(base+metaExt, m)
		}
	}
	req := CloseRequest{EndReason: m.EndReason, ExitCode: m.ExitCode, Signal: m.Signal}
	if !m.Ended {
		// The recorder died before it could say how the run ended.
		req = CloseRequest{EndReason: "recorder_error", ExitCode: nil}
	}
	for {
		err := up.Close(ctx, req)
		if errors.Is(err, ErrRecordingClosed) {
			if err := continueRun(ctx, up, &m, base, up.SpoolPos(up.Offset)); err != nil {
				return flushStopped(err, drop)
			}
			continue
		}
		if err != nil {
			if errors.Is(err, ErrFatal) {
				drop()
				return outDropped, nil
			}
			if errors.Is(err, ErrAuth) {
				return outAuth, ErrAuth
			}
			return outPending, fmt.Errorf("flush %s: %w", m.RunKey, err)
		}
		break
	}
	drop()
	return outSent, nil
}

// continueRun opens the recording that continues m's run from spool position
// acked, saves it in the meta file and points up at it. The terminal size is
// the one m was opened with, because a flush has no terminal.
func continueRun(ctx context.Context, up *Uploader, m *Meta, base string, acked int64) error {
	now := flushClock()
	wait, notBefore := Guard{}.plan(*m, now)
	if notBefore.After(now) {
		return errWaiting // a later flush opens it
	}
	n, resp, err := nextRecording(ctx, up.C, *m, acked, 0, 0, now, wait)
	if err != nil {
		return err
	}
	*m = n
	if werr := writeMeta(base+metaExt, n); werr != nil {
		return werr
	}
	up.Switch(resp.RecordingID, n.SpoolBase, n.Prefix, resp.MaxBatchBytes, resp.WindowBytes)
	return nil
}

// flushStopped settles a flush whose continuation could not be opened: a login
// refusal or a failure to reach the deployment leaves the spool for next time,
// any other answer means the run is not recorded and the spool goes.
func flushStopped(err error, drop func()) (outcome, error) {
	var u errUnrecorded
	if errors.As(err, &u) {
		drop()
		return outDropped, nil
	}
	if errors.Is(err, ErrAuth) {
		return outAuth, ErrAuth
	}
	return outPending, err
}
