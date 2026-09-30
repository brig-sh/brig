package wrap

import (
	"io"
	"os"
	"strings"
)

// Notices prints brig's own warnings to one writer: a line, or a block of a
// heading and rows under it.
//
// Off a terminal every line carries brig's prefix, so a line copied out of a
// log or read back by brigd's client still says whose it is. On a terminal only
// a block's heading does: the rows sit under it as one unit, and a blank line
// parts a block from the warnings around it. A prefix on every row of a list is
// noise to a reader who can see where the block starts.
//
// One Notices per writer, because the spacing depends on what the writer
// already shows. cmd/brig and a Config both write to stderr in one run, and
// both use Stderr for it, so a line from one after a block from the other is
// still parted from it.
type Notices struct {
	// W is where the warnings go. nil is the process's stderr, read at each
	// write, so a test that swaps os.Stderr sees them.
	W io.Writer
	// NoTerminal says W has no person reading it, whatever it is. See
	// Config.NoTerminal.
	NoTerminal bool
	// warned and lastWasBlock are what the spacing needs to know about the
	// warnings already on the terminal: whether there are any, and whether
	// the last one was a block.
	warned, lastWasBlock bool
}

// Stderr is the Notices for the process's own stderr, shared by every writer
// of it. See Notices.
var Stderr = &Notices{}

// out is W, or the process's stderr when W is nil.
func (n *Notices) out() io.Writer {
	if n.W == nil {
		return os.Stderr
	}
	return n.W
}

// Say prints msg: one line, or a block when msg has more than one. A block
// goes out in one write, so it does not interleave with a child's output.
func (n *Notices) Say(msg string) {
	lines := strings.Split(msg, "\n")
	var b strings.Builder
	if !n.isTerminal() {
		for _, line := range lines {
			b.WriteString("brig: " + line + "\n")
		}
	} else {
		n.part(&b, len(lines) > 1)
		b.WriteString("brig: " + msg + "\n")
	}
	_, _ = io.WriteString(n.out(), b.String())
}

// Error prints the error a command ended with. Its lines stay as the error
// wrote them, with the prefix on the first alone, on a terminal or not: an
// error is quoted whole into bug reports and scripts, and its wording is its
// own. It only takes part in the spacing, so it does not run into a block.
func (n *Notices) Error(msg string) {
	var b strings.Builder
	if n.isTerminal() {
		n.part(&b, true)
	}
	b.WriteString("brig: " + msg + "\n")
	_, _ = io.WriteString(n.out(), b.String())
}

// Ask puts a yes-or-no question after the warnings, parted from a block above
// it like any other line, and leaves the cursor after it for the answer.
func (n *Notices) Ask(question string) {
	var b strings.Builder
	if n.isTerminal() {
		n.part(&b, false)
	}
	b.WriteString("brig: " + question + " [y/N] ")
	_, _ = io.WriteString(n.out(), b.String())
}

// part puts a blank line on the terminal between a block and the line next to
// it, before or after. Lines that are one each stay together.
func (n *Notices) part(b *strings.Builder, block bool) {
	if n.warned && (block || n.lastWasBlock) {
		b.WriteString("\n")
	}
	n.warned, n.lastWasBlock = true, block
}

// isTerminal returns whether the writer is a terminal that a person reads.
// brigd hands a buffer, and a pipe or a TERM of dumb is a reader that wants
// every line to stand alone.
func (n *Notices) isTerminal() bool {
	f, ok := n.out().(*os.File)
	return ok && !n.NoTerminal && readable(f)
}

// notices is the Notices for Err: the shared Stderr when Err is the process's
// stderr, and the Config's own otherwise.
func (c *Config) notices() *Notices {
	if c.Err == os.Stderr && !c.NoTerminal {
		return Stderr
	}
	c.own.W, c.own.NoTerminal = c.Err, c.NoTerminal
	return &c.own
}
