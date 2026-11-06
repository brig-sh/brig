package report

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func hostFacts(ctx context.Context) string {
	var b strings.Builder
	product := strings.Join(strings.Fields(run(ctx, "sw_vers", "-productVersion")), " ")
	fmt.Fprintf(&b, "host          macOS %s on %s\n", product, strings.TrimSpace(run(ctx, "uname", "-m")))
	fmt.Fprintf(&b, "cpu           %s\n", strings.TrimSpace(run(ctx, "sysctl", "-n", "machdep.cpu.brand_string")))
	if mem, err := strconv.ParseInt(strings.TrimSpace(run(ctx, "sysctl", "-n", "hw.memsize")), 10, 64); err == nil {
		fmt.Fprintf(&b, "memory        %d MiB\n", mem>>20)
	}
	fmt.Fprintf(&b, "load          %s\n", strings.Trim(run(ctx, "sysctl", "-n", "vm.loadavg"), "{} \n"))
	return b.String()
}

// procStats returns ps's line for pid with its thread count and state, and
// the share of one CPU it used over the last second. ps's own %cpu is a
// decaying average since start, which still reads busy for a process that
// stopped a minute ago, so the share is measured here from its CPU time.
func procStats(ctx context.Context, pid int) (string, float64, error) {
	p := strconv.Itoa(pid)
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=,etime=,stat=", "-p", p).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ps -p %s: %w", p, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 {
		return "", 0, fmt.Errorf("ps -p %s printed %q", p, out)
	}
	cpu, err := cpuShare(ctx, func() (float64, error) { return psCPUTime(ctx, p) })
	if err != nil {
		return "", 0, err
	}
	threads := "unknown"
	if m, err := exec.CommandContext(ctx, "ps", "-M", "-p", p).Output(); err == nil {
		threads = strconv.Itoa(strings.Count(strings.TrimSpace(string(m)), "\n"))
	}
	return fmt.Sprintf("pid %s  cpu %.1f%% over 1s  rss %s KiB  elapsed %s  state %s  threads %s\n",
		fields[0], cpu, fields[1], fields[2], fields[3], threads), cpu, nil
}

// psCPUTime is the CPU time ps reports for pid, in seconds. ps prints it as
// [[hh:]mm:]ss.cc.
func psCPUTime(ctx context.Context, pid string) (float64, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "time=", "-p", pid).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -o time -p %s: %w", pid, err)
	}
	return parseClock(strings.TrimSpace(string(out)))
}

// vmmStacks samples pid for five seconds. sample writes its analysis to a
// file and only a progress note to stdout, so it gets a file of its own.
func vmmStacks(ctx context.Context, pid int) (string, string) {
	dir, err := os.MkdirTemp("", "brig-report-sample-")
	if err != nil {
		return "sample", "failed: " + err.Error()
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "sample.txt")
	if out, err := exec.CommandContext(ctx, "sample", strconv.Itoa(pid), "5", "-mayDie", "-file", path).CombinedOutput(); err != nil {
		return "sample", fmt.Sprintf("failed: %v\n%s", err, out)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return "sample", "failed: " + err.Error()
	}
	return fmt.Sprintf("sample %d 5", pid), string(blob)
}
