// Package notice lays out the warnings brig prints as a block: a heading, then
// rows under it. It knows nothing about where a block goes. wrap.Notices
// prints one, with brig's prefix, and decides the spacing on a terminal.
//
// A leaf package so every package that words a warning can build one: wrap
// imports creds and verify, so neither could reach a layout kept in wrap, and
// each wrote the glyphs and the padding out by hand.
package notice

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The glyphs of a block, one meaning each.
const (
	noteMark    = "↳" // a note on the heading, or on the row above
	doMark      = "→" // something to type or do
	missingMark = "○" // a thing that has no value
)

// Block is a heading and the rows under it.
//
//	claude-code runs without 2 secrets
//	  ○ claude-credentials  → brig secret import claude-code
//	                          ↳ run `claude` on the host once to log in
//	  ○ gh-token            → brig secret create gh-token
//
//	the guest home of claude-code is temporary
//	  ↳ `brig rm claude-code` deletes it and every file in it
//	  → to keep your work, share a project:  brig run claude-code <dir>
//	  → to keep the guest home:              brig run claude-code --home <dir>
type Block struct {
	heading string
	rows    []row
}

type row struct {
	mark string
	// left is the text of a note, the label of an action, or the name of a
	// missing thing. right is what to type, if anything.
	left, right string
	// under are notes on this row, lined up under right.
	under []string
}

// New starts a block with its heading.
func New(heading string) *Block { return &Block{heading: heading} }

// Newf is New with the heading formatted.
func Newf(format string, a ...any) *Block { return New(fmt.Sprintf(format, a...)) }

// Note adds a line about the heading.
func (b *Block) Note(format string, a ...any) *Block {
	b.rows = append(b.rows, row{mark: noteMark, left: fmt.Sprintf(format, a...)})
	return b
}

// Do adds something to do: label says what it is for, and input is what to
// type. With no input the label is the whole instruction.
func (b *Block) Do(label, input string) *Block {
	b.rows = append(b.rows, row{mark: doMark, left: label, right: input})
	return b
}

// Missing adds a thing with no value, the command that gives it one, and
// notes on that command.
func (b *Block) Missing(name, command string, hints ...string) *Block {
	b.rows = append(b.rows, row{mark: missingMark, left: name, right: command, under: hints})
	return b
}

// Heading is the block's first line.
func (b *Block) Heading() string { return b.heading }

// Notes are the lines of the block's notes on the heading, for a caller that
// prints the heading somewhere else.
func (b *Block) Notes() []string {
	var out []string
	for _, r := range b.rows {
		if r.mark == noteMark {
			out = append(out, r.left)
		}
	}
	return out
}

// String lays the block out: every row indented under the heading, and the
// inputs of the rows of one kind in one column, so the eye finds them.
func (b *Block) String() string {
	width := map[string]int{}
	for _, r := range b.rows {
		if r.right != "" {
			width[r.mark] = max(width[r.mark], utf8.RuneCountInString(r.left))
		}
	}
	var s strings.Builder
	s.WriteString(b.heading)
	for _, r := range b.rows {
		s.WriteString("\n  " + r.mark + " ")
		switch {
		case r.right == "":
			s.WriteString(r.left)
		case r.mark == doMark:
			fmt.Fprintf(&s, "%-*s  %s", width[r.mark]+1, r.left+":", r.right)
		default:
			fmt.Fprintf(&s, "%-*s  %s %s", width[r.mark], r.left, doMark, r.right)
		}
		// Four columns of "  ○ " and four of "  → " around the padded name:
		// the column the command starts at, so a note lines up under it.
		under := strings.Repeat(" ", 4+width[r.mark]+4)
		for _, u := range r.under {
			s.WriteString("\n" + under + noteMark + " " + u)
		}
	}
	return s.String()
}

// Prose is the block as one run of sentences, for a place that prints a line
// rather than a block: doctor's row, a refusal quoting it.
func (b *Block) Prose() string {
	out := b.heading
	for _, r := range b.rows {
		if r.mark == noteMark {
			out += ". " + upperFirst(r.left)
		}
	}
	return out
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[n:]
}
