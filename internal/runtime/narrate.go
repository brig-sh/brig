package runtime

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// narrationLimit is how much of a child process's output brig keeps while it
// runs.
//
// An image pull writes a progress bar per layer and can print megabytes of it,
// so what is held has to be bounded -- and it is the END that is kept. A tool
// that printed progress and then died says why on its last few lines; the
// first 64K of a progress bar is the half that explains nothing.
const narrationLimit = 64 << 10

// narration is where a child process's own output goes.
//
// brig used to hand the runtime its own stderr, so every pull progress bar and
// boot message landed in the middle of brig's output whether or not anyone
// wanted to read it. It is held instead: streamed live only when the caller
// asked to see it, and otherwise quoted back on the error, where it is the
// evidence for why a boot failed. Dropping it altogether would make a broken
// boot unreportable.
//
// It is deliberately not an io.Writer somebody can hand around: a narration
// belongs to one child process, and explain is the other half of it.
type narration struct {
	live io.Writer
	buf  bytes.Buffer
}

// narrate collects a child's output, streaming it to live as well when live is
// non-nil. A nil live is the ordinary case: hold it, and say nothing unless
// something goes wrong.
func narrate(live io.Writer) *narration { return &narration{live: live} }

func (n *narration) Write(p []byte) (int, error) {
	if n.live != nil {
		// Best effort. A terminal that has gone away is not a reason to fail
		// the boot that was writing to it.
		_, _ = n.live.Write(p)
	}
	n.buf.Write(p)
	// Trimmed in blocks rather than on every write, so a chatty child costs one
	// copy per 64K written instead of one per line.
	if n.buf.Len() > 2*narrationLimit {
		kept := append([]byte(nil), n.buf.Bytes()[n.buf.Len()-narrationLimit:]...)
		n.buf.Reset()
		n.buf.Write(kept)
	}
	return len(p), nil
}

// explain attaches what the child said to the error it died with, and returns
// the error unchanged when it said nothing.
//
// Only when the output was not already streamed: a caller that asked to see it
// live has seen it, and quoting the same lines again at the bottom of the
// message reads like a second failure rather than the same one.
func (n *narration) explain(err error) error {
	if err == nil || n.live != nil {
		return err
	}
	said := lastLines(strings.TrimRight(n.buf.String(), "\n"), 10)
	if strings.TrimSpace(said) == "" {
		return err
	}
	return fmt.Errorf("%w\n%s", err, indent(said))
}

// lastLines is the final n lines of s, which is where a tool that ran for a
// while and then died says why. firstLines beside it is for a command that
// fails immediately and prints one line about it.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// indent puts the child's words under brig's, so a multi-line quotation reads
// as a quotation rather than as more of brig's own sentence.
func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

// noticef writes one of brig's own lines about a long operation: that it has
// started, and that it is done.
//
// It carries brig's prefix because it is brig's voice rather than the tool's --
// the tool's own words go to Progress. A nil writer is silence, which is what a
// caller that wants neither hands in.
func noticef(w io.Writer, format string, a ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "brig: "+format+"\n", a...)
}
