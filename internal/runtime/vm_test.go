package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// scriptBin writes a stand-in runtime binary that runs body under /bin/sh.
func scriptBin(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// What hull 0.1.0 prints for a running hvi instance, trimmed to the fields VM
// reads and the flags around the two it takes from the command line.
const hullInspectJSON = `{
  "id": "brig-plum",
  "pid": 28867,
  "status": "running",
  "startTime": "2026-11-06T10:38:27.825731+02:00",
  "logFile": "/Users/u/.hull/store/instances/brig-plum/log",
  "cmdLine": ["/opt/homebrew/bin/hvi", "boot", "--kernel", "/k", "--mem-mib", "4096", "--cpus", "4", "--sandbox-id", "brig-plum"],
  "backend": "hvi"
}`

func TestHullVMParsesInspect(t *testing.T) {
	h := &hull{bin: scriptBin(t, "hull", "[ \"$1\" = inspect ] && [ \"$2\" = brig-plum ] || exit 1\ncat <<'EOF'\n"+hullInspectJSON+"\nEOF")}
	vm, err := h.VM(context.Background(), "brig-plum")
	if err != nil {
		t.Fatal(err)
	}
	if vm.PID != 28867 || vm.MemMiB != 4096 || vm.CPUs != 4 || vm.Backend != "hvi" || vm.Status != "running" {
		t.Errorf("VM = %+v, want pid 28867, 4096 MiB, 4 CPUs, hvi, running", vm)
	}
	if vm.LogFile != "/Users/u/.hull/store/instances/brig-plum/log" {
		t.Errorf("LogFile = %q", vm.LogFile)
	}
	if want := time.Date(2026, 11, 6, 8, 38, 27, 825731000, time.UTC); !vm.Started.Equal(want) {
		t.Errorf("Started = %s, want %s", vm.Started, want)
	}
}

func TestHullVMFailsWhenInspectFails(t *testing.T) {
	h := &hull{bin: scriptBin(t, "hull", "echo 'instance not found' >&2; exit 1")}
	if _, err := h.VM(context.Background(), "gone"); err == nil {
		t.Fatal("VM of an instance hull does not know returned no error")
	}
}

// A diagnostic report asks a wedged guest questions on purpose. Its own
// deadline has to win over the agent call's longer one.
func TestHullOutputHonoursCtx(t *testing.T) {
	h := &hull{bin: scriptBin(t, "hull", "sleep 30")}
	assertOutputBounded(t, h)
}

// nerdctl exec had no deadline at all, so a guest that never answers held
// the caller forever.
func TestNerdctlOutputHonoursCtx(t *testing.T) {
	n := &nerdctl{bin: scriptBin(t, "nerdctl", "sleep 30")}
	assertOutputBounded(t, n)
}

func assertOutputBounded(t *testing.T, rt interface {
	Output(ExecSpec) (string, error)
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := rt.Output(ExecSpec{Ctx: ctx, Name: "vm", Cmd: []string{"uptime"}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an exec cut off by its context returned no error")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Output took %s with a 200ms context", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Output ignored its context")
	}
}

func TestNerdctlVMParsesInspect(t *testing.T) {
	swapCmdline(t, func(int) ([]string, error) { return nil, os.ErrNotExist })
	n := &nerdctl{bin: scriptBin(t, "nerdctl", "[ \"$1\" = inspect ] || exit 1\n"+
		`echo '{"State":{"Pid":4242,"Status":"running","StartedAt":"2026-11-06T10:00:00Z"}}'`)}
	vm, err := n.VM(context.Background(), "brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if vm.PID != 4242 || vm.Status != "running" || vm.MemMiB != 0 || vm.CPUs != 0 {
		t.Errorf("VM = %+v, want pid 4242, running, and no memory or CPU figure", vm)
	}
	if want := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC); !vm.Started.Equal(want) {
		t.Errorf("Started = %s, want %s", vm.Started, want)
	}
}

// Both runtimes answer the optional interface, so a caller that finds no
// inspector knows it is talking to something else.
var (
	_ VMInspector = (*hull)(nil)
	_ VMInspector = (*nerdctl)(nil)
)

// A caller with a longer deadline than the agent call's own still has to be
// able to tell a silent guest from a failed command. Without this the agent
// call's timer kills hull first, and Run reports only "signal: killed".
func TestHullOutputSaysTheGuestTimedOut(t *testing.T) {
	h := &hull{bin: hangingHull(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := h.Output(ExecSpec{Ctx: ctx, Name: "vm", Cmd: []string{"uptime"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Output = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

func TestNerdctlOutputSaysTheGuestTimedOut(t *testing.T) {
	n := &nerdctl{bin: scriptBin(t, "nerdctl", "sleep 30")}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := n.Output(ExecSpec{Ctx: ctx, Name: "vm", Cmd: []string{"uptime"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Output = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

func swapCmdline(t *testing.T, fn func(int) ([]string, error)) {
	t.Helper()
	old := procCmdline
	procCmdline = fn
	t.Cleanup(func() { procCmdline = old })
}

// Seen on Debian 13 with bundle rc21: the container's pid is hvi itself, and
// its argv carries what the VM booted with.
func TestNerdctlVMReadsTheMonitorCommandLine(t *testing.T) {
	swapCmdline(t, func(pid int) ([]string, error) {
		if pid != 2004 {
			t.Errorf("read the command line of pid %d, want 2004", pid)
		}
		return []string{"/home/u/.local/share/brig/data/bin/hvi", "boot", "--kernel", "/urunc-boot/kernel",
			"--mem-mib", "2048", "--cpus", "2"}, nil
	})
	n := &nerdctl{bin: scriptBin(t, "nerdctl", `echo '{"State":{"Pid":2004,"Status":"running"}}'`)}
	vm, err := n.VM(context.Background(), "brig-ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Backend != "hvi" || vm.MemMiB != 2048 || vm.CPUs != 2 {
		t.Errorf("VM = %+v, want hvi, 2048 MiB, 2 CPUs", vm)
	}
}

func TestGatewayFiles(t *testing.T) {
	// A short directory: macOS's per-test temp path is too long for the unix
	// socket the gateway path is derived from.
	dir, err := os.MkdirTemp("/tmp", "brig-gw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	h, err := (&hull{}).GatewayFiles("brig-x")
	if err != nil || !h.Always || !strings.HasSuffix(h.PID, ".pid") || !strings.HasSuffix(h.Log, ".log") {
		t.Errorf("hull GatewayFiles = %+v, %v; want an always-present gateway with .pid and .log", h, err)
	}
	n, err := (&nerdctl{}).GatewayFiles("brig-x")
	if err != nil || n.Always || !strings.Contains(n.PID, "egress") {
		t.Errorf("nerdctl GatewayFiles = %+v, %v; want the egress files, not always present", n, err)
	}
}
