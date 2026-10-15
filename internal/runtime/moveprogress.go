package runtime

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
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
//
// each runs on the goroutine that reads hull's stderr, and hull waits on a
// full pipe while it runs. It must not block: one that prints hands the line
// to an Echo.
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

// echoQueue is how many lines an Echo holds for a printer that has fallen
// behind. A move prints a few lines a second, so a printer this far behind
// has stopped, and lines past it are counted rather than kept.
const echoQueue = 4096

// Echo prints lines on a goroutine of its own, so that whoever hands it a
// line never waits on the printing. hull writes its stderr in the pause, the
// last rounds among it, and the goroutine that reads that pipe is where
// Progress runs. A Progress that printed there would tie hull to brig's own
// stderr: a terminal held by a scroll lock, or a reader that stopped, would
// stop brig reading the pipe, and hull, blocked on a full pipe, would hold
// the guest paused for as long. An Echo that falls behind drops lines and
// counts them.
type Echo struct {
	print   func(string)
	lines   chan string
	done    chan struct{}
	dropped atomic.Int64
}

// NewEcho starts an Echo that hands each line to print, in order.
func NewEcho(print func(string)) *Echo {
	e := &Echo{print: print, lines: make(chan string, echoQueue), done: make(chan struct{})}
	go func() {
		defer close(e.done)
		for line := range e.lines {
			e.print(line)
		}
	}()
	return e
}

// Say queues line for printing, or counts it when the queue is full. It
// never blocks. It is not to be called after Stop.
func (e *Echo) Say(line string) {
	select {
	case e.lines <- line:
	default:
		e.dropped.Add(1)
	}
}

// Stop prints what is queued, then how many lines were dropped, if any, and
// returns when that is printed: a caller printing after Stop prints after
// every line its Echo was given.
func (e *Echo) Stop() {
	close(e.lines)
	<-e.done
	if n := e.dropped.Load(); n > 0 {
		e.print(fmt.Sprintf("%d of hull's lines were not shown: brig's stderr did not take them as fast "+
			"as hull printed them", n))
	}
}
