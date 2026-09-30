package wrap

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/brig-sh/brig/internal/runtime"
)

// spinnerFrames are the braille frames of briandowns/spinner's set 14. They are
// written out here, so brig takes no dependency for them.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerTick is how often the frame turns.
var spinnerTick = 100 * time.Millisecond

// spinSignals are the signals that end brig while a frame is on screen. The
// spinner clears the frame and raises the signal again, so brig handles it as
// it would have. Without that, the shell prompt lands after the frame.
var spinSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// The signal calls Spin makes, as variables so a test can hand it a signal
// without delivering one to the test binary.
var (
	notifySpin  = signal.Notify
	stopSpin    = signal.Stop
	spinIgnored = signal.Ignored
	reraiseSpin = func(s os.Signal) {
		if sig, ok := s.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), sig)
		}
	}
)

// inForeground is foreground, as a variable so a test can answer for a
// pseudo-terminal that is not the test binary's controlling terminal.
var inForeground = foreground

// canSpin returns whether a spinner may draw on f: a terminal whose TERM names
// one that understands the escape codes, with brig in its foreground process
// group. A TERM of dumb, or none, may print the codes as text. A job in the
// background would redraw over the line the user is typing on.
func canSpin(f *os.File) bool {
	term := os.Getenv("TERM")
	return term != "" && term != "dumb" && IsTerminal(f) && inForeground(f.Fd())
}

// spinner is the notice writer for a terminal at the default level. It draws a
// long operation on one line that redraws in place, and clears the line when
// the operation ends. See runtime.Spinner.
//
// Nothing else writes to the terminal while a frame is up: the runtime adapter
// spins only around a child process it waits on.
type spinner struct {
	w io.Writer
	// cols returns the terminal's width, or 0 when it is not known.
	cols func() int
	// fg returns whether brig is in the terminal's foreground process group.
	// The spinner draws and clears only while it is, so a run sent to the
	// background with Ctrl-Z and bg stops writing over the shell. nil means
	// always.
	fg func() bool
	mu sync.Mutex
	// drawn is whether a frame is on screen now.
	drawn bool
}

var _ runtime.Spinner = (*spinner)(nil)

// newSpinner returns a spinner drawing on the terminal f.
func newSpinner(f *os.File) *spinner {
	return &spinner{
		w:    f,
		cols: func() int { return termWidth(f.Fd()) },
		fg:   func() bool { return inForeground(f.Fd()) },
	}
}

// owns returns whether the spinner may write to the terminal now.
func (s *spinner) owns() bool { return s.fg == nil || s.fg() }

// Write passes a plain notice through. A frame on screen is cleared first, so
// the notice gets a line of its own and the next frame goes below it.
func (s *spinner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	return s.w.Write(p)
}

// Spin draws what behind a turning frame until stop is called. The first frame
// is on screen when Spin returns. stop clears the line and returns once no more
// frames will be drawn. A second call to stop does nothing.
func (s *spinner) Spin(what string) (stop func()) {
	frame := func(i int) string { return spinnerFrames[i%len(spinnerFrames)] + " " + what + "..." }
	// Registered before the first frame, so a signal that ends brig finds a
	// handler whenever a frame is up. A signal brig was started with ignored
	// is left out: Notify would stop it being ignored. Notify with no signals
	// relays every signal, so with all of them ignored Spin asks for none.
	var watch []os.Signal
	for _, sig := range spinSignals {
		if !spinIgnored(sig) {
			watch = append(watch, sig)
		}
	}
	sigs := make(chan os.Signal, 1)
	if len(watch) > 0 {
		notifySpin(sigs, watch...)
	}
	s.draw(frame(0))
	quit, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		tick := time.NewTicker(spinnerTick)
		defer tick.Stop()
		var caught os.Signal
	spin:
		for i := 1; ; i++ {
			select {
			case <-quit:
				break spin
			case caught = <-sigs:
				break spin
			case <-tick.C:
				s.draw(frame(i))
			}
		}
		s.erase()
		stopSpin(sigs)
		// A signal that arrives as stop is called can lose the select to quit.
		// Once Stop returns nothing more reaches sigs, so one already there is
		// read here and still ends brig.
		if caught == nil {
			select {
			case caught = <-sigs:
			default:
			}
		}
		if caught != nil {
			reraiseSpin(caught)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			<-gone
		})
	}
}

// draw replaces the line the cursor is on with line, cut to the terminal's
// width, and leaves the cursor at its end.
func (s *spinner) draw(line string) {
	if !s.owns() {
		return
	}
	width := 0
	if s.cols != nil {
		width = s.cols()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprint(s.w, "\r\033[K"+fit(line, width))
	s.drawn = true
}

// erase clears a frame on screen.
func (s *spinner) erase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
}

// clear erases a frame on screen. The caller holds mu. In the background the
// cursor is on the shell's line, so the frame is left where it is.
func (s *spinner) clear() {
	if !s.drawn {
		return
	}
	s.drawn = false
	if s.owns() {
		_, _ = fmt.Fprint(s.w, "\r\033[K")
	}
}

// fit returns line cut to one column less than cols, so a frame never wraps
// onto a second row, which "\r\033[K" would leave behind. Every rune of a frame
// takes one column. A cols of 0 or 1 leaves the line whole.
func fit(line string, cols int) string {
	if cols < 2 {
		return line
	}
	r := []rune(line)
	if len(r) < cols {
		return line
	}
	return string(r[:cols-1])
}
