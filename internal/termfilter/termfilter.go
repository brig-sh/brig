// Package termfilter strips terminal control sequences from text brig reads back
// from a sandbox, so that text cannot drive the terminal it is printed on.
package termfilter

import (
	"bytes"
	"io"
)

// New returns a writer that passes text on to w without its control sequences.
func New(w io.Writer) io.Writer { return &filter{w: w} }

// Strip runs complete text through the filter.
func Strip(in []byte) []byte {
	var buf bytes.Buffer
	_, _ = New(&buf).Write(in)
	return buf.Bytes()
}

// filterState is where the control-sequence filter is in a sequence it has
// started to recognise. Held on the writer rather than reset per Write so a
// sequence split across two writes -- which streaming makes ordinary -- is
// still filtered whole.
type filterState int

const (
	fText    filterState = iota // ordinary text
	fEsc                        // just saw ESC
	fCSI                        // in ESC [ ... , waiting for the final byte
	fOSC                        // in ESC ] ... , waiting for BEL or ST
	fOSCEsc                     // in an OSC and saw ESC, waiting for the \ of ST
	fCharset                    // ESC ( or ) , which consumes one more byte
)

// filter strips terminal control sequences from a log as it passes
// through. A runtime replays into its log whatever a program wrote, cursor moves
// and colour and title-setting included, and a log meant to be read back is
// text, not a terminal to drive.
//
// Newline, tab and carriage return are kept: they are the layout of the text,
// not control of a terminal. Every other C0 control and DEL is dropped.
type filter struct {
	w  io.Writer
	st filterState
}

func (f *filter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		// A newline is no part of any sequence here, so it ends whichever one is
		// open and stays. An OSC a program was killed in the middle of writing
		// would otherwise take the rest of the log with it.
		if b == '\n' && f.st != fText {
			f.st = fText
			out = append(out, b)
			continue
		}
		switch f.st {
		case fText:
			switch {
			case b == 0x1b:
				f.st = fEsc
			case b == '\n' || b == '\t' || b == '\r':
				out = append(out, b)
			case b < 0x20 || b == 0x7f:
				// A C0 control or DEL with no place in read-back text.
			default:
				out = append(out, b)
			}
		case fEsc:
			switch b {
			case '[':
				f.st = fCSI
			case ']':
				f.st = fOSC
			case '(', ')':
				f.st = fCharset
			default:
				// A lone ESC-something, e.g. ESC c reset: drop the pair.
				f.st = fText
			}
		case fCSI:
			// The final byte of a CSI is in 0x40-0x7e; the bytes before it are
			// parameters and intermediates.
			if b >= 0x40 && b <= 0x7e {
				f.st = fText
			}
		case fOSC:
			switch b {
			case 0x07: // BEL terminates an OSC
				f.st = fText
			case 0x1b: // ESC may begin the ST that terminates it
				f.st = fOSCEsc
			}
		case fOSCEsc:
			// ESC \ is the string terminator; anything else was ESC inside the
			// OSC body, and either way the sequence is over as far as text goes.
			f.st = fText
		case fCharset:
			f.st = fText
		}
	}
	if _, err := f.w.Write(out); err != nil {
		return 0, err
	}
	// The whole input was consumed, sequences included, so report it fully
	// written: a short count with no error is what makes io.Copy report
	// ErrShortWrite, and this writer drops bytes by design.
	return len(p), nil
}
