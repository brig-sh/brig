// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

//go:build darwin || linux

package ptyrelay

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	chunk       = 32 << 10
	drainIdle   = 50 * time.Millisecond
	teeFlushMax = 2 * time.Second // how long close waits for a slow Tap
)

// Run starts cmd with a new pseudo-terminal as its stdin, stdout and controlling
// terminal, and relays until cmd ends. cmd.Stdin, Stdout, Stderr and
// SysProcAttr are replaced.
//
// The command's stderr is on the pseudo-terminal only when opt.Err is the same
// terminal device as opt.Out. Otherwise the command's stderr is opt.Err itself,
// a file, a pipe or another terminal the person chose: it is neither relayed
// nor given to the Tap.
//
// While running, opt.In is in raw mode when it is a terminal and is restored on
// every return path. A ^C typed by the user is the byte 0x03 in raw mode; it
// goes to the pseudo-terminal, whose line discipline signals the command, so
// Run does not signal the command for it. A SIGINT, SIGQUIT, SIGTERM or SIGHUP
// sent to this process (which a keyboard cannot cause while In is raw) is
// forwarded to the command. SIGHUP and SIGINT that this process inherited as
// ignored are not taken: they stay ignored, and the command inherits that. The
// command is the leader of its own session, so a signal sent to this process's
// group does not reach it directly, and the forward is its only delivery.
func Run(cmd *exec.Cmd, opt Options) (Result, error) {
	in, out := opt.In, opt.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	errOut := opt.Err
	if errOut == nil {
		errOut = os.Stderr
	}

	master, slave, err := OpenPty()
	if err != nil {
		return Result{}, err
	}
	defer master.Close()
	slaveOpen := true
	defer func() {
		if slaveOpen {
			slave.Close()
		}
	}()

	inFd, outFd := int(in.Fd()), int(out.Fd())
	sizeFd := -1
	switch {
	case isTTY(outFd):
		sizeFd = outFd
	case isTTY(inFd):
		sizeFd = inFd
	}
	mfd := int(master.Fd())
	tap := newTap(opt.Tap, opt.QueueBytes)
	var lastCols, lastRows uint16
	copySize := func() {
		if sizeFd < 0 {
			return
		}
		ws, err := unix.IoctlGetWinsize(sizeFd, unix.TIOCGWINSZ)
		if err != nil {
			return
		}
		_ = unix.IoctlSetWinsize(mfd, unix.TIOCSWINSZ, ws)
		if ws.Col != 0 && ws.Row != 0 && (ws.Col != lastCols || ws.Row != lastRows) {
			lastCols, lastRows = ws.Col, ws.Row
			tap.resize(time.Now(), ws.Col, ws.Row)
		}
	}
	// The command's terminal starts with the user's terminal's settings, not
	// the kernel's defaults, so line editing, echo and flow control behave as
	// they do in the user's own terminal.
	if isTTY(inFd) {
		if tm, err := unix.IoctlGetTermios(inFd, getTermios); err == nil {
			_ = unix.IoctlSetTermios(int(slave.Fd()), setTermios, tm)
		}
	}
	copySize()

	// Signals are registered before Start so none falls under the default
	// disposition in the gap.
	sigs := make(chan os.Signal, 8)
	// SIGHUP and SIGINT that this process inherited as ignored (nohup, a shell's
	// background job) are not taken, so they stay ignored and the command
	// inherits that. Go installs its own handler for SIGQUIT and SIGTERM when the
	// process starts, so signal.Ignored reports false for them even when they
	// were inherited as ignored, and the relay takes and forwards them. A run
	// without the relay behaves the same: the handler is reset at exec, so hull
	// gets the default action for both.
	signal.Notify(sigs, syscall.SIGWINCH)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)

	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// stderr goes on the pseudo-terminal only when it is the terminal stdout
	// is. Any other stderr, a different terminal included, is the person's own
	// and goes to the command untouched.
	if !SameTerminal(errOut, out) {
		cmd.Stderr = errOut
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	_ = slave.Close()
	slaveOpen = false

	restore := func() {}
	if isTTY(inFd) {
		if r, err := makeRaw(inFd); err == nil {
			restore = r
		}
	}
	defer func() { restore() }()

	var wakeIn, wakeOut [2]int
	if err := unix.Pipe(wakeIn[:]); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, err
	}
	if err := unix.Pipe(wakeOut[:]); err != nil {
		closeAll(wakeIn[:])
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, err
	}
	defer closeAll(wakeIn[:])
	defer closeAll(wakeOut[:])

	var wg sync.WaitGroup
	tee := tap

	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		pumpInput(inFd, mfd, wakeIn[0])
	}()

	var outBytes int64
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		outBytes = pumpOutput(mfd, wakeOut[0], out, tee)
	}()

	stopSigs := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGWINCH {
					copySize()
					continue
				}
				if sig, ok := s.(syscall.Signal); ok {
					_ = cmd.Process.Signal(sig)
				}
			case <-stopSigs:
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	// Wake the output pump so it drains what is left and stops, then stop input.
	_, _ = unix.Write(wakeOut[1], []byte{1})
	<-outDone
	_, _ = unix.Write(wakeIn[1], []byte{1})
	<-inDone
	close(stopSigs)
	wg.Wait()
	restore()
	restore = func() {}

	dropped := tee.close(time.Now())
	res := Result{OutputBytes: outBytes, DroppedBytes: dropped}
	return finish(res, waitErr)
}

