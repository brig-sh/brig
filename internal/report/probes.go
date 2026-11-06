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
// Pressure goes first. It is the cheapest read and the one that tells a
// thrashing guest from an idle one, so if the guest answers only one
// question this is the one worth having.
var probes = []Probe{
	{Name: "pressure", Script: `for f in memory cpu io; do echo "$f:"; cat /proc/pressure/$f; done`},
	{Name: "free -m", Script: `free -m`},
	{Name: "uptime", Script: `uptime`},
	// The probe's own shell and its children are left out by pid: in a quiet
	// guest they would otherwise top the list.
	{Name: "processes by RSS", Script: `ps -eo pid,ppid,rss,etime,args --sort=-rss 2>/dev/null | ` +
		`awk -v me=$$ 'NR==1 || ($1 != me && $2 != me)' | head -16 | cut -c1-200 | grep . || ` +
		`awk '/^VmRSS/{print FILENAME, $2, "kB"}' /proc/[0-9]*/status | sort -k2 -nr | head -15`},
	{Name: "df -h", Script: `df -h / /brig/claude "$HOME" $(ls -d /work/* 2>/dev/null)`},
	{Name: "dmesg", Script: `dmesg | tail -200`},
}

// runProbes asks each probe in turn. Once one times out, the guest is taken
// as not answering, and the rest are skipped: each would only wait out its
// own deadline.
func runProbes(ctx context.Context, rt runtime.Runtime, in Input) []Probe {
	out := make([]Probe, len(probes))
	silent := false
	for i, p := range probes {
		p.timeout = in.ProbeTimeout
		switch {
		case !in.Running:
		case silent:
			p.Skipped = true
		default:
			p = ask(ctx, rt, in, p)
			silent = p.TimedOut
		}
		out[i] = p
	}
	return out
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

// probe returns the result of the named probe, if it answered.
func (r *Report) probe(name string) (string, bool) {
	for _, p := range r.Probes {
		if p.Name == name && !p.Skipped && !p.TimedOut && p.Err == nil {
			return p.Out, true
		}
	}
	return "", false
}
