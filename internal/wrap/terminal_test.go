package wrap

import (
	"os"
	"testing"

	"github.com/brig-sh/brig/internal/ttytest"
)

// /dev/null is a character device, so the old mode-based check called it a
// terminal. Every decision that hangs on IsTerminal then made the wrong one
// for `brig run < /dev/null`: it put a confirmation to a file that answers
// nothing, and it counted a redirected stdin as somebody sitting at a prompt.
func TestIsTerminalRejectsADeviceThatIsNotATerminal(t *testing.T) {
	for _, path := range []string{os.DevNull, "/dev/zero"} {
		f, err := os.Open(path)
		if err != nil {
			t.Skipf("cannot open %s: %v", path, err)
		}
		if IsTerminal(f) {
			t.Errorf("%s was reported as a terminal", path)
		}
		_ = f.Close()
	}
}

// A regular file is the other half of the same question, and the one a test
// suite hits constantly.
func TestIsTerminalRejectsARegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Error("a regular file was reported as a terminal")
	}
}

// And the positive case, against a real pseudo-terminal rather than a stand-in
// for one: a check that answers no to everything would pass the two tests
// above and leave brig unable to ask a question at all.
func TestIsTerminalAcceptsARealTerminal(t *testing.T) {
	_, slave := ttytest.Pair(t)
	if !IsTerminal(slave) {
		t.Error("a real terminal was not reported as one")
	}
}
