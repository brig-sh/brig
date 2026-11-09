package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/runtime"
)

// Probe is one question put to the guest and what came back.
type Probe struct {
	Name     string
	Script   string
	Out      string
	Err      error
	Elapsed  time.Duration
	TimedOut bool
	Skipped  bool
	timeout  time.Duration
	// stall is set on a probe that reads something able to hang on its
	// own: a mount, or a process stuck in the kernel. Its timeout marks the
	// probe, not the guest, and stall says what may have hung.
	stall string
}

// Body is the text the report shows for the probe.
func (p Probe) Body() string {
	switch {
	case p.Skipped:
		return "skipped: guest did not answer"
	case p.TimedOut:
		return "timed out after " + p.Elapsed.Round(100*time.Millisecond).String()
	case p.Err != nil:
		return strings.TrimRight(p.Out, "\n") + "\nfailed: " + p.Err.Error()
	}
	return p.Out
}

// The questions, in the order they are asked. Each is a fixed script: none
// prints the environment, and each ends in exit 0 with stderr folded in, so
// a missing tool shows its own error and does not lose the output before it.
//
// dmesg goes first: it holds the kernel's account of an out-of-memory kill,
// the most common reason a guest stops, so if the guest answers only one
// question this is the one worth having. Pressure follows; it tells a
// thrashing guest from an idle one.
//
// The probes that can stall on their own go last, so a guest that answers
// the others has said so before one of them hangs.
var probes = []Probe{
	{Name: "dmesg", Script: `dmesg | tail -200`},
	{Name: "pressure", Script: `for f in memory cpu io; do echo "$f:"; cat /proc/pressure/$f; done`},
	{Name: "free -m", Script: `free -m`},
	{Name: "uptime", Script: `uptime`},
	// The probe's own shell and its children are left out by pid: in a quiet
	// guest they would otherwise top the list. ps reads each process's
	// command line, which waits on a process stuck in the kernel.
	{Name: "processes by RSS", Script: `ps -eo pid,ppid,rss,etime,args --sort=-rss 2>/dev/null | ` +
		`awk -v me=$$ 'NR==1 || ($1 != me && $2 != me)' | head -16 | cut -c1-200 | grep . || ` +
		`awk '/^VmRSS/{print FILENAME, $2, "kB"}' /proc/[0-9]*/status | sort -k2 -nr | head -15`,
		stall: "A process may be stuck in the kernel; dmesg above may name it."},
	// runProbes adds the profile's tmpfs mount points, which differ by
	// profile. df stats each mount, and waits on one whose host side stalled.
	{Name: "df -h", Script: `df -h / "$HOME"`,
		stall: "A mount it reads may be hung: the workspace under /work, $HOME, or a tmpfs."},
}

// runProbes asks each probe in turn. Once one times out, the guest is taken
// as not answering, and the rest are skipped: each would only wait out its
// own deadline. A probe that can stall on its own is the exception: its
// timeout says nothing of the guest, which has answered the probes before
// it.
func runProbes(ctx context.Context, rt runtime.Runtime, in Input) []Probe {
	out := make([]Probe, len(probes))
	silent := false
	for i, p := range probes {
		p.timeout = in.ProbeTimeout
		if p.Name == "df -h" {
			for _, at := range in.Tmpfs {
				p.Script += " " + shellQuote(at)
			}
			p.Script += ` $(ls -d /work/* 2>/dev/null)`
		}
		switch {
		case !in.Running:
		case silent:
			p.Skipped = true
		default:
			p = ask(ctx, rt, in, p)
			silent = p.TimedOut && p.stall == ""
		}
		out[i] = p
	}
	return out
}

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ask(ctx context.Context, rt runtime.Runtime, in Input, p Probe) Probe {
	pctx, cancel := context.WithTimeout(ctx, in.ProbeTimeout)
	defer cancel()
	start := time.Now()
	p.Out, p.Err = rt.Output(runtime.ExecSpec{
		Ctx:  pctx,
		Name: in.Sandbox,
		Cmd:  []string{"sh", "-c", "{ " + p.Script + "; } 2>&1; exit 0"},
	})
	p.Elapsed = time.Since(start)
	// The runtime can hold a shorter deadline of its own (hull's agent call
	// does), so its error counts as a timeout as well as this one's.
	p.TimedOut = errors.Is(pctx.Err(), context.DeadlineExceeded) || errors.Is(p.Err, context.DeadlineExceeded)
	return p
}

func (r *Report) probesBody() string {
	if !r.in.Running {
		return "not running: nothing was asked of the guest"
	}
	var b strings.Builder
	for _, p := range r.Probes {
		if p.TimedOut {
			r.TimedOut++
		}
		elapsed := ""
		if !p.Skipped {
			elapsed = fmt.Sprintf(" (%s)", p.Elapsed.Round(time.Millisecond))
		}
		body := strings.TrimRight(p.Body(), "\n")
		if body == "" {
			body = "(no output)"
		}
		fmt.Fprintf(&b, "--- %s%s\n%s\n", p.Name, elapsed, body)
	}
	return b.String()
}

// silentProbe returns the probe the guest did not answer, if one that cannot
// stall on its own timed out.
func (r *Report) silentProbe() (Probe, bool) {
	for _, p := range r.Probes {
		if p.TimedOut && p.stall == "" {
			return p, true
		}
	}
	return Probe{}, false
}

// stalledProbe returns a probe that timed out on its own while the guest
// answered the others.
func (r *Report) stalledProbe() (Probe, bool) {
	if _, silent := r.silentProbe(); silent {
		return Probe{}, false
	}
	for _, p := range r.Probes {
		if p.TimedOut && p.stall != "" {
			return p, true
		}
	}
	return Probe{}, false
}

// probe returns the result of the named probe, if it answered.
func (r *Report) probe(name string) (string, bool) {
	for _, p := range r.Probes {
		if p.Name == name && !p.Skipped && !p.TimedOut && p.Err == nil {
			return p.Out, true
		}
	}
	return "", false
}
