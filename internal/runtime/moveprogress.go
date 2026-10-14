package runtime

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// A move takes seconds to minutes, most of it hull copying the guest to the
// other host, and hull says how that goes on stderr as it goes. brig used to
// collect those lines until hull exited, so a person watching `brig teleport`
// saw nothing for the length of the copy, and a copy that stalled looked the
// same as one that ran. callLines hands each line on as hull writes it.

// maxSaid bounds what a streamed call keeps of hull's stderr for its error.
// A move prints a few lines a second at most; the reason it stopped is in
// its first lines or on an `error:` line, which hullReason picks out.
const maxSaid = 1 << 20

// maxLine is the longest line callLines hands on whole. hull's lines are
// short; a longer one is cut there rather than stopping the read, which
// would leave hull blocked on a full pipe.
const maxLine = 64 << 10

// callLines is call for a hull command whose stderr says how it is going:
// each line goes to each as hull writes it, and the call's error is the one
// call would make of the same output. A carriage return ends a line too, so
// a line hull rewrites in place on a terminal still arrives as it changes.
func (h *hull) callLines(args []string, each func(string), drop ...string) (string, error) {
	cmd := h.command(args, drop)
	var out bytes.Buffer
	cmd.Stdout = &out
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return "", h.failed(args, err, "")
	}
	if err := cmd.Start(); err != nil {
		return "", h.failed(args, err, "")
	}
	var said strings.Builder
	readLines(pipe, func(line string) {
		if said.Len() < maxSaid {
			said.WriteString(line)
			said.WriteByte('\n')
		}
		if each != nil {
			each(line)
		}
	})
	err = h.failed(args, cmd.Wait(), said.String())
	return out.String(), err
}

// readLines hands each non-empty line of r to each, up to the end of r. A
// line is ended by a newline or a carriage return, and one past maxLine is
// handed on in pieces. It reads r to its end whatever each does.
func readLines(r io.Reader, each func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), maxLine)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		if len(data) >= maxLine {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			each(line)
		}
	}
	// A read error ends the lines, not the command: what is left goes
	// nowhere, so hull is not left blocked writing it.
	_, _ = io.Copy(io.Discard, r)
}
