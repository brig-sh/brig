package report

import (
	"bufio"
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
	fmt.Fprintf(&b, "host          %s, kernel %s\n", osRelease(), strings.TrimSpace(run(ctx, "uname", "-r", "-m")))
	fmt.Fprintf(&b, "cpu           %s\n", procField("/proc/cpuinfo", "model name"))
	fmt.Fprintf(&b, "memory        %s\n", procField("/proc/meminfo", "MemTotal"))
	if load, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Fprintf(&b, "load          %s\n", strings.TrimSpace(string(load)))
	}
	return b.String()
}

func osRelease() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}

// procField returns the value of the first "key: value" line of path.
func procField(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// procStats returns ps's line for pid, and the share of one CPU it used over
// the last second, measured from /proc: ps's own %cpu is an average since
// start.
func procStats(ctx context.Context, pid int) (string, float64, error) {
	p := strconv.Itoa(pid)
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=,etime=,stat=,nlwp=", "-p", p).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ps -p %s: %w", p, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 5 {
		return "", 0, fmt.Errorf("ps -p %s printed %q", p, out)
	}
	cpu, err := cpuShare(ctx, func() (float64, error) { return procCPUTime(pid) })
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("pid %s  cpu %.1f%% over 1s  rss %s KiB  elapsed %s  state %s  threads %s\n",
		f[0], cpu, f[1], f[2], f[3], f[4]), cpu, nil
}

// clockTicks is USER_HZ, which /proc/<pid>/stat counts CPU time in. It is
// 100 on every Linux architecture Brig ships for.
const clockTicks = 100

// procCPUTime is the user and system CPU time of pid, in seconds.
func procCPUTime(pid int) (float64, error) {
	blob, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// Fields after the parenthesised command name, which can hold spaces.
	// utime and stime are the 14th and 15th fields of the line, so the 12th
	// and 13th after it.
	i := strings.LastIndexByte(string(blob), ')')
	if i < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: unexpected shape", pid)
	}
	f := strings.Fields(string(blob[i+1:]))
	if len(f) < 13 {
		return 0, fmt.Errorf("/proc/%d/stat: unexpected shape", pid)
	}
	utime, err1 := strconv.ParseFloat(f[11], 64)
	stime, err2 := strconv.ParseFloat(f[12], 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("/proc/%d/stat: unexpected shape", pid)
	}
	return (utime + stime) / clockTicks, nil
}

// vmmStacks lists each thread's state and the kernel function it waits in,
// which /proc gives the process owner without ptrace. When eu-stack or gdb
// is installed, their user-space stacks follow.
func vmmStacks(ctx context.Context, pid int) (string, string) {
	var b strings.Builder
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*", pid))
	for _, task := range tasks {
		stat, _ := os.ReadFile(filepath.Join(task, "stat"))
		wchan, _ := os.ReadFile(filepath.Join(task, "wchan"))
		comm, _ := os.ReadFile(filepath.Join(task, "comm"))
		state := "?"
		// The state follows the parenthesised command name, which can itself
		// hold spaces and parentheses.
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
			state = string(stat[i+2])
		}
		fmt.Fprintf(&b, "tid %-8s %-16s state %s  wchan %s\n", filepath.Base(task),
			strings.TrimSpace(string(comm)), state, strings.TrimSpace(string(wchan)))
	}
	tool := fmt.Sprintf("/proc/%d/task", pid)
	switch {
	case have("eu-stack"):
		out, err := stackTool(ctx, pid, "eu-stack", "-p", strconv.Itoa(pid))
		tool += " and eu-stack"
		b.WriteString("\n" + string(out))
		if err != nil {
			b.WriteString("eu-stack: " + err.Error() + "\n")
		}
	case have("gdb"):
		out, err := stackTool(ctx, pid, "gdb", "-batch", "-p", strconv.Itoa(pid), "-ex", "thread apply all bt")
		tool += " and gdb"
		b.WriteString("\n" + string(out))
		if err != nil {
			b.WriteString("gdb: " + err.Error() + "\n")
		}
	default:
		tool += "; neither eu-stack nor gdb is installed"
	}
	return tool, b.String()
}

func have(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
