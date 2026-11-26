// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

//go:build darwin || linux

package ptyrelay

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// outer is a test-owned terminal standing in for the user's: the relay gets the
// slave as In and Out, the test reads and writes the master.
type outer struct {
	master, slave *os.File
	mu            sync.Mutex
	got           bytes.Buffer
}

func newOuter(t *testing.T, rows, cols uint16) *outer {
	t.Helper()
	m, s, err := OpenPty()
	if err != nil {
		t.Fatal(err)
	}
	o := &outer{master: m, slave: s}
	t.Cleanup(func() { m.Close(); s.Close() })
	if err := unix.IoctlSetWinsize(int(m.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := m.Read(buf)
			if n > 0 {
				o.mu.Lock()
				o.got.Write(buf[:n])
				o.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *outer) bytes() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.got.Bytes()...)
}

func (o *outer) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(o.bytes(), []byte(s)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outer terminal never showed %q; got %q", s, o.bytes())
}

func (o *outer) termios(t *testing.T) unix.Termios {
	t.Helper()
	tm, err := unix.IoctlGetTermios(int(o.slave.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	return *tm
}

// safeBuf is a Tap that keeps the output, and notes drops and resizes.
type safeBuf struct {
	mu      sync.Mutex
	b       bytes.Buffer
	drops   []int64
	dropPos []int
	sizes   [][2]uint16
	delay   time.Duration
}

func (s *safeBuf) Output(_ time.Time, p []byte) {
	if d := s.delay; d > 0 {
		s.delay = 0 // only the first call is slow: the queue backs up, then drains
		time.Sleep(d)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
}
func (s *safeBuf) Dropped(_ time.Time, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops = append(s.drops, n)
	s.dropPos = append(s.dropPos, s.b.Len())
}
func (s *safeBuf) Resize(_ time.Time, c, r uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, [2]uint16{c, r})
}
func (s *safeBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func run(t *testing.T, o *outer, tee Tap, name string, args ...string) (Result, error) {
	t.Helper()
	return Run(exec.Command(name, args...), Options{In: o.slave, Out: o.slave, Tap: tee})
}

// stripCR makes a pty's ONLCR output comparable to what the command wrote.
func stripCR(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func TestOutputReachesOuterAndTeeUnchanged(t *testing.T) {
	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	res, err := run(t, o, tee, "sh", "-c", `printf '\033[31mred\033[0m \033[?1049h\033[2J\033[?2004h\033[?1000h\n'`)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := "\x1b[31mred\x1b[0m \x1b[?1049h\x1b[2J\x1b[?2004h\x1b[?1000h\r\n"
	if got := tee.Bytes(); string(got) != want {
		t.Errorf("tee = %q, want %q", got, want)
	}
	if got := o.bytes(); string(got) != want {
		t.Errorf("outer = %q, want %q", got, want)
	}
	if res.DroppedBytes != 0 || res.OutputBytes != int64(len(want)) {
		t.Errorf("counts: %+v", res)
	}
}

func TestOneMiBBurst(t *testing.T) {
	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	// 1 MiB of the byte pattern 0..250 with no newline, so ONLCR leaves it alone.
	res, err := run(t, o, tee, "sh", "-c", `head -c 1048576 /dev/zero | tr '\0' 'x'`)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := bytes.Repeat([]byte("x"), 1<<20)
	deadline := time.Now().Add(10 * time.Second)
	for len(o.bytes()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.Equal(tee.Bytes(), want) {
		t.Errorf("tee: %d bytes, equal=false (dropped %d)", len(tee.Bytes()), res.DroppedBytes)
	}
	if !bytes.Equal(o.bytes(), want) {
		t.Errorf("outer: %d bytes", len(o.bytes()))
	}
}

func TestResizeAtStartAndOnSIGWINCH(t *testing.T) {
	o := newOuter(t, 30, 100)
	tee := &safeBuf{}
	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		defer close(done)
		res, err = run(t, o, tee, "sh", "-c", `stty size; read x; stty size`)
	}()
	o.waitFor(t, "30 100")
	if err := unix.IoctlSetWinsize(int(o.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 41, Col: 133}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := o.master.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	<-done
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	o.waitFor(t, "41 133")
	if !strings.Contains(string(tee.Bytes()), "41 133") {
		t.Errorf("tee lacks the resized size: %q", tee.Bytes())
	}
}

func TestExitStatus(t *testing.T) {
	o := newOuter(t, 24, 80)
	res, err := run(t, o, nil, "sh", "-c", "exit 3")
	if err != nil || res.ExitCode != 3 || res.Signal != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSignalDeathAndForwardedSIGTERM(t *testing.T) {
	o := newOuter(t, 24, 80)
	res, err := run(t, o, nil, "sh", "-c", `kill -KILL $$`)
	if err != nil || res.Signal != syscall.SIGKILL || res.ExitCode != 137 {
		t.Fatalf("self-kill: res=%+v err=%v", res, err)
	}
	go func() {
		o.waitFor(t, "ready")
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	res, err = run(t, o, nil, "sh", "-c", `echo ready; exec sleep 30`)
	if err != nil || res.Signal != syscall.SIGTERM || res.ExitCode != 143 {
		t.Fatalf("forwarded SIGTERM: res=%+v err=%v", res, err)
	}
}

func TestCtrlCIsDeliveredOnceByTheInnerTerminal(t *testing.T) {
	o := newOuter(t, 24, 80)
	// The command counts SIGINTs; two deliveries would print 2.
	go func() {
		o.waitFor(t, "ready")
		_, _ = o.master.Write([]byte{0x03})
		time.Sleep(300 * time.Millisecond)
		_, _ = o.master.Write([]byte("q\n"))
	}()
	res, err := run(t, o, nil, "sh", "-c", `n=0; trap 'n=$((n+1))' INT; echo ready; read x; echo count=$n`)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	o.waitFor(t, "count=1")
}

func TestTerminalModeRestored(t *testing.T) {
	o := newOuter(t, 24, 80)
	before := o.termios(t)

	duringCh := make(chan unix.Termios, 1)
	go func() {
		o.waitFor(t, "ready")
		duringCh <- o.termios(t)
		_, _ = o.master.Write([]byte("\n"))
	}()
	if _, err := run(t, o, nil, "sh", "-c", `echo ready; read x`); err != nil {
		t.Fatal(err)
	}
	during := <-duringCh
	if during.Lflag&unix.ICANON != 0 || during.Lflag&unix.ECHO != 0 {
		t.Errorf("outer terminal was not raw during the run: lflag=%#x", during.Lflag)
	}
	if after := o.termios(t); after != before {
		t.Errorf("mode after exit differs:\nbefore %+v\nafter  %+v", before, after)
	}

	// Error path: the command cannot start.
	if _, err := run(t, o, nil, "/nonexistent/brig-spike"); err == nil {
		t.Fatal("expected a start error")
	}
	if after := o.termios(t); after != before {
		t.Errorf("mode after a start error differs")
	}

	// Error path: nonzero exit.
	if res, err := run(t, o, nil, "sh", "-c", "exit 9"); err != nil || res.ExitCode != 9 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if after := o.termios(t); after != before {
		t.Errorf("mode after a failing command differs")
	}
}

func TestTypedInputWithEchoOffIsNotInTee(t *testing.T) {
	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	go func() {
		o.waitFor(t, "ready")
		_, _ = o.master.Write([]byte("hunter2-secret\n"))
	}()
	res, err := run(t, o, tee, "sh", "-c", `stty -echo; echo ready; read x; echo done`)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if bytes.Contains(tee.Bytes(), []byte("hunter2")) || bytes.Contains(o.bytes(), []byte("hunter2")) {
		t.Errorf("echo-off input leaked: tee=%q outer=%q", tee.Bytes(), o.bytes())
	}
	if !bytes.Contains(stripCR(tee.Bytes()), []byte("done\n")) {
		t.Errorf("tee = %q", tee.Bytes())
	}
}

func TestEchoedInputAppearsInOutputAsExpected(t *testing.T) {
	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	go func() {
		o.waitFor(t, "ready")
		_, _ = o.master.Write([]byte("visible\n"))
	}()
	if _, err := run(t, o, tee, "sh", "-c", `echo ready; read x; echo done`); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(tee.Bytes(), []byte("visible")) {
		t.Errorf("echoed input should be in the tee, as a terminal shows it: %q", tee.Bytes())
	}
}

// A slow tap must not stall the command or the user's terminal, and must be
// told where output was lost.
func TestSlowTapDropsInsteadOfStallingAndSaysWhere(t *testing.T) {
	o := newOuter(t, 24, 80)
	tap := &safeBuf{delay: 400 * time.Millisecond}
	start := time.Now()
	res, err := Run(exec.Command("sh", "-c", `head -c 4194304 /dev/zero | tr '\0' 'x'; echo END`),
		Options{In: o.slave, Out: o.slave, Tap: tap, QueueBytes: 64 << 10})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if el := time.Since(start); el > 8*time.Second {
		t.Errorf("relay stalled behind the tap: %v", el)
	}
	if res.DroppedBytes == 0 {
		t.Fatalf("expected drops: %+v", res)
	}
	var sum int64
	tap.mu.Lock()
	for _, n := range tap.drops {
		sum += n
	}
	got := int64(tap.b.Len())
	tap.mu.Unlock()
	if sum != res.DroppedBytes || got+sum != res.OutputBytes {
		t.Errorf("drops %d (result %d) + delivered %d != output %d", sum, res.DroppedBytes, got, res.OutputBytes)
	}
	t.Logf("elapsed %v dropped %d of %d", time.Since(start), res.DroppedBytes, res.OutputBytes)
}

func TestTapSeesInitialAndChangedSizeOnly(t *testing.T) {
	o := newOuter(t, 30, 100)
	tap := &safeBuf{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = run(t, o, tap, "sh", "-c", `echo ready; read x`)
	}()
	o.waitFor(t, "ready")
	_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH) // no change: no event
	time.Sleep(100 * time.Millisecond)
	_ = unix.IoctlSetWinsize(int(o.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120})
	_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	time.Sleep(200 * time.Millisecond)
	_, _ = o.master.Write([]byte("\n"))
	<-done
	tap.mu.Lock()
	defer tap.mu.Unlock()
	if len(tap.sizes) != 2 || tap.sizes[0] != [2]uint16{100, 30} || tap.sizes[1] != [2]uint16{120, 40} {
		t.Errorf("sizes = %v", tap.sizes)
	}
}

func TestCommandStartsWithTheUsersTerminalSettings(t *testing.T) {
	o := newOuter(t, 24, 80)
	tm, err := unix.IoctlGetTermios(int(o.slave.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	tm.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(o.slave.Fd()), setTermios, tm); err != nil {
		t.Fatal(err)
	}
	tap := &safeBuf{}
	if _, err := run(t, o, tap, "sh", "-c", `stty -a | tr ' ' '\n' | grep -x -e -echo -e echo`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tap.Bytes()), "-echo") {
		t.Errorf("command did not inherit echo off: %q", tap.Bytes())
	}
}

func TestNoTerminalOnInput(t *testing.T) {
	// In is a pipe, not a terminal: no raw mode, EOF on input must not hang.
	o := newOuter(t, 24, 80)
	pr, pw, _ := os.Pipe()
	go func() { _, _ = pw.WriteString("abc\n"); pw.Close() }()
	tee := &safeBuf{}
	res, err := Run(exec.Command("sh", "-c", `read x; echo got-$x`), Options{In: pr, Out: o.slave, Tap: tee})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !bytes.Contains(tee.Bytes(), []byte("got-abc")) {
		t.Errorf("tee = %q", tee.Bytes())
	}
}

// When the person's stderr is not a terminal, the command's stderr stays that
// file, byte for byte: it is not on the pseudo-terminal, not shown on the
// person's terminal and not given to the tap.
func TestStderrThatIsNotATerminalStaysThePersonsFile(t *testing.T) {
	o := newOuter(t, 24, 80)
	errf, err := os.Create(t.TempDir() + "/err.log")
	if err != nil {
		t.Fatal(err)
	}
	defer errf.Close()
	tee := &safeBuf{}
	res, err := Run(exec.Command("sh", "-c", `echo to-out; echo to-err >&2; [ -t 2 ] && echo err-tty`),
		Options{In: o.slave, Out: o.slave, Err: errf, Tap: tee})
	if err != nil || res.ExitCode != 1 {
		t.Fatalf("res=%+v err=%v", res, err) // exit 1: stderr is not a terminal
	}
	if b, _ := os.ReadFile(errf.Name()); string(b) != "to-err\n" {
		t.Errorf("stderr file = %q, want the bytes as written", b)
	}
	if bytes.Contains(tee.Bytes(), []byte("to-err")) || bytes.Contains(o.bytes(), []byte("to-err")) {
		t.Errorf("stderr reached the tap or the terminal: tap %q terminal %q", tee.Bytes(), o.bytes())
	}
	if !bytes.Contains(tee.Bytes(), []byte("to-out")) {
		t.Errorf("stdout was not recorded: %q", tee.Bytes())
	}
}

// A stderr that is the same terminal as stdout goes on the pseudo-terminal.
func TestStderrThatIsTheSameTerminalGoesOnThePty(t *testing.T) {
	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	res, err := Run(exec.Command("sh", "-c", `echo to-err >&2; [ -t 2 ]`),
		Options{In: o.slave, Out: o.slave, Err: o.slave, Tap: tee})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !bytes.Contains(tee.Bytes(), []byte("to-err")) {
		t.Errorf("tap = %q", tee.Bytes())
	}
}

// A signal the process inherited as ignored stays ignored while the relay runs.
func TestAnIgnoredSignalStaysIgnored(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	o := newOuter(t, 24, 80)
	seen := make(chan bool, 1)
	go func() {
		o.waitFor(t, "ready")
		seen <- signal.Ignored(syscall.SIGHUP)
		_, _ = o.master.Write([]byte("\n"))
	}()
	if _, err := run(t, o, nil, "sh", "-c", `echo ready; read x`); err != nil {
		t.Fatal(err)
	}
	if !<-seen {
		t.Error("SIGHUP was no longer ignored while the relay ran")
	}
	if !signal.Ignored(syscall.SIGHUP) {
		t.Error("SIGHUP was not ignored after the relay ended")
	}
}

// A stderr that is a different terminal, as in `brig run x 2>/dev/ttys005`, is
// the person's own: it reaches that terminal, not the session's, and is not
// recorded.
func TestStderrOnAnotherTerminalIsLeftAloneAndNotRecorded(t *testing.T) {
	session := newOuter(t, 24, 80)
	other := newOuter(t, 24, 80)
	tee := &safeBuf{}
	res, err := Run(exec.Command("sh", "-c", `echo to-out; echo to-other >&2`),
		Options{In: session.slave, Out: session.slave, Err: other.slave, Tap: tee})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	other.waitFor(t, "to-other")
	if bytes.Contains(session.bytes(), []byte("to-other")) || bytes.Contains(tee.Bytes(), []byte("to-other")) {
		t.Errorf("stderr reached the session or the recording: session %q tap %q", session.bytes(), tee.Bytes())
	}
	if !bytes.Contains(tee.Bytes(), []byte("to-out")) {
		t.Errorf("stdout was not recorded: %q", tee.Bytes())
	}
}

func TestSameTerminalComparesDevices(t *testing.T) {
	a := newOuter(t, 24, 80)
	b := newOuter(t, 24, 80)
	dup, err := os.OpenFile(a.slave.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dup.Close()
	file, _ := os.Create(t.TempDir() + "/f")
	defer file.Close()
	for name, tc := range map[string]struct {
		x, y *os.File
		want bool
	}{
		"one terminal twice":        {a.slave, a.slave, true},
		"two opens of one terminal": {a.slave, dup, true},
		"two terminals":             {a.slave, b.slave, false},
		"a terminal and a file":     {a.slave, file, false},
		"two files":                 {file, file, false},
		"nil":                       {nil, a.slave, false},
	} {
		if got := SameTerminal(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// Ctrl-Z typed in a recorded session does not stop the command: the person's
// terminal is raw, so the byte goes to the pseudo-terminal, and the signal its
// line discipline raises is discarded because the command's process group is
// orphaned.
func TestCtrlZDoesNotStopTheCommand(t *testing.T) {
	o := newOuter(t, 24, 80)
	go func() {
		o.waitFor(t, "ready")
		_, _ = o.master.Write([]byte{0x1a})
		time.Sleep(300 * time.Millisecond)
		_, _ = o.master.Write([]byte("q\n"))
	}()
	type done struct {
		res Result
		err error
	}
	ch := make(chan done, 1)
	go func() {
		res, err := run(t, o, nil, "sh", "-c", `echo ready; read x; echo finished`)
		ch <- done{res, err}
	}()
	select {
	case d := <-ch:
		if d.err != nil || d.res.ExitCode != 0 {
			t.Fatalf("res=%+v err=%v", d.res, d.err)
		}
		o.waitFor(t, "finished")
	case <-time.After(10 * time.Second):
		t.Fatal("the command stopped on Ctrl-Z")
	}
}

// ignoredHelper is set in the environment of the copy of this test binary that
// TestInheritedIgnoresStayIgnoredForHangupAndInterrupt starts.
const ignoredHelper = "PTYRELAY_IGNORED_HELPER"

// A process started with SIGHUP, SIGINT, SIGQUIT and SIGTERM ignored keeps SIGHUP
// and SIGINT ignored while the relay runs. SIGQUIT and SIGTERM are not reported
// as ignored by Go even when inherited that way, since Go installs a handler
// for them at start, so the relay takes them, as an unrecorded run's exec does
// not carry an ignore for them either.
func TestInheritedIgnoresStayIgnoredForHangupAndInterrupt(t *testing.T) {
	if os.Getenv(ignoredHelper) != "" {
		master, slave, err := OpenPty()
		if err != nil {
			t.Fatal(err)
		}
		defer master.Close()
		var got string
		sampled := make(chan struct{})
		go func() {
			time.Sleep(150 * time.Millisecond)
			for _, s := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM} {
				got += fmt.Sprintf("%v=%v ", int(s), signal.Ignored(s))
			}
			close(sampled)
		}()
		_, _ = Run(exec.Command("sleep", "0.5"), Options{In: slave, Out: slave})
		<-sampled
		_ = os.WriteFile(os.Getenv(ignoredHelper), []byte(got), 0o600)
		return
	}
	result := t.TempDir() + "/result"
	cmd := exec.Command("sh", "-c", `trap '' HUP INT QUIT TERM; exec "$0" -test.run='^TestInheritedIgnoresStayIgnoredForHangupAndInterrupt$'`, os.Args[0])
	cmd.Env = append(os.Environ(), ignoredHelper+"="+result)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(result)
	want := fmt.Sprintf("%d=true %d=true %d=false %d=false ", int(syscall.SIGHUP), int(syscall.SIGINT), int(syscall.SIGQUIT), int(syscall.SIGTERM))
	if string(b) != want {
		t.Errorf("during the run: %q, want %q", b, want)
	}
}

// A failure before the command starts is reported as ErrNotStarted, leaves the
// person's terminal as it was and runs nothing, so the caller can run the
// command without the relay.
func TestARelayThatCannotSetUpReportsNotStartedAndTouchesNothing(t *testing.T) {
	o := newOuter(t, 24, 80)
	before := o.termios(t)
	marker := t.TempDir() + "/ran"
	for name, setup := range map[string]func(t *testing.T){
		"no pseudo-terminal": func(t *testing.T) {
			old := openPty
			openPty = func() (*os.File, *os.File, error) { return nil, nil, errors.New("injected") }
			t.Cleanup(func() { openPty = old })
		},
		"a command that cannot start": func(t *testing.T) {},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			c := exec.Command("sh", "-c", "touch "+marker)
			if name == "a command that cannot start" {
				c = exec.Command("/nonexistent/brig-relay")
			}
			_, err := Run(c, Options{In: o.slave, Out: o.slave})
			if !errors.Is(err, ErrNotStarted) {
				t.Fatalf("err = %v, want ErrNotStarted", err)
			}
			if _, serr := os.Stat(marker); serr == nil {
				t.Error("the command ran")
			}
			if after := o.termios(t); after != before {
				t.Error("the person's terminal was changed")
			}
		})
	}
}

// The relay's wake pipes are for the relay. The command, and any other child
// this process starts, must not inherit them.
//
// The test process can itself hold descriptors that every child inherits (a CI
// runner passes some down). So the test does not name descriptor numbers. It
// lists the descriptors of the same command started without the relay, and
// requires the relay's command to hold nothing beyond that. A descriptor that
// is inheritable is opened first, so the case holds on any machine.
func TestTheCommandDoesNotInheritTheWakePipes(t *testing.T) {
	// F_DUPFD returns a descriptor that is not close-on-exec, above any number
	// the command or ls opens for itself.
	inherited, err := unix.FcntlInt(0, unix.F_DUPFD, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(inherited)
	if flags, err := unix.FcntlInt(uintptr(inherited), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("descriptor %d should be inheritable: flags %d, err %v", inherited, flags, err)
	}

	list := func(out []byte) map[string]bool {
		set := map[string]bool{}
		for _, fd := range strings.Fields(string(out)) {
			set[fd] = true
		}
		return set
	}

	// The baseline: any child of this process, with no relay.
	baseOut, err := exec.Command("sh", "-c", "ls /dev/fd").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := list(baseOut)
	if !base[fmt.Sprint(inherited)] {
		t.Fatalf("the baseline child does not hold the inherited descriptor %d: %v", inherited, base)
	}

	o := newOuter(t, 24, 80)
	tee := &safeBuf{}
	if _, err := run(t, o, tee, "sh", "-c", "ls /dev/fd"); err != nil {
		t.Fatal(err)
	}
	got := list(stripCR(tee.Bytes()))
	if len(got) < 3 {
		t.Errorf("the listing %v is not the command's descriptors", got)
	}
	// 0, 1 and 2 are the pty here and pipes or files in the baseline, but they
	// are always there. ls's own directory descriptor is the lowest free number
	// in both, so it is in the baseline too. A leaked descriptor either is
	// outside the baseline or pushes ls's directory descriptor outside it, and
	// either way the set differs.
	for fd := range got {
		if fd != "0" && fd != "1" && fd != "2" && !base[fd] {
			t.Errorf("the command holds descriptor %s that a child without the relay does not; relay %v, baseline %v", fd, got, base)
		}
	}
	if len(got) > len(base) {
		t.Errorf("the command holds %d descriptors, a child without the relay %d; relay %v, baseline %v", len(got), len(base), got, base)
	}
}

// A write to the command's terminal that has no room is woken by the stop, so
// Run can always return and restore the person's terminal. The master stands in
// as a pipe nobody reads, filled until it takes no more.
func TestPumpInputStopsWhileTheCommandTakesNoInput(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()
	mR, mW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer mR.Close()
	defer mW.Close()
	wR, wW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer wR.Close()
	defer wW.Close()
	go func() { _, _ = inW.Write(bytes.Repeat([]byte("x"), 1<<20)) }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpInput(int(inR.Fd()), int(mW.Fd()), int(wR.Fd()))
	}()
	time.Sleep(200 * time.Millisecond) // the pipe is full and the writer waits
	select {
	case <-done:
		t.Fatal("pumpInput ended before it was woken")
	default:
	}
	_, _ = unix.Write(int(wW.Fd()), []byte{1})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pumpInput stayed blocked in a write after the wake")
	}
}
