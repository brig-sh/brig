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
// --gateway takes an optional ref: the shared gateway is one per host and is
// named by no session, while the run line's parser requires a ref to resolve a
// profile at all.
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

	// Which gateway is asked for is the ref: bare --gateway is the shared one,
	// which serves the host and is named by no session, and a ref is that
	// sandbox's own. An isolated sandbox has a gateway to itself, so the ref is
	// the only thing that can pick between the two.
	if gateway {
		if ref == "" {
			return gatewayLogs(os.Stdout, o)
		}
		_, name, err := resolveSandbox(ref)
		if err != nil {
			return err
		}
		return isolatedGatewayLogs(os.Stdout, o, ref, name)
	}
	if ref == "" {
		return usagef("brig logs needs a ref, for example `brig logs claude`. " +
			"`brig logs --gateway` reads the shared gateway's log instead")
	}

	rt, name, err := resolveSandbox(ref)
	if err != nil {
		return err
	}
	return logsFor(rt, name, ref, o, os.Stdout)
}

// resolveSandbox turns a ref into the runtime it runs on and the sandbox name
// it is known by there, the way every other ref'd verb resolves one: the ref
// names the agent and the session, the profile picks the runtime, and Load
// derives the sandbox name from the two.
//
// Both gateway and sandbox logs go through this. The gateway path wants only
// the name -- an isolated gateway's socket is named after the sandbox -- but it
// has to arrive at that name by exactly the same route, or `brig logs <ref>`
// and `brig logs --gateway <ref>` could disagree about which sandbox a ref is.
func resolveSandbox(ref string) (runtime.Runtime, string, error) {
	r, err := session.ParseRef(ref)
	if err != nil {
		// Classed as a usage error, the same way split classes the identical
		// ParseRef failure on the run line: nothing ran, so this is a token in
		// the wrong place rather than a run that started and failed.
		return nil, "", usagef("%s", err)
	}
	t, ok := profile.Lookup(r.Agent)
	if !ok {
		return nil, "", notFoundf("unknown profile %q. `brig agent ls` lists them", r.Agent)
	}
	rt, err := runtime.DetectFor(runtime.Preference{Bin: t.RuntimeBin})
	if err != nil {
		return nil, "", err
	}
	cfg, err := wrap.Load(t, wrap.Options{Name: r.Label, Verbosity: verbosity}, rt)
	if err != nil {
		return nil, "", err
	}
	return rt, cfg.VMName, nil
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
//
// That gateway is per host: it serves every sandbox that asked for the shared
// network, so no ref names it. An isolated sandbox has one to itself, and
// isolatedGatewayLogs is that one's log.
func gatewayLogs(out io.Writer, o logsOptions) error {
	path, err := runtime.GatewayLogPath()
	if err != nil {
		return err
	}
	missing := notFoundf("no gateway log at %s yet -- it appears once a sandbox "+
		"first needs the host network gateway", path)
	return streamGatewayFile(filtered(out, o.raw), path, o.follow, o.tail, missing)
}

// isolatedGatewayLogs prints the log of the gateway serving one sandbox alone.
//
// This is the log the feature is most for. An isolated gateway that fails to
// start takes its sandbox's network with it, and its output is the only account
// of why -- the boot error names the file, and nothing could read it.
//
// It lives only as long as its gateway: the log is removed along with the pid
// and spec records once the gateway is confirmed stopped, which is what keeps
// ~/.brig from growing a file for every sandbox ever isolated. A gateway that
// never came up was never confirmed stopped, so the failure this is for leaves
// its log in place. Absent means the gateway is not running, and the message
// says so rather than implying the log was somewhere else.
func isolatedGatewayLogs(out io.Writer, o logsOptions, ref, name string) error {
	path, err := runtime.IsolatedGatewayLogPath(name)
	if err != nil {
		return err
	}
	missing := notFoundf("no gateway log at %s: the gateway serving %s is not running, "+
		"and its log is removed with it. A gateway that failed to start leaves its log "+
		"behind, so this is a sandbox that is stopped, was never isolated, or never ran",
		path, ref)
	return streamGatewayFile(filtered(out, o.raw), path, o.follow, o.tail, missing)
}

// gatewayFollowPoll is how often --follow re-reads the gateway log for bytes
// appended since. It is a file brig does not own the writer of, so there is
// nothing to wait on but the file itself.
const gatewayFollowPoll = 500 * time.Millisecond

// streamGatewayFile writes the gateway log to out, tailing it under --follow.
//
// A missing file is not a fault to fail loudly over, but why it is missing
// differs by gateway -- the shared one's log has not appeared yet, an isolated
// one's has been removed -- so the caller supplies that error and this returns
// it. Both are not-found, so a script can tell either from a failure.
func streamGatewayFile(out io.Writer, path string, follow bool, tail int, missing error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return missing
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
	// Interrupting this leaves the gateway running: it serves sandboxes -- every
	// shared one on the host, or the single isolated one -- and reading its log
	// is not owning it. Ctrl-C reaches this process and ends it, which is the
	// whole of what stopping should do.
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
