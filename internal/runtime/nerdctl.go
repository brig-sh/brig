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

// nerdctl drives the Linux path. It is the same product logic over
// containerd: brig still resolves credentials on the host, forwards them per
// exec and mounts the workspace as the guest home, and nerdctl only does the
// container mechanics underneath. No behaviour is re-implemented here -- the
// only difference from hull is the command line.
type nerdctl struct{ bin string }

func newNerdctl(bin string) (Runtime, error) {
	if bin != "" {
		return &nerdctl{bin: bin}, nil
	}
	for _, candidate := range []string{"nerdctl", "docker"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return &nerdctl{bin: p}, nil
		}
	}
	return nil, fmt.Errorf("no container runtime found: install nerdctl, " +
		"or point BRIG_RUNTIME_BIN at one")
}

func (n *nerdctl) Kind() string { return "nerdctl" }
func (n *nerdctl) Bin() string  { return n.bin }

func (n *nerdctl) Running(name string) bool {
	cmd := exec.Command(n.bin, "ps", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func (n *nerdctl) List() ([]Instance, error) {
	cmd := exec.Command(n.bin, "ps", "-a", "--format", "{{.Names}}\t{{.Status}}")
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var list []Instance
	for _, line := range strings.Split(out.String(), "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name == "" {
			continue
		}
		list = append(list, Instance{Name: name, State: state})
	}
	return list, nil
}

func (n *nerdctl) Run(spec RunSpec) error {
	args := []string{"run", "--detach", "--name", spec.Name,
		"--memory", strconv.Itoa(spec.Mem) + "m",
		"--cpus", strconv.Itoa(spec.CPUs),
	}
	switch spec.Pull {
	case "always", "never", "missing":
		args = append(args, "--pull", spec.Pull)
	}
	if spec.Net == "none" {
		args = append(args, "--network", "none")
	}
	for _, s := range spec.Shares {
		args = append(args, "-v", s.Host+":"+s.Guest)
	}
	envArgs, envVals := splitEnv("-e", spec.Env)
	args = append(args, envArgs...)
	// A container exits when its command does, and the sandbox has to outlive
	// the exec that uses it -- the whole point is that the VM keeps running
	// between invocations. Park it on a shell that never returns.
	args = append(args, spec.Image, "sleep", "infinity")

	cmd := exec.Command(n.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (n *nerdctl) execArgs(spec ExecSpec) (args, env []string) {
	args = []string{"exec", "-i"}
	if spec.TTY {
		args = append(args, "-t")
	}
	if spec.Cwd != "" {
		args = append(args, "-w", spec.Cwd)
	}
	envArgs, envVals := splitEnv("-e", spec.Env)
	args = append(args, envArgs...)
	args = append(args, spec.Name)
	return append(args, spec.Cmd...), envVals
}

func (n *nerdctl) Probe(spec ExecSpec) bool {
	args, envVals := n.execArgs(spec)
	cmd := exec.Command(n.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(false), envVals)
	return cmd.Run() == nil
}

func (n *nerdctl) Output(spec ExecSpec) (string, error) {
	args, envVals := n.execArgs(spec)
	cmd := exec.Command(n.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (n *nerdctl) Replace(spec ExecSpec) error {
	args, envVals := n.execArgs(spec)
	argv := append([]string{n.bin}, args...)
	return syscall.Exec(n.bin, argv, mergeEnv(telemetryEnv(spec.Counted), envVals))
}

func (n *nerdctl) Stop(name string) error   { return n.quiet("stop", name) }
func (n *nerdctl) Remove(name string) error { return n.quiet("rm", name) }

func (n *nerdctl) quiet(verb, name string) error {
	cmd := exec.Command(n.bin, verb, name)
	cmd.Env = mergeEnv(telemetryEnv(false))
	return cmd.Run()
}

func (n *nerdctl) LogsHint(name string) string {
	return fmt.Sprintf("%s logs %s", n.bin, name)
}
