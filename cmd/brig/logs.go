package main

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/wrap"
)

// logsOptions is what `brig logs` was asked for.
type logsOptions struct {
	follow bool
	// tail is how many trailing lines to show, or -1 for all -- the default,
	// and what both runtimes read as "all".
	tail int
	raw  bool
}

// logsCmd streams a sandbox's log, the one thing no brig command pointed at
// before: after `brig run -d`, or a boot that never became ready, brig's only
// advice was to leave brig for `hull logs <sandbox>` or `nerdctl logs
// <sandbox>` -- which asks the reader for the sandbox name, which is not the
// ref, and which runtime is underneath, both of which brig has and they do not.
//
// It parses its own flags rather than going through the run line, because
// --gateway takes no ref: the gateway is one per host, and the run line's
// parser requires a ref to resolve a profile at all.
func logsCmd(args []string) error {
	o := logsOptions{tail: -1}
	var gateway bool
	ref := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--follow":
			o.follow = true
		case a == "--raw":
			o.raw = true
		case a == "--gateway":
			gateway = true
		case a == "--tail":
			i++
			if i >= len(args) {
				return usagef("--tail needs a number, for example `--tail 100`")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return usagef("--tail needs a number, not %q", args[i])
			}
			o.tail = n
		case strings.HasPrefix(a, "--tail="):
			raw := a[len("--tail="):]
			n, err := strconv.Atoi(raw)
			if err != nil {
				return usagef("--tail needs a number, not %q", raw)
			}
			o.tail = n
		case strings.HasPrefix(a, "-"):
			return usagef("unknown flag %q for `brig logs`", a)
		default:
			if ref != "" {
				return usagef("unexpected argument %q; `brig logs` takes one ref", a)
			}
			ref = a
		}
	}

	// The gateway log is per host, so it takes no ref -- and naming one is a
	// mistake to say, not a session to go and resolve.
	if gateway {
		if ref != "" {
			return usagef("`brig logs --gateway` names no session; the gateway is per host, "+
				"not per sandbox. Drop %q", ref)
		}
		return gatewayLogs(os.Stdout, o)
	}
	if ref == "" {
		return usagef("brig logs needs a ref, for example `brig logs claude`. " +
			"`brig logs --gateway` reads the host's gateway log instead")
	}

	// Resolved the way every other ref'd verb resolves: the ref names the agent
	// and the session, the profile picks the runtime, and Load derives the
	// sandbox name from the two.
	r, err := session.ParseRef(ref)
	if err != nil {
		// Classed as a usage error, the same way split classes the identical
		// ParseRef failure on the run line: nothing ran, so this is a token in
		// the wrong place rather than a run that started and failed.
		return usagef("%s", err)
	}
	t, ok := profile.Lookup(r.Agent)
	if !ok {
		return notFoundf("unknown profile %q. `brig agent ls` lists them", r.Agent)
	}
	rt, err := runtime.DetectFor(runtime.Preference{Bin: t.RuntimeBin})
	if err != nil {
		return err
	}
	cfg, err := wrap.Load(t, wrap.Options{Name: r.Label, Verbosity: verbosity}, rt)
	if err != nil {
		return err
	}
	return logsFor(rt, cfg.VMName, ref, o, os.Stdout)
}

// logsFor confirms the runtime has the sandbox before streaming its log.
//
// There is nothing to read from a sandbox that is not there, which the exit
// table calls a not-found (3) rather than a log stream that started and failed
// (1). So the runtime is asked before the stream is opened, and the ref the
// reader typed -- not the sandbox name -- is what the not-found names. A List
// that itself fails comes back as is (exit 4, the runtime unavailable), not as
// absence: the two are different facts. See sandboxPresent.
//
// Split from logsCmd so a test can drive it with a runtime double, the way
// streamLogs is split for the same reason.
func logsFor(rt runtime.Runtime, name, ref string, o logsOptions, out io.Writer) error {
	present, err := sandboxPresent(rt, name)
	if err != nil {
		return err
	}
	if !present {
		return noSandboxf(ref)
	}
	return streamLogs(rt, name, o, out)
}

