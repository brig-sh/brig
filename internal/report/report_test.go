package report

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/runtime"
)

// fakeRuntime answers the few calls a report makes. The embedded interface
// is nil, so any other call panics and shows up in the test.
type fakeRuntime struct {
	runtime.Runtime
	instances []runtime.Instance
	console   string
	// output answers Output; nil blocks until the exec's context is done.
	output  func(spec runtime.ExecSpec) (string, error)
	outputs int
}

func (f *fakeRuntime) Kind() string { return "fake" }

func (f *fakeRuntime) List() ([]runtime.Instance, error) { return f.instances, nil }

func (f *fakeRuntime) Logs(spec runtime.LogsSpec) error {
	_, err := io.WriteString(spec.Out, f.console)
	return err
}

func (f *fakeRuntime) Output(spec runtime.ExecSpec) (string, error) {
	f.outputs++
	if f.output != nil {
		return f.output(spec)
	}
	<-spec.Ctx.Done()
	return "", spec.Ctx.Err()
}

// inspectingRuntime adds the optional VM inspector.
type inspectingRuntime struct {
	*fakeRuntime
	vm runtime.VM
}

func (r inspectingRuntime) VM(context.Context, string) (runtime.VM, error) { return r.vm, nil }

func running() Input {
	return Input{Ref: "claude", Sandbox: "brig-claude-code", Network: "isolated", Running: true,
		Tail: 2000, ProbeTimeout: 100 * time.Millisecond, BrigVersion: "v0.5.1"}
}

// The case the report exists for: a guest that accepts the exec and never
// answers. The report has to come back, say the first probe timed out, and
// not spend a timeout on each of the others.
func TestCollectSkipsProbesAfterFirstTimeout(t *testing.T) {
	rt := &fakeRuntime{}
	start := time.Now()
	r := Collect(context.Background(), rt, running(), io.Discard)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Collect took %s against a silent guest with a 100ms probe timeout", elapsed)
	}
	if len(r.Probes) != len(probes) {
		t.Fatalf("got %d probe results, want %d", len(r.Probes), len(probes))
	}
	if !r.Probes[0].TimedOut || !strings.Contains(r.Probes[0].Body(), "timed out after") {
		t.Errorf("first probe = %+v, want it timed out", r.Probes[0])
	}
	for _, p := range r.Probes[1:] {
		if !p.Skipped || p.Body() != "skipped: guest did not answer" {
			t.Errorf("probe %s = %q, want it skipped", p.Name, p.Body())
		}
	}
	if rt.outputs != 1 {
		t.Errorf("the guest was asked %d times after it went silent, want 1", rt.outputs)
	}
	if r.TimedOut != 1 {
		t.Errorf("TimedOut = %d, want 1", r.TimedOut)
	}
}

