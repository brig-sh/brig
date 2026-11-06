// Package report builds the diagnostic report `brig report` writes: what a
// maintainer needs to tell why a session hung, slowed down or died, in one
// plain-text file a user can attach to a public issue.
//
// It runs while something is broken, often while the guest is wedged, so
// every step that can block has its own deadline, and a step that runs out
// of time is written down as a finding. It only observes. It never stops,
// starts or repairs anything. The one step that touches the VMM is the stack
// sample: eu-stack, gdb and macOS sample pause it while they read its threads,
// for at most stackTimeout.
//
// It never reads a credential. The CLI resolves the session, the profile and
// the doctor checks, which can reach the secret store, and passes the facts
// in as an Input. Everything this package renders goes through
// internal/redact, which masks by shape and knows no stored value.
// arch_test.go keeps both packages away from the store.
package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/redact"
	"github.com/brig-sh/brig/internal/runtime"
)

// Row is one `brig doctor` check, as the CLI rendered it.
type Row struct {
	State, Name, Detail string
}

// LogFile is a host log to include. A non-empty Grep keeps only the lines
// that contain it, for a log that holds every sandbox, such as the broker's
// audit log.
type LogFile struct {
	Title, Path, Grep string
}

// Input is what the CLI knows and this package must not look up itself.
type Input struct {
	// Ref is what the user typed, such as "claude". Sandbox is the name the
	// runtime knows it by. Both are empty for a report of the host alone.
	Ref, Sandbox string
	// Network is the sandbox's network mode, such as "isolated".
	Network string
	// Running is whether the runtime lists the sandbox as running. Nothing
	// is asked of a guest that is not.
	Running bool
	Doctor  []Row
	// Sessions is what `brig ls` prints. SessionRecord is the ref's entry
	// in the session index.
	Sessions, SessionRecord string
	Logs                    []LogFile
	// GatewayPIDFile is the pid file of the sandbox's own network gateway,
	// for a sandbox that has one. GatewayAlways says the runtime runs that
	// gateway for every isolated sandbox, so a missing one is a fault; where
	// it does not, a missing pid file means no gateway was needed.
	GatewayPIDFile string
	GatewayAlways  bool
	// Tmpfs are the guest paths where the profile mounts its tmpfs volumes.
	// The df probe asks about each, and a full one is a verdict.
	Tmpfs []string
	// Tail caps each log at its last Tail lines.
	Tail int
	// ProbeTimeout bounds each question put to the guest.
	ProbeTimeout time.Duration
	BrigVersion  string
	// ResolveError is why the ref could not be resolved, when it could not.
	// The report is still written, for the host alone.
	ResolveError string
}

// Section is one titled part of the report.
type Section struct {
	Title, Source, Body string
}

// Verdict is a known failure the report recognised, the line that showed it
// and what to do about it.
type Verdict struct {
	Name, Evidence, Fix string
}

// Report is everything collected, ready to render.
type Report struct {
	Generated time.Time
	Host      string
	VM        runtime.VM
	HaveVM    bool
	Probes    []Probe
	Console   string
	Sections  []Section
	Verdicts  []Verdict
	// OOMKills counts the distinct kills the guest kernel reported.
	OOMKills int
	// TimedOut counts the steps that ran out of time.
	TimedOut int

	in       Input
	vmmCPU   float64
	haveCPU  bool
	progress io.Writer
}

// step writes one progress line, so a user watching a step wait out its
// deadline knows which one it is.
func (r *Report) step(what string) {
	if r.progress != nil {
		fmt.Fprintf(r.progress, "brig report: %s\n", what)
	}
}

// Deadlines for the host-side steps. A guest probe uses Input.ProbeTimeout.
const (
	hostStepTimeout = 10 * time.Second
	logStepTimeout  = 15 * time.Second
	stackTimeout    = 20 * time.Second
)