// SameTerminal reports whether a and b are the same terminal device: both are
// terminals and their device numbers are equal. Two descriptors of one
// terminal, such as stdout and a 2>&1 stderr, are the same; two terminals are
// not.
func SameTerminal(a, b *os.File) bool {
	if a == nil || b == nil || !isTTY(int(a.Fd())) || !isTTY(int(b.Fd())) {
		return false
	}
	var sa, sb unix.Stat_t
	if unix.Fstat(int(a.Fd()), &sa) != nil || unix.Fstat(int(b.Fd()), &sb) != nil {
		return false
	}
	return sa.Rdev == sb.Rdev
}

func finish(res Result, err error) (Result, error) {
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal()
			res.ExitCode = 128 + int(ws.Signal())
			return res, nil
		}
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func closeAll(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}

func isTTY(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, getTermios)
	return err == nil
}

// makeRaw puts fd in raw mode and returns a function that restores the mode it
// had. The restore is idempotent.
func makeRaw(fd int) (func(), error) {
	old, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, setTermios, &raw); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = unix.IoctlSetTermios(fd, setTermios, old) })
	}, nil
}

// waitReadable blocks until fd or wake is readable. It reports which.
func waitReadable(fd, wake int, timeout *unix.Timeval) (fdReady, wakeReady bool, err error) {
	for {
		var set unix.FdSet
		set.Set(fd)
		max := fd
		if wake >= 0 {
			set.Set(wake)
			if wake > max {
				max = wake
			}
		}
		n, err := unix.Select(max+1, &set, nil, nil, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, false, err
		}
		if n == 0 {
			return false, false, nil
		}
		return set.IsSet(fd), wake >= 0 && set.IsSet(wake), nil
	}
}

