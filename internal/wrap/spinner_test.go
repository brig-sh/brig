package wrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/brig-sh/brig/internal/ttytest"
)

// said reads what the spinner wrote, under the lock its frames are drawn with.
func (s *spinner) said() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.(*bytes.Buffer).String()
}

func TestTheSpinnerClearsItsLineWhenStopped(t *testing.T) {
	s := &spinner{w: &bytes.Buffer{}}
	stop := s.Spin("pulling img")
	stop()
	stop()

	got := s.said()
	if !strings.Contains(got, "\r\033[K⠋ pulling img...") {
		t.Errorf("no frame was drawn: %q", got)
	}
	if !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("the line was not cleared: %q", got)
	}
	time.Sleep(3 * spinnerTick)
	if s.said() != got {
		t.Errorf("a frame was drawn after stop returned: %q", s.said())
	}
}

func TestTheSpinnerTurns(t *testing.T) {
	old := spinnerTick
	spinnerTick = time.Millisecond
	t.Cleanup(func() { spinnerTick = old })

	s := &spinner{w: &bytes.Buffer{}}
	stop := s.Spin("pulling img")
	defer stop()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(s.said(), "⠙ pulling img...") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the frame never turned: %q", s.said())
}

func TestAPlainNoticeGetsALineOfItsOwn(t *testing.T) {
	s := &spinner{w: &bytes.Buffer{}}
	stop := s.Spin("pulling img")
	_, _ = s.Write([]byte("brig: something else\n"))
	stop()

	if got := s.said(); !strings.Contains(got, "\r\033[Kbrig: something else\n") {
		t.Errorf("the notice shared a line with a frame: %q", got)
	}
}

func TestTheNoticeSpinsOnlyOnATerminalAtTheDefaultLevel(t *testing.T) {
	_, tty := ttytest.Pair(t)
	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// The pseudo-terminal is not the test binary's controlling terminal, so
	// the driver cannot say which group is in front. Each case answers.
	old := inForeground
	t.Cleanup(func() { inForeground = old })

	for _, tc := range []struct {
		name       string
		out        *os.File
		level      Verbosity
		term       string
		fg         bool
		noTerminal bool
		spins      bool
	}{
		{"terminal", tty, Normal, "xterm-256color", true, false, true},
		{"terminal under -q", tty, Quiet, "xterm-256color", true, false, false},
		{"terminal under --verbose", tty, Verbose, "xterm-256color", true, false, false},
		{"dumb terminal", tty, Normal, "dumb", true, false, false},
		{"terminal with no TERM", tty, Normal, "", true, false, false},
		{"job in the background", tty, Normal, "xterm-256color", false, false, false},
		{"caller with no terminal of its own", tty, Normal, "xterm-256color", true, true, false},
		{"file", file, Normal, "xterm-256color", true, false, false},
	} {
		t.Setenv("TERM", tc.term)
		fg := tc.fg
		inForeground = func(uintptr) bool { return fg }
		c := &Config{Progress: tc.out, Verbosity: tc.level, NoTerminal: tc.noTerminal}
		_, spins := c.runtimeNotice().(*spinner)
		if spins != tc.spins {
			t.Errorf("%s: spinner %v, want %v", tc.name, spins, tc.spins)
		}
	}
}

// A run sent to the background with Ctrl-Z and bg shares the terminal with
// the shell. A frame drawn or cleared there lands on the line being typed.
func TestABackgroundedSpinnerLeavesTheTerminalAlone(t *testing.T) {
	// No tick fires, so every write below is one the test makes happen.
	old := spinnerTick
	spinnerTick = time.Hour
	t.Cleanup(func() { spinnerTick = old })

	var front atomic.Bool
	front.Store(true)
	s := &spinner{w: &bytes.Buffer{}, fg: front.Load}
	stop := s.Spin("pulling img")
	if s.said() == "" {
		t.Fatal("no frame was drawn in the foreground")
	}
	front.Store(false)
	before := s.said()
	s.draw("⠙ pulling img...")
	stop()

	if got := s.said(); got != before {
		t.Errorf("a spinner in the background wrote %q", strings.TrimPrefix(got, before))
	}
}

