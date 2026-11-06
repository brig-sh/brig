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
	return VM{
		PID:     doc.PID,
		MemMiB:  flagInt(doc.CmdLine, "--mem-mib"),
		CPUs:    flagInt(doc.CmdLine, "--cpus"),
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
			Pid       int       `json:"Pid"`
			Status    string    `json:"Status"`
			StartedAt time.Time `json:"StartedAt"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return VM{}, fmt.Errorf("%s inspect %s: %w", n.bin, name, err)
	}
	vm := VM{PID: doc.State.Pid, Status: doc.State.Status, Started: doc.State.StartedAt}
	// nerdctl reports no memory or vCPUs for a microVM, but urunc execs the
	// monitor in place, so the pid is the monitor's and its command line says
	// what the VM booted with.
	if vm.PID > 0 {
		// A zombie's command line is empty.
		if argv, err := procCmdline(vm.PID); err == nil && len(argv) > 0 && argv[0] != "" {
			vm.Backend = filepath.Base(argv[0])
			vm.MemMiB = flagInt(argv, "--mem-mib")
			vm.CPUs = flagInt(argv, "--cpus")
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
	return GatewayFiles{Log: gatewayLogPath(sock), PID: gatewayPIDPath(sock), Always: true}, nil
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
	return GatewayFiles{Log: log, PID: pid}, nil
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

// flagInt returns the integer after flag in args, or 0 when it is absent or
// not a number.
func flagInt(args []string, flag string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			v, err := strconv.Atoi(args[i+1])
			if err == nil {
				return v
			}
		}
	}
	return 0
}
