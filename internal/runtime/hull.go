package runtime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// hull drives the macOS microVM runtime: hull, or urunc-macos, which is what
// the same binary is still called until the brig/hull split lands. Resolution
// prefers hull and falls back, so a host with either one works and a host
// with both gets the new name.
type hull struct{ bin string }

func newHull(bin string) (Runtime, error) {
	if bin != "" {
		return &hull{bin: bin}, nil
	}
	for _, candidate := range []string{"hull", "urunc-macos"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return &hull{bin: p}, nil
		}
	}
	// Naming the two spellings matters while the runtime is mid-rename: hull
	// is what it will be called, urunc-macos is what is installed today.
	return nil, fmt.Errorf("no microVM runtime found on PATH. brig drives hull " +
		"on macOS, which ships today as urunc-macos: see " +
		"https://github.com/brig-sh/brig#macos. Or point BRIG_RUNTIME_BIN at a build")
}

func (h *hull) Kind() string { return "hull" }
func (h *hull) Bin() string  { return h.bin }

// hypervisor is vz because that is the backend with a graphical console, a
// virtiofs share per directory and the notarised runner. BRIG_HYPERVISOR
// overrides it for a qemu host.
func hypervisor() string {
	if v := os.Getenv("BRIG_HYPERVISOR"); v != "" {
		return v
	}
	return "vz"
}

// Running parses `ps` rather than asking for one instance, because a stopped
// instance still holds its name and must not read as running.
func (h *hull) Running(name string) bool {
	cmd := exec.Command(h.bin, "ps")
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == name && f[1] == "running" {
			return true
		}
	}
	return false
}

// List reads the same table Running does. A stopped instance still holds its
// name, so it belongs in the listing -- that is exactly the thing a user
// needs to see before wondering why a name is taken.
func (h *hull) List() ([]Instance, error) {
	cmd := exec.Command(h.bin, "ps", "-a")
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// -a is not universal across runtime versions; fall back to the plain
		// listing rather than reporting no sandboxes at all.
		cmd = exec.Command(h.bin, "ps")
		cmd.Env = mergeEnv(telemetryEnv(false))
		out.Reset()
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, err
		}
	}
	var list []Instance
	for i, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// Skip a header row, whatever it is called.
		if i == 0 && (strings.EqualFold(f[0], "name") || strings.EqualFold(f[0], "id")) {
			continue
		}
		list = append(list, Instance{Name: f[0], State: f[1]})
	}
	return list, nil
}

func (h *hull) Run(spec RunSpec) error {
	args := []string{"run", "--detach", "--name", spec.Name,
		"--hypervisor", hypervisor(),
		"--net", orDefault(spec.Net, "shared"),
		"--pull", orDefault(spec.Pull, "missing"),
		"--mem", strconv.Itoa(spec.Mem),
		"--cpus", strconv.Itoa(spec.CPUs),
	}
	for _, s := range spec.Shares {
		args = append(args, "--shared-dir", s.Host+":"+s.Guest)
	}
	if spec.GUI {
		args = append(args, "--gui")
		if spec.GUITitle != "" {
			args = append(args, "--gui-title", spec.GUITitle)
		}
	}
	envArgs, envVals := splitEnv("--env", spec.Env)
	args = append(args, envArgs...)
	args = append(args, spec.Image)

	cmd := exec.Command(h.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	cmd.Stdout = nil // the instance id is not interesting; failures explain themselves
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execArgs is the one place the exec command line is built, so the probe, the
// captured read and the terminal handover cannot drift apart.
func (h *hull) execArgs(spec ExecSpec) (args, env []string) {
	args = []string{"exec"}
	if spec.TTY {
		args = append(args, "-t")
	}
	if spec.Cwd != "" {
		args = append(args, "--cwd", spec.Cwd)
	}
	envArgs, envVals := splitEnv("--env", spec.Env)
	args = append(args, envArgs...)
	args = append(args, spec.Name, "--")
	return append(args, spec.Cmd...), envVals
}

func (h *hull) Probe(spec ExecSpec) bool {
	args, envVals := h.execArgs(spec)
	cmd := exec.Command(h.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(false), envVals)
	return cmd.Run() == nil
}

func (h *hull) Output(spec ExecSpec) (string, error) {
	args, envVals := h.execArgs(spec)
	cmd := exec.Command(h.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Replace hands the process over. On success it does not return: the agent's
// TUI gets the real terminal, ^C reaches it rather than brig, and its exit
// status is brig's exit status without any relaying.
func (h *hull) Replace(spec ExecSpec) error {
	args, envVals := h.execArgs(spec)
	argv := append([]string{h.bin}, args...)
	return syscall.Exec(h.bin, argv, mergeEnv(telemetryEnv(spec.Counted), envVals))
}

func (h *hull) Stop(name string) error   { return h.quiet("stop", name, true) }
func (h *hull) Remove(name string) error { return h.quiet("rm", name, false) }

func (h *hull) quiet(verb, name string, counted bool) error {
	cmd := exec.Command(h.bin, verb, name)
	cmd.Env = mergeEnv(telemetryEnv(counted))
	return cmd.Run()
}

func (h *hull) LogsHint(name string) string {
	return fmt.Sprintf("%s logs %s", h.bin, name)
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