// Every section is read by a stranger once the file is on an issue. A
// planted credential in any log must not come back out.
func TestRenderMasksPlantedTokens(t *testing.T) {
	planted := []string{
		"sk-ant-api03-PLANTED0123456789",
		"ghp_PLANTED0123456789abcdefABCDEF012345",
		"PLANTEDPLANTED0123456789",
	}
	dir := t.TempDir()
	gw := filepath.Join(dir, "gateway.log")
	broker := filepath.Join(dir, "broker.log")
	writeFile(t, gw, "level=info msg=\"dial\" key="+planted[0]+"\n")
	writeFile(t, broker, "upstream said Authorization: Bearer "+planted[2]+"\n")

	in := running()
	in.Logs = []LogFile{{Title: "Gateway log", Path: gw}, {Title: "Broker log", Path: broker}}
	in.Doctor = []Row{{State: "ok", Name: "brig", Detail: "v0.5.1"}}
	rt := &fakeRuntime{
		console: "export GH=" + planted[1] + "\n",
		output:  func(runtime.ExecSpec) (string, error) { return "ok\n", nil },
	}
	var out bytes.Buffer
	if err := Collect(context.Background(), rt, in, io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, p := range planted {
		if strings.Contains(out.String(), p) {
			t.Errorf("the rendered report still holds %q", p)
		}
	}
	if !strings.Contains(out.String(), "[masked: ") {
		t.Error("the rendered report shows no mask at all")
	}
}

// countingReaderAt serves size bytes of filler ending in "\nLAST\n" and
// counts what was read.
type countingReaderAt struct {
	size int64
	read int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.size {
		return 0, io.EOF
	}
	tail := []byte("\nLAST\n")
	n := 0
	for ; n < len(p) && off+int64(n) < c.size; n++ {
		pos := off + int64(n)
		if pos >= c.size-int64(len(tail)) {
			p[n] = tail[pos-(c.size-int64(len(tail)))]
		} else if pos%80 == 79 {
			p[n] = '\n'
		} else {
			p[n] = 'x'
		}
	}
	c.read += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A chatty guest leaves a console log of hundreds of megabytes. The report
// reads the end of it.
func TestTailReadsOnlyTheEnd(t *testing.T) {
	r := &countingReaderAt{size: 50 << 20}
	got, err := tailReaderAt(r, r.size, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "LAST\n") {
		t.Errorf("tail ends %q, want LAST", got[max(0, len(got)-20):])
	}
	if lines := strings.Count(got, "\n"); lines != 3 {
		t.Errorf("tail holds %d lines, want 3", lines)
	}
	if r.read > 1<<20 {
		t.Errorf("read %d bytes of a 50 MB log to find 3 lines", r.read)
	}
}

// The broker audit log holds every sandbox. Grep keeps one.
func TestTailFileGrepKeepsMatchingLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var b strings.Builder
	for i := range 10 {
		fmt.Fprintf(&b, "route=gh sandbox=brig-a n=%d\nroute=gh sandbox=brig-b n=%d\n", i, i)
	}
	writeFile(t, path, b.String())
	got, err := tailFile(path, 2, "sandbox=brig-b")
	if err != nil {
		t.Fatal(err)
	}
	if got != "route=gh sandbox=brig-b n=8\nroute=gh sandbox=brig-b n=9\n" {
		t.Errorf("tailFile = %q", got)
	}
}

func TestCollectWithoutInspector(t *testing.T) {
	rt := &fakeRuntime{output: func(runtime.ExecSpec) (string, error) { return "", nil }}
	r := Collect(context.Background(), rt, running(), io.Discard)
	for _, title := range []string{"VMM process", "VMM stacks"} {
		s, ok := r.section(title)
		if !ok || !strings.Contains(s.Body, "not available on this runtime") {
			t.Errorf("section %q = %+v, want it to say the runtime cannot inspect the VM", title, s)
		}
	}
}

// A ref with no sandbox still gets a report, and the guest is not asked
// anything.
func TestCollectWithoutSandbox(t *testing.T) {
	rt := &fakeRuntime{}
	in := running()
	in.Running = false
	r := Collect(context.Background(), rt, in, io.Discard)
	if rt.outputs != 0 {
		t.Errorf("a stopped sandbox was asked %d questions", rt.outputs)
	}
	s, ok := r.section("Guest probes")
	if !ok || !strings.Contains(s.Body, "not running") {
		t.Errorf("guest section = %+v, want it to say the sandbox is not running", s)
	}
}

func TestRenderOrder(t *testing.T) {
	rt := inspectingRuntime{&fakeRuntime{output: func(runtime.ExecSpec) (string, error) { return "ok\n", nil }},
		runtime.VM{MemMiB: 4096, CPUs: 4, Backend: "hvi", Status: "running"}}
	in := running()
	in.Logs = []LogFile{{Title: "Gateway log", Path: filepath.Join(t.TempDir(), "absent.log")}}
	var out bytes.Buffer
	if err := Collect(context.Background(), rt, in, io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	want := []string{"## Verdict", "## Summary", "## brig doctor", "## Sessions", "## Runtime",
		"## VMM process", "## VMM stacks", "## Guest probes", "## Console log", "## Gateway log"}
	at := -1
	for _, h := range want {
		i := strings.Index(out.String(), h)
		if i < 0 {
			t.Fatalf("report has no %q:\n%s", h, out.String())
		}
		if i < at {
			t.Errorf("%q is out of order", h)
		}
		at = i
	}
	if strings.Contains(out.String(), "Read the file before you post it") {
		t.Error("the closing line belongs to the command, not the file")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A coloured dmesg and a console with a shell prompt carry terminal control
// sequences. In a file they are noise, and on an issue they garble the text.
func TestRenderDropsControlSequences(t *testing.T) {
	rt := &fakeRuntime{console: "\x1b[?2004hroot@urunc:/# \x1b[31mred\x1b[0m\n",
		output: func(runtime.ExecSpec) (string, error) { return "", nil }}
	var out bytes.Buffer
	if err := Collect(context.Background(), rt, running(), io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("rendered report holds an escape character: %q", out.String())
	}
	if !strings.Contains(out.String(), "root@urunc:/# red") {
		t.Error("the text around the sequences was lost")
	}
}

// Stripping a control sequence can join two runs of backticks. The fence is
// measured on the text after stripping, so the log cannot close it.
func TestRenderFencesTheStrippedText(t *testing.T) {
	rt := &fakeRuntime{console: "a``\x1b[0m`b\n## not a heading\n",
		output: func(runtime.ExecSpec) (string, error) { return "", nil }}
	var out bytes.Buffer
	if err := Collect(context.Background(), rt, running(), io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "````\na```b\n") {
		t.Errorf("console not fenced with four backticks:\n%s", out.String())
	}
}

// A kernel line with a backtick in it stays one code span in the verdict.
func TestRenderEvidenceWithABacktick(t *testing.T) {
	console := "[1.0] Out of memory: Killed process 7 (n`de) total-vm:1kB\n"
	var out bytes.Buffer
	if err := Collect(context.Background(), answering(console, nil), running(), io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Evidence: `` [1.0] Out of memory: Killed process 7 (n`de) total-vm:1kB ``") {
		t.Errorf("evidence not wrapped in a wider span:\n%s", out.String())
	}
}

// One runaway line in the audit log is cut. The section keeps every other
// line instead of failing.
func TestTailFileCutsALongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	blob := "sandbox=a " + strings.Repeat("x", 2<<20) + "\nsandbox=a short\nsandbox=b other\n"
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(path, 10, "sandbox=a ")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " [line cut]") || len(lines[0]) > 70<<10 || lines[1] != "sandbox=a short" {
		t.Errorf("got %d lines (first %d bytes), want the cut line then %q", len(lines), len(lines[0]), "sandbox=a short")
	}
}

// A host with no runtime still gets a report, which says so.
func TestCollectWithoutRuntime(t *testing.T) {
	var out bytes.Buffer
	if err := Collect(context.Background(), nil, running(), io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no runtime found") {
		t.Errorf("report does not say no runtime was found:\n%s", out.String())
	}
}

// hull's agent call holds a deadline shorter than the probe's, and kills the
// exec when it fires. That is the guest not answering too.
func TestCollectCountsTheRuntimesOwnTimeout(t *testing.T) {
	rt := &fakeRuntime{output: func(runtime.ExecSpec) (string, error) {
		time.Sleep(50 * time.Millisecond)
		return "", fmt.Errorf("the guest did not answer: %w", context.DeadlineExceeded)
	}}
	in := running()
	in.ProbeTimeout = 10 * time.Second
	r := Collect(context.Background(), rt, in, io.Discard)
	if !r.Probes[0].TimedOut {
		t.Fatalf("first probe = %+v, want it timed out", r.Probes[0])
	}
	if !r.Probes[1].Skipped || rt.outputs != 1 {
		t.Errorf("the guest was asked %d times; second probe = %+v", rt.outputs, r.Probes[1])
	}
	verdictNamed(t, r, "unresponsive")
}

func TestSummaryNamesAResolveError(t *testing.T) {
	in := Input{Ref: "claude@x", ResolveError: "unknown profile", Tail: 10, ProbeTimeout: time.Second}
	var out bytes.Buffer
	if err := Collect(context.Background(), nil, in, io.Discard).Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "claude@x, not resolved: unknown profile") {
		t.Errorf("summary does not name the resolve error:\n%s", out.String())
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]float64{"0:00.38": 0.38, "12:01.50": 721.5, "1:02:03.25": 3723.25, "7.00": 7} {
		got, err := parseClock(in)
		if err != nil || got != want {
			t.Errorf("parseClock(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseClock("x"); err == nil {
		t.Error("parseClock accepted x")
	}
}

// A process that stopped uses no CPU, whatever its average since start says.
// That is what lets the verdict call a stuck VMM idle.
func TestProcStatsMeasuresAStoppedProcessAsIdle(t *testing.T) {
	cmd := exec.Command("sh", "-c", "while :; do :; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	_, cpu, err := procStats(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if cpu >= 1 {
		t.Errorf("a stopped process measured at %.1f%% CPU", cpu)
	}
}