// A frame one column too wide wraps, and the next "\r\033[K" clears only the
// second row, so every tick would leave a row behind.
func TestAFrameFitsTheTerminal(t *testing.T) {
	s := &spinner{w: &bytes.Buffer{}, cols: func() int { return 12 }}
	s.Spin("pulling ghcr.io/brig-sh/claude-code-stock:root")()

	for _, frame := range strings.Split(s.said(), "\r\033[K") {
		if n := utf8.RuneCountInString(frame); n > 11 {
			t.Errorf("a %d-column frame was drawn on a 12-column terminal: %q", n, frame)
		}
	}
}

func TestFit(t *testing.T) {
	for _, tc := range []struct {
		line string
		cols int
		want string
	}{
		{"⠋ pulling img...", 0, "⠋ pulling img..."},
		{"⠋ pulling img...", 1, "⠋ pulling img..."},
		{"⠋ pulling img...", 80, "⠋ pulling img..."},
		{"⠋ pulling img...", 17, "⠋ pulling img..."},
		{"⠋ pulling img...", 16, "⠋ pulling img.."},
		{"⠋ pulling img...", 5, "⠋ pu"},
	} {
		if got := fit(tc.line, tc.cols); got != tc.want {
			t.Errorf("fit(%q, %d) = %q, want %q", tc.line, tc.cols, got, tc.want)
		}
	}
}

// stubSpinSignals hands Spin a channel the test can deliver signals on, and
// reports what Spin raised again.
func stubSpinSignals(t *testing.T, ignored bool) (delivered func() chan<- os.Signal, raised <-chan os.Signal) {
	t.Helper()
	oldNotify, oldStop, oldIgnored, oldReraise := notifySpin, stopSpin, spinIgnored, reraiseSpin
	t.Cleanup(func() {
		notifySpin, stopSpin, spinIgnored, reraiseSpin = oldNotify, oldStop, oldIgnored, oldReraise
	})
	var ch chan<- os.Signal
	notifySpin = func(c chan<- os.Signal, _ ...os.Signal) { ch = c }
	stopSpin = func(chan<- os.Signal) {}
	spinIgnored = func(os.Signal) bool { return ignored }
	r := make(chan os.Signal, 1)
	reraiseSpin = func(s os.Signal) { r <- s }
	return func() chan<- os.Signal { return ch }, r
}

// Ctrl-C mid-spin must not leave a frame for the shell prompt to land after,
// and brig must still die of the signal.
func TestASignalClearsTheFrameAndIsRaisedAgain(t *testing.T) {
	delivered, raised := stubSpinSignals(t, false)
	s := &spinner{w: &bytes.Buffer{}}
	stop := s.Spin("pulling img")
	delivered() <- syscall.SIGINT

	select {
	case got := <-raised:
		if got != syscall.SIGINT {
			t.Errorf("raised %v again, want SIGINT", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the signal was not raised again")
	}
	stop()
	if got := s.said(); !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("the frame was left on screen: %q", got)
	}
}

// A Ctrl-C at the moment the pull ends can lose the select to stop. The stub
// Stop queues a signal the way a delivery just before Stop returns does.
func TestASignalAtStopStillEndsBrig(t *testing.T) {
	delivered, raised := stubSpinSignals(t, false)
	stopSpin = func(chan<- os.Signal) {
		select {
		case delivered() <- syscall.SIGINT:
		default:
		}
	}
	s := &spinner{w: &bytes.Buffer{}}
	s.Spin("pulling img")()

	select {
	case got := <-raised:
		if got != syscall.SIGINT {
			t.Errorf("raised %v again, want SIGINT", got)
		}
	default:
		t.Error("a signal that arrived as the spinner stopped was dropped")
	}
}

// A signal between the first frame and Notify would end brig with the frame
// on screen.
func TestTheSignalHandlerIsInPlaceBeforeTheFirstFrame(t *testing.T) {
	stubSpinSignals(t, false)
	s := &spinner{w: &bytes.Buffer{}}
	drawnAtNotify := "unset"
	notifySpin = func(chan<- os.Signal, ...os.Signal) { drawnAtNotify = s.said() }
	s.Spin("pulling img")()
	if drawnAtNotify != "" {
		t.Errorf("a frame was on screen before the signal handler: %q", drawnAtNotify)
	}
}

// Notify would stop an ignored signal being ignored, so a brig started under
// nohup must not start dying of SIGHUP while it spins.
func TestAnIgnoredSignalIsLeftAlone(t *testing.T) {
	delivered, _ := stubSpinSignals(t, true)
	s := &spinner{w: &bytes.Buffer{}}
	s.Spin("pulling img")()
	if delivered() != nil {
		t.Error("Spin asked for signals that were all ignored")
	}
}
