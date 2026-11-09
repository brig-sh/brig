package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VM reads `hull inspect`. The memory and vCPU count come from the VMM's own
// command line, which is what the VM booted with.
func (h *hull) VM(ctx context.Context, name string) (VM, error) {
	out, err := inspectOutput(ctx, h.bin, "inspect", name)
	if err != nil {
		return VM{}, err
	}
	var doc struct {
		PID       int       `json:"pid"`
		Status    string    `json:"status"`
		StartTime time.Time `json:"startTime"`
		LogFile   string    `json:"logFile"`
		CmdLine   []string  `json:"cmdLine"`
		Backend   string    `json:"backend"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return VM{}, fmt.Errorf("%s inspect %s: %w", h.bin, name, err)
	}
	mem, cpus := vmSize(doc.CmdLine)
	return VM{
		PID:     doc.PID,
		MemMiB:  mem,
		CPUs:    cpus,
		Backend: doc.Backend,
		Status:  doc.Status,
		Started: doc.StartTime,
		LogFile: doc.LogFile,
	}, nil
}

// VM reads `nerdctl inspect`. Under urunc the container's pid is the
// monitor itself: urunc execs it in place (seen with hvi on Debian 13 and the
// rc21 runtime bundle).
func (n *nerdctl) VM(ctx context.Context, name string) (VM, error) {
	out, err := inspectOutput(ctx, n.bin, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return VM{}, err
	}
	var doc struct {
		State struct {
			Pid    int    `json:"Pid"`
			Status string `json:"Status"`
			// A string, not a time: nerdctl prints "" for a container
			// with no running task.
			StartedAt string `json:"StartedAt"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return VM{}, fmt.Errorf("%s inspect %s: %w", n.bin, name, err)
	}
	vm := VM{PID: doc.State.Pid, Status: doc.State.Status}
	if doc.State.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, doc.State.StartedAt); err == nil {
			vm.Started = t
		}
	}
	// nerdctl reports no memory or vCPUs for a microVM, but urunc execs the
	// monitor in place, so the pid is the monitor's and its command line says
	// what the VM booted with. Only while it runs: the pid of an exited task
	// may since belong to another process.
	if vm.PID > 0 && vm.Status == "running" {
		// A zombie's command line is empty.
		if argv, err := procCmdline(vm.PID); err == nil && len(argv) > 0 && argv[0] != "" {
			vm.Backend = filepath.Base(argv[0])
			vm.MemMiB, vm.CPUs = vmSize(argv)
		}
	}
	return vm, nil
}

// procCmdline reads a process's argv from /proc. A test replaces it.
var procCmdline = func(pid int) ([]string, error) {
	blob, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(blob), "\x00"), "\x00"), nil
}

// GatewayFiles names the files of the network gateway that serves one
// sandbox alone.
type GatewayFiles struct {
	Log, PID string
	// Always is whether the runtime runs such a gateway for every isolated
	// sandbox. Where it does not, missing files mean no gateway was needed.
	// Neither runtime can tell from the name alone whether the sandbox is
	// isolated: the caller checks its network posture first.
	Always bool
	// Owns reports whether pid is this gateway. A gateway that died gives
	// its pid back, and a live process at the recorded pid may be any
	// other; the runtime tells its own by the command line.
	Owns func(pid int) bool
}

// GatewayFiler is implemented by a runtime that can name those files.
type GatewayFiler interface {
	GatewayFiles(name string) (GatewayFiles, error)
}

// GatewayFiles: every isolated hull sandbox has a gateway of its own.
func (h *hull) GatewayFiles(name string) (GatewayFiles, error) {
	sock, err := isolatedSocket(name)
	if err != nil {
		return GatewayFiles{}, err
	}
	return GatewayFiles{
		Log:    gatewayLogPath(sock),
		PID:    gatewayPIDPath(sock),
		Always: true,
		Owns:   func(pid int) bool { return ownsGateway(pid, sock) },
	}, nil
}

// GatewayFiles: on nerdctl the bridge carries an isolated sandbox's traffic,
// and a host process runs beside it only when a policy filters egress.
func (n *nerdctl) GatewayFiles(name string) (GatewayFiles, error) {
	log, err := egressFile(name, ".log")
	if err != nil {
		return GatewayFiles{}, err
	}
	pid, err := egressFile(name, ".pid")
	if err != nil {
		return GatewayFiles{}, err
	}
	owns := func(p int) bool {
		got, ok := resolverPID(name)
		return ok && got == p
	}
	return GatewayFiles{Log: log, PID: pid, Owns: owns}, nil
}

func inspectOutput(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = agentCallWaitDelay
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(errb.String()); said != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, firstLines(said, 3))
		}
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// vmSize reads the memory in MiB and the vCPU count from a monitor's command
// line, 0 for a figure it does not carry. Each monitor spells them its own
// way:
//
//	hvi               --mem-mib 2048 --cpus 2
//	vz-runner         --mem 2048 --cpus 2
//	qemu              -m 2048M | -m size=2G   -smp 2 | -smp cpus=2,sockets=1
//	cloud-hypervisor  --memory size=2048M     --cpus boot=2,max=4
//
// A flag may also be written --flag=value.
func vmSize(argv []string) (memMiB, cpus int) {
	for i, a := range argv {
		flag, v, inline := strings.Cut(a, "=")
		if !inline {
			if i+1 >= len(argv) {
				continue
			}
			v = argv[i+1]
		}
		switch flag {
		case "--mem-mib", "--mem":
			if n, err := strconv.Atoi(v); err == nil && memMiB == 0 {
				memMiB = n
			}
		case "-m":
			if n := sizeMiB(strings.TrimPrefix(firstField(v, "size"), "size="), 1<<20); memMiB == 0 {
				memMiB = n
			}
		case "--memory":
			if n := sizeMiB(strings.TrimPrefix(firstField(v, "size"), "size="), 1); memMiB == 0 {
				memMiB = n
			}
		case "--cpus", "-smp":
			if n := countField(v); cpus == 0 {
				cpus = n
			}
		}
	}
	return memMiB, cpus
}

// firstField returns the key=value part of a comma list whose key is key, or
// the first part when none is keyed.
func firstField(v, key string) string {
	parts := strings.Split(v, ",")
	for _, p := range parts {
		if strings.HasPrefix(p, key+"=") {
			return p
		}
	}
	return parts[0]
}

// countField reads a vCPU count: a bare number, or the cpus= or boot= part
// of a comma list.
func countField(v string) int {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	for _, p := range strings.Split(v, ",") {
		for _, key := range []string{"cpus=", "boot="} {
			if after, ok := strings.CutPrefix(p, key); ok {
				if n, err := strconv.Atoi(after); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

// sizeMiB reads a size with an optional K, M or G suffix, in MiB. A bare
// number is in unit bytes: qemu's -m counts MiB, cloud-hypervisor's bytes.
func sizeMiB(v string, unit int64) int {
	mult := unit
	switch {
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		mult, v = 1<<30, v[:len(v)-1]
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		mult, v = 1<<20, v[:len(v)-1]
	case strings.HasSuffix(v, "K"), strings.HasSuffix(v, "k"):
		mult, v = 1<<10, v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n * mult >> 20)
}
