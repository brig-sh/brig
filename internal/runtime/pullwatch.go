package runtime

import (
	"bytes"
	"io"
	"strings"
)

// pullWatch reads hull's stderr during `hull run` and announces an image pull
// on the run's notice writer.
//
// hull decides whether a run pulls. It looks the reference up in its store by
// tag or digest, for the platform asked, and counts only a complete unpack.
// brig learns of a pull from the progress hull prints while it runs one
// (pkg/ociclient/progress.go in hull):
//
//	pull: layer 1/3, 12.0 MB extracted (image 480.2 MB compressed)
//	pull: 3/3 layers, 1.1 GB extracted
//
// The first "pull: " line starts the notice and the summary line ends it. A
// hull that prints neither leaves the run silent, as it was before.
type pullWatch struct {
	notice io.Writer
	ref    string
	// line is the start of the line being written, which is all read needs.
	line []byte
	end  func(ok bool)
	over bool
}

// pullLineHead bounds what pullWatch keeps of one line. The first few words
// tell a progress line apart, and a long line on hull's stderr must not grow
// the buffer.
const pullLineHead = 64

func (p *pullWatch) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 && !p.over {
		i := bytes.IndexByte(b, '\n')
		chunk := b
		if i >= 0 {
			chunk = b[:i]
		}
		if room := pullLineHead - len(p.line); room > 0 {
			p.line = append(p.line, chunk[:min(room, len(chunk))]...)
		}
		if i < 0 {
			break
		}
		p.read(string(p.line))
		p.line = p.line[:0]
		b = b[i+1:]
	}
	return n, nil
}

// read acts on one line of hull's stderr.
func (p *pullWatch) read(line string) {
	rest, ok := strings.CutPrefix(line, "pull: ")
	if !ok {
		return
	}
	if p.end == nil {
		p.end = announce(p.notice, "pulling "+p.ref, p.ref+" pulled")
	}
	// "pull: layer i/n, ..." is a pull in flight. The summary line is the
	// other shape hull prints, "pull: n/n layers, ...".
	if !strings.HasPrefix(rest, "layer ") {
		p.finish(true)
	}
}

// finish ends the notice if a pull started it. Run calls it once hull exits,
// with whether the run succeeded. A nil pullWatch does nothing.
func (p *pullWatch) finish(ok bool) {
	if p == nil || p.over {
		return
	}
	p.over = true
	if p.end != nil {
		p.end(ok)
	}
}
