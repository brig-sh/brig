package wrap

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
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

// Say prints msg: one line, or a block when msg has more than one.
func (n *Notices) Say(msg string) {
	lines := strings.Split(msg, "\n")
	block := len(lines) > 1
	if !n.isTerminal() {
		for _, line := range lines {
			fmt.Fprintf(n.out(), "brig: %s\n", line)
		}
		return
	}
	n.part(block)
	fmt.Fprintf(n.out(), "brig: %s\n", lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintln(n.out(), line)
	}
}

// Error prints the error a command ended with. Its lines stay as the error
// wrote them, with the prefix on the first alone, on a terminal or not: an
// error is quoted whole into bug reports and scripts, and its wording is its
// own. It only takes part in the spacing, so it does not run into a block.
func (n *Notices) Error(msg string) {
	if n.isTerminal() {
		n.part(true)
	}
	fmt.Fprintln(n.out(), "brig: "+msg)
}

// part puts a blank line on the terminal between a block and the warning next
// to it, before or after. Warnings that are one line each stay together.
func (n *Notices) part(block bool) {
	if n.warned && (block || n.lastWasBlock) {
		fmt.Fprintln(n.out())
	}
	n.warned, n.lastWasBlock = true, block
}

// isTerminal returns whether W is a terminal that a person reads. brigd hands
// a buffer, and a pipe or a TERM of dumb is a reader that wants every line to
// stand alone.
func (n *Notices) isTerminal() bool {
	f, ok := n.out().(*os.File)
	if !ok || n.NoTerminal {
		return false
	}
	term := os.Getenv("TERM")
	return term != "" && term != "dumb" && IsTerminal(f)
}

// notices is the Notices for Err: the shared Stderr when Err is the process's
// stderr, and one of the Config's own otherwise.
func (c *Config) notices() *Notices {
	if c.Err == os.Stderr && !c.NoTerminal {
		return Stderr
	}
	if c.ownNotices == nil || c.ownNotices.W != c.Err || c.ownNotices.NoTerminal != c.NoTerminal {
		c.ownNotices = &Notices{W: c.Err, NoTerminal: c.NoTerminal}
	}
	return c.ownNotices
}

// Rows builds the rows of a block. Note is a line about the heading, Do a
// command to type, labelled with what it does. The commands line up in one
// column, so the eye finds them:
//
//	↳ `brig rm claude-code` deletes it and every file in it
//	→ to keep your work, share a project:  brig run claude-code <dir>
//	→ to keep the guest home:              brig run claude-code --home <dir>
//
// ↳ marks a note and → something to do, the same two glyphs in every block.
type Rows struct {
	rows []row
}

type row struct {
	note         string
	label, input string
}

// Note adds a line about the heading.
func (r *Rows) Note(format string, a ...any) *Rows {
	r.rows = append(r.rows, row{note: fmt.Sprintf(format, a...)})
	return r
}

// Do adds something to do. input is what to type, and may be empty when the
// label says it all.
func (r *Rows) Do(label, input string) *Rows {
	r.rows = append(r.rows, row{label: label, input: input})
	return r
}

// Block returns the heading and the rows as one message for Say.
func (r *Rows) Block(heading string) string {
	width := 0
	for _, x := range r.rows {
		if x.note == "" && x.input != "" {
			width = max(width, utf8.RuneCountInString(x.label))
		}
	}
	lines := []string{heading}
	for _, x := range r.rows {
		switch {
		case x.note != "":
			lines = append(lines, "  ↳ "+x.note)
		case x.input == "":
			lines = append(lines, "  → "+x.label)
		default:
			pad := strings.Repeat(" ", width-utf8.RuneCountInString(x.label))
			lines = append(lines, fmt.Sprintf("  → %s:%s  %s", x.label, pad, x.input))
		}
	}
	return strings.Join(lines, "\n")
}
