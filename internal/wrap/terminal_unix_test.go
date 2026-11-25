//go:build darwin || linux

package wrap

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/brig-sh/brig/internal/ttytest"
)

func TestTermWidthReadsTheTerminal(t *testing.T) {
	_, tty := ttytest.Pair(t)
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 37}); err != nil {
		t.Fatalf("set the pseudo-terminal's size: %v", err)
	}
	if got := termWidth(tty.Fd()); got != 37 {
		t.Errorf("termWidth = %d on a 37-column terminal", got)
	}
	if got := newSpinner(tty).cols(); got != 37 {
		t.Errorf("the spinner sees %d columns on a 37-column terminal", got)
	}

	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got := termWidth(file.Fd()); got != 0 {
		t.Errorf("termWidth = %d on a file, want 0", got)
	}
}

func TestTerminalSizeReadsColumnsAndRows(t *testing.T) {
	_, tty := ttytest.Pair(t)
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 31, Col: 97}); err != nil {
		t.Fatalf("set the pseudo-terminal's size: %v", err)
	}
	if cols, rows := TerminalSize(tty); cols != 97 || rows != 31 {
		t.Errorf("TerminalSize = %dx%d on a 97x31 terminal", cols, rows)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if cols, rows := TerminalSize(file); cols != 0 || rows != 0 {
		t.Errorf("TerminalSize = %dx%d on a file, want 0x0", cols, rows)
	}
	if cols, rows := TerminalSize(nil); cols != 0 || rows != 0 {
		t.Errorf("TerminalSize = %dx%d on nil, want 0x0", cols, rows)
	}
}

// The driver answers for the controlling terminal alone. The test binary's
// pseudo-terminal is not one, and neither is a file.
func TestForegroundAnswersOnlyForTheControllingTerminal(t *testing.T) {
	_, tty := ttytest.Pair(t)
	if foreground(tty.Fd()) {
		t.Error("foreground on a terminal this process does not control")
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if foreground(file.Fd()) {
		t.Error("foreground on a file")
	}
}