// streamLogs hands the runtime the log request, filtering terminal control
// sequences on the way out unless the raw bytes were asked for.
//
// A function of its own, taking the runtime rather than reaching for it, so a
// test can hand it a double and assert that --follow and --tail reach the
// adapter with the values the line carried.
func streamLogs(rt runtime.Runtime, name string, o logsOptions, out io.Writer) error {
	return rt.Logs(runtime.LogsSpec{
		Name:   name,
		Follow: o.follow,
		Tail:   o.tail,
		Out:    filtered(out, o.raw),
	})
}

// gatewayLogs prints the shared gateway's own log. brig starts that gateway for
// the hvi backend and writes its output to a file beside the socket; a network
// failure at boot lands there and nothing pointed at it until now.
func gatewayLogs(out io.Writer, o logsOptions) error {
	path, err := runtime.GatewayLogPath()
	if err != nil {
		return err
	}
	return streamGatewayFile(filtered(out, o.raw), path, o.follow, o.tail)
}

// gatewayFollowPoll is how often --follow re-reads the gateway log for bytes
// appended since. It is a file brig does not own the writer of, so there is
// nothing to wait on but the file itself.
const gatewayFollowPoll = 500 * time.Millisecond

// streamGatewayFile writes the gateway log to out, tailing it under --follow.
//
// A missing file is not a fault to fail loudly over: the gateway log appears
// only once a sandbox has needed the host network gateway, so on a machine that
// has booted only vz sandboxes there is simply nothing yet.
func streamGatewayFile(out io.Writer, path string, follow bool, tail int) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return notFoundf("no gateway log at %s yet -- it appears once a sandbox "+
				"first needs the host network gateway", path)
		}
		return err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if tail >= 0 {
		data = lastLines(data, tail)
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	// Interrupting this leaves the gateway running: it serves every sandbox on
	// the host, and reading its log is not owning it. Ctrl-C reaches this
	// process and ends it, which is the whole of what stopping should do.
	for {
		time.Sleep(gatewayFollowPoll)
		more, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		if len(more) > 0 {
			if _, err := out.Write(more); err != nil {
				return err
			}
		}
	}
}

// lastLines keeps only the final n lines of data, for --tail on a file the
// runtime is not the one reading. n <= 0 keeps nothing; fewer lines than asked
// keeps them all.
func lastLines(data []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	// SplitAfter leaves a trailing empty piece after a final newline; it is not
	// a line, so it does not count toward n.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return data
	}
	return bytes.Join(lines[len(lines)-n:], nil)
}

// filtered wraps a writer to strip terminal control sequences, or hands it back
// untouched when the raw bytes were asked for.
func filtered(out io.Writer, raw bool) io.Writer {
	if raw {
		return out
	}
	return &controlFilter{w: out}
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

// controlFilter strips terminal control sequences from a log as it passes
// through. brig sits in the middle of this stream -- unlike `brig run`, which
// hands the terminal straight to the agent -- which is what makes filtering
// possible here at all. A runtime replays into its log whatever a program
// wrote, cursor moves and colour and title-setting included, and a log meant to
// be read back is text, not a terminal to drive. --raw skips this.
//
// Newline, tab and carriage return are kept: they are the layout of the text,
// not control of a terminal. Every other C0 control and DEL is dropped.
type controlFilter struct {
	w  io.Writer
	st filterState
}

func (f *controlFilter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
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

// stripControl runs a complete byte slice through the filter, for a caller --
// a test above all -- that has all the bytes at once.
func stripControl(in []byte) []byte {
	var buf bytes.Buffer
	f := &controlFilter{w: &buf}
	_, _ = f.Write(in)
	return buf.Bytes()
}
