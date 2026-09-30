package wrap

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// warnBlock is warnf for a notice of more than one line: a heading, then rows
// indented under it.
//
// Off a terminal every line carries brig's prefix, as warnf's lines do, so a
// line copied out of a log or read back by brigd's client still says whose it
// is. On a terminal only the heading does: the rows sit under it as one block,
// and a blank line parts it from the block before. A prefix on every row of a
// list is noise to a reader who can see where the block starts.
func (c *Config) warnBlock(block string) {
	if c.Verbosity < Normal {
		return
	}
	lines := strings.Split(block, "\n")
	if !c.errIsTerminal() {
		for _, line := range lines {
			fmt.Fprintf(c.Err, "brig: %s\n", line)
		}
		return
	}
	c.partFromBlock(true)
	fmt.Fprintf(c.Err, "brig: %s\n", lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintln(c.Err, line)
	}
}

// partFromBlock puts a blank line on the terminal between a block and
// whatever warning comes next to it, before or after, so each block reads as
// one unit. Warnings that are one line each stay together. block says whether
// the warning about to print is a block. Off a terminal it does nothing: there
// every line already stands alone.
func (c *Config) partFromBlock(block bool) {
	if !c.errIsTerminal() {
		return
	}
	if c.warned && (block || c.lastWasBlock) {
		fmt.Fprintln(c.Err)
	}
	c.warned, c.lastWasBlock = true, block
}

// errIsTerminal returns whether Err is a terminal that a person reads. brigd
// hands a buffer, and a pipe or a TERM of dumb is a reader that wants every
// line to stand alone.
func (c *Config) errIsTerminal() bool {
	f, ok := c.Err.(*os.File)
	if !ok || c.NoTerminal {
		return false
	}
	term := os.Getenv("TERM")
	return term != "" && term != "dumb" && IsTerminal(f)
}

// choices lays out the rows of a block that each pair a label with what to
// type, with the commands in one column so the eye finds them:
//
//	→ to keep your work, share a project:  brig run claude-code <dir>
//	→ to keep the guest home:              brig run claude-code --home <dir>
func choices(rows [][2]string) []string {
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r[0]))
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(r[0]))
		out = append(out, fmt.Sprintf("  → %s:%s  %s", r[0], pad, r[1]))
	}
	return out
}