// Collect gathers the report. It writes one line per step to progress, so a
// user watching a step wait out its deadline knows which one it is.
//
// rt may be nil when no runtime was found. The report is still written: that
// is a finding too.
func Collect(ctx context.Context, rt runtime.Runtime, in Input, progress io.Writer) Report {
	if rt == nil {
		rt = noRuntime{}
		in.Running = false
	}
	r := Report{Generated: time.Now().UTC(), in: in, progress: progress}
	step := r.step

	step("host")
	hctx, cancel := context.WithTimeout(ctx, hostStepTimeout)
	r.Host = hostFacts(hctx)
	if errors.Is(hctx.Err(), context.DeadlineExceeded) {
		r.TimedOut++
		r.Host += "host facts timed out after " + hostStepTimeout.String() + "\n"
	}
	cancel()

	r.add("brig doctor", "brig doctor, without the secrets check", doctorBody(in.Doctor))
	r.add("Sessions", "brig ls and the session index", sessionsBody(in))

	step("runtime")
	r.add("Runtime", rt.Kind()+" listing", r.listing(rt))

	step("VMM")
	r.inspect(ctx, rt)

	if in.Running {
		step("guest")
	}
	r.Probes = runProbes(ctx, rt, in)
	r.add("Guest probes", rt.Kind()+" exec, "+in.ProbeTimeout.String()+" each", r.probesBody())

	step("console log")
	r.Console = r.console(ctx, rt)
	r.add("Console log", rt.Kind()+" logs, last "+fmt.Sprint(in.Tail)+" lines", r.Console)

	for _, l := range in.Logs {
		step(strings.ToLower(l.Title))
		body, err := tailFile(l.Path, in.Tail, l.Grep)
		source := l.Path
		if l.Grep != "" {
			source += ", lines with " + l.Grep
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			body = "not present"
		case err != nil:
			body = "not read: " + err.Error()
		}
		r.add(l.Title, source, body)
	}

	r.Verdicts = match(&r)
	return r
}

// noRuntime stands in when no runtime was found, so every section says so.
type noRuntime struct{ runtime.Runtime }

func (noRuntime) Kind() string { return "no runtime" }

func (noRuntime) List() ([]runtime.Instance, error) {
	return nil, errors.New("no runtime found; brig doctor says why")
}

func (noRuntime) Logs(runtime.LogsSpec) error { return errors.New("no runtime found") }

func (r *Report) add(title, source, body string) {
	r.Sections = append(r.Sections, Section{Title: title, Source: source, Body: body})
}

func (r Report) section(title string) (Section, bool) {
	for _, s := range r.Sections {
		if s.Title == title {
			return s, true
		}
	}
	return Section{}, false
}

func doctorBody(rows []Row) string {
	if len(rows) == 0 {
		return "not run"
	}
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%4s  %-9s %s\n", row.State, row.Name, row.Detail)
	}
	return b.String()
}

func sessionsBody(in Input) string {
	body := strings.TrimRight(in.Sessions, "\n") + "\n"
	if in.SessionRecord != "" {
		body += "\nsession record for " + in.Ref + ":\n" + in.SessionRecord
	}
	return body
}

// listing runs the runtime's list under a deadline. The list call takes no
// context, so a runtime that never answers is left behind; that costs one
// goroutine in a process about to exit.
func (r *Report) listing(rt runtime.Runtime) string {
	type result struct {
		list []runtime.Instance
		err  error
	}
	done := make(chan result, 1)
	go func() {
		list, err := rt.List()
		done <- result{list, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return "failed: " + res.err.Error()
		}
		var b strings.Builder
		found := false
		for _, i := range res.list {
			fmt.Fprintf(&b, "%-40s %s\n", i.Name, i.State)
			found = found || i.Name == r.in.Sandbox
		}
		if r.in.Sandbox != "" && !found {
			fmt.Fprintf(&b, "no sandbox named %s\n", r.in.Sandbox)
		}
		return b.String()
	case <-time.After(hostStepTimeout):
		r.TimedOut++
		return "timed out after " + hostStepTimeout.String()
	}
}

// inspect fills the VM and the two VMM sections.
func (r *Report) inspect(ctx context.Context, rt runtime.Runtime) {
	const unavailable = "not available on this runtime"
	vi, ok := rt.(runtime.VMInspector)
	switch {
	case r.in.Sandbox == "":
		r.add("VMM process", "", "no sandbox named")
		r.add("VMM stacks", "", "no sandbox named")
		return
	case !ok:
		r.add("VMM process", "", unavailable)
		r.add("VMM stacks", "", unavailable)
		return
	}
	ictx, cancel := context.WithTimeout(ctx, hostStepTimeout)
	vm, err := vi.VM(ictx, r.in.Sandbox)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			r.TimedOut++
		}
		r.add("VMM process", rt.Kind()+" inspect", "failed: "+err.Error())
		r.add("VMM stacks", "", "no process to sample")
		return
	}
	r.VM, r.HaveVM = vm, true
	if vm.PID <= 0 {
		r.add("VMM process", rt.Kind()+" inspect", "the runtime names no process (status "+vm.Status+")")
		r.add("VMM stacks", "", "no process to sample")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, hostStepTimeout)
	stats, cpu, err := procStats(sctx, vm.PID)
	cancel()
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		r.TimedOut++
	}
	if err != nil {
		stats = "failed: " + err.Error()
	} else {
		r.vmmCPU, r.haveCPU = cpu, true
	}
	r.add("VMM process", fmt.Sprintf("ps, pid %d", vm.PID), stats)

	sctx, cancel = context.WithTimeout(ctx, stackTimeout)
	r.step("VMM stacks; this pauses the VMM while its threads are read")
	tool, stacks := vmmStacks(sctx, vm.PID)
	if errors.Is(sctx.Err(), context.DeadlineExceeded) {
		r.TimedOut++
		stacks += "\ntimed out after " + stackTimeout.String()
	}
	cancel()
	r.add("VMM stacks", tool, stacks)
}