func writeAll(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Write(fd, p)
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// pumpInput copies the user's terminal to the master until wake is readable or
// the user's input ends. It reads only when select says data is ready, so a
// stop never leaves it holding a byte the next reader needed.
func pumpInput(inFd, mfd, wake int) {
	buf := make([]byte, chunk)
	for {
		ready, woke, err := waitReadable(inFd, wake, nil)
		if err != nil || woke {
			return
		}
		if !ready {
			continue
		}
		n, err := unix.Read(inFd, buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			return
		}
		if writeAll(mfd, buf[:n]) != nil {
			return
		}
	}
}

// pumpOutput copies the master to out and tee. When wake fires (the command
// has exited) it keeps reading until the master reports the end or stays
// quiet for drainIdle, so output written just before exit is not lost and a
// grandchild holding the slave cannot hold the relay open.
func pumpOutput(mfd, wake int, out *os.File, tee *tapQueue) (total int64) {
	buf := make([]byte, chunk)
	draining := false
	outOK := true
	for {
		w := wake
		var tv *unix.Timeval
		if draining {
			w = -1
			t := unix.NsecToTimeval(int64(drainIdle))
			tv = &t
		}
		ready, woke, err := waitReadable(mfd, w, tv)
		if err != nil {
			return total
		}
		if woke {
			draining = true
		}
		if !ready {
			if draining && !woke {
				return total
			}
			continue
		}
		n, err := unix.Read(mfd, buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 { // EIO on linux, EOF on darwin: slave closed
			return total
		}
		total += int64(n)
		if outOK {
			if _, err := out.Write(buf[:n]); err != nil {
				// The user's terminal is gone. Keep reading so the command
				// does not block on a full pty; the tee still gets the bytes.
				outOK = false
			}
		}
		tee.output(time.Now(), buf[:n])
	}
}

// tapQueue hands events to the Tap on a goroutine of its own, so the relay
// never waits for it. Output is bounded: when the queue holds its byte limit,
// further output is dropped and counted, and the Tap is told how many bytes
// were lost before the next output it sees. Resizes are tiny and always kept.
type tapQueue struct {
	tap     Tap
	limit   int
	mu      sync.Mutex
	cond    *sync.Cond
	items   []tapItem
	queued  int
	pending int64 // dropped bytes not yet reported
	total   int64
	closed  bool
	done    chan struct{}
}

type tapItem struct {
	kind       byte // 'o', 'd', 'r'
	at         time.Time
	data       []byte
	n          int64
	cols, rows uint16
}

const defaultQueueBytes = 4 << 20

func newTap(t Tap, limit int) *tapQueue {
	q := &tapQueue{tap: t, limit: limit}
	if t == nil {
		return q
	}
	if q.limit <= 0 {
		q.limit = defaultQueueBytes
	}
	q.cond = sync.NewCond(&q.mu)
	q.done = make(chan struct{})
	go q.run()
	return q
}

func (q *tapQueue) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		batch := q.items
		q.items = nil
		q.mu.Unlock()
		for _, it := range batch {
			switch it.kind {
			case 'o':
				q.tap.Output(it.at, it.data)
			case 'd':
				q.tap.Dropped(it.at, it.n)
			case 'r':
				q.tap.Resize(it.at, it.cols, it.rows)
			}
			if it.kind == 'o' {
				q.mu.Lock()
				q.queued -= len(it.data)
				q.mu.Unlock()
			}
		}
	}
}

func (q *tapQueue) output(at time.Time, p []byte) {
	if q.tap == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued+len(p) > q.limit {
		q.pending += int64(len(p))
		q.total += int64(len(p))
		return
	}
	if q.pending > 0 {
		q.items = append(q.items, tapItem{kind: 'd', at: at, n: q.pending})
		q.pending = 0
	}
	c := make([]byte, len(p))
	copy(c, p)
	q.queued += len(c)
	q.items = append(q.items, tapItem{kind: 'o', at: at, data: c})
	q.cond.Signal()
}

func (q *tapQueue) resize(at time.Time, cols, rows uint16) {
	if q.tap == nil {
		return
	}
	q.mu.Lock()
	q.items = append(q.items, tapItem{kind: 'r', at: at, cols: cols, rows: rows})
	q.mu.Unlock()
	q.cond.Signal()
}

// close reports any drop still unreported, stops the Tap's goroutine after it
// has taken what is queued, waiting up to tapFlushMax, and returns the number
// of bytes dropped.
func (q *tapQueue) close(at time.Time) int64 {
	if q.tap == nil {
		return 0
	}
	q.mu.Lock()
	if q.pending > 0 {
		q.items = append(q.items, tapItem{kind: 'd', at: at, n: q.pending})
		q.pending = 0
	}
	q.closed = true
	total := q.total
	q.mu.Unlock()
	q.cond.Broadcast()
	select {
	case <-q.done:
	case <-time.After(teeFlushMax):
	}
	return total
}