func (r *Report) console(ctx context.Context, rt runtime.Runtime) string {
	if r.in.Sandbox == "" {
		return "no sandbox named"
	}
	lctx, cancel := context.WithTimeout(ctx, logStepTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	err := rt.Logs(runtime.LogsSpec{Name: r.in.Sandbox, Tail: r.in.Tail, Out: &out, Err: &errb, Ctx: lctx})
	if errors.Is(lctx.Err(), context.DeadlineExceeded) {
		r.TimedOut++
		return out.String() + "\ntimed out after " + logStepTimeout.String()
	}
	if err != nil {
		return out.String() + "\nfailed: " + err.Error() + " " + strings.TrimSpace(errb.String())
	}
	return out.String()
}

// Render writes the report. Every body and every verdict passes through
// redact.String on its way out.
func (r Report) Render(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# brig report\n\nGenerated %s\n\n", r.Generated.Format(time.RFC3339))

	b.WriteString("## Verdict\n\n")
	for _, v := range r.Verdicts {
		fmt.Fprintf(&b, "- **%s**: %s\n", v.Name, plain(v.Fix))
		if v.Evidence != "" {
			fmt.Fprintf(&b, "  Evidence: %s\n", codeSpan(plain(strings.TrimSpace(v.Evidence))))
		}
	}

	b.WriteString("\n## Summary\n\n")
	b.WriteString(fence(plain(r.summary())))

	for _, s := range r.Sections {
		fmt.Fprintf(&b, "\n## %s\n\n", plain(s.Title))
		if s.Source != "" {
			fmt.Fprintf(&b, "Source: %s\n\n", plain(s.Source))
		}
		b.WriteString(fence(plain(s.Body)))
	}
	_, err := io.WriteString(w, redact.String(b.String()))
	return err
}

// controlSeq matches the terminal control sequences a guest console or a
// coloured dmesg carries. They mean nothing in a file and garble an issue.
//
// Each piece is stripped before it is fenced: stripping afterwards could join
// two runs of backticks into one long enough to close the fence.
var controlSeq = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

func (r Report) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "brig          %s\n", r.in.BrigVersion)
	b.WriteString(r.Host)
	if r.in.ResolveError != "" {
		fmt.Fprintf(&b, "ref           %s, not resolved: %s\n", r.in.Ref, r.in.ResolveError)
	} else if r.in.Ref != "" {
		fmt.Fprintf(&b, "ref           %s\n", r.in.Ref)
		fmt.Fprintf(&b, "sandbox       %s (running: %t)\n", r.in.Sandbox, r.in.Running)
		fmt.Fprintf(&b, "network       %s\n", r.in.Network)
	}
	if r.HaveVM {
		fmt.Fprintf(&b, "vm            backend %s, status %s, pid %d, %d MiB, %d vCPUs",
			orUnknown(r.VM.Backend), orUnknown(r.VM.Status), r.VM.PID, r.VM.MemMiB, r.VM.CPUs)
		if !r.VM.Started.IsZero() {
			fmt.Fprintf(&b, ", started %s", r.VM.Started.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "oom kills     %d\n", r.OOMKills)
	fmt.Fprintf(&b, "timed out     %d steps\n", r.TimedOut)
	return b.String()
}

func plain(s string) string { return controlSeq.ReplaceAllString(s, "") }

// codeSpan wraps s in more backticks than any run inside it, padded with
// spaces, so a backtick in a guest's kernel line cannot end the span early.
func codeSpan(s string) string {
	ticks := "`"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	if len(ticks) == 1 {
		return ticks + s + ticks
	}
	return ticks + " " + s + " " + ticks
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// fence wraps a body in a code fence longer than any run of backticks
// inside it, so a log cannot close the fence and turn into Markdown.
func fence(body string) string {
	ticks := "```"
	for strings.Contains(body, ticks) {
		ticks += "`"
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		body = "(empty)"
	}
	return ticks + "\n" + body + "\n" + ticks + "\n"
}
