package runtime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// nerdctl drives the Linux path. It is the same product logic over
// containerd: brig still resolves credentials on the host, forwards them per
// exec and mounts the workspace as the guest home, and nerdctl only does the
// container mechanics underneath. No behaviour is re-implemented here -- the
// only difference from hull is the command line.
//
// The sandbox is a microVM there too, not a namespace: containerd hands the
// container to the urunc shim, which boots it. That keeps the isolation story
// the same on both operating systems, which is the whole reason brig has one
// runtime interface rather than two behaviours.
type nerdctl struct{ bin string }

// defaultRuntime is the containerd shim that boots the sandbox as a microVM.
// BRIG_CONTAINERD_RUNTIME overrides it -- runc, for instance, when you want a
// plain container and accept what that costs you.
func containerdRuntime() string {
	if v := os.Getenv("BRIG_CONTAINERD_RUNTIME"); v != "" {
		return v
	}
	return "io.containerd.urunc.v2"
}

func newNerdctl(bin string) (Runtime, error) {
	if bin != "" {
		return &nerdctl{bin: bin}, nil
	}
	for _, candidate := range []string{"nerdctl", "docker"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return &nerdctl{bin: p}, nil
		}
	}
	return nil, fmt.Errorf("%w: install nerdctl, "+
		"or point BRIG_RUNTIME_BIN at one", ErrNoRuntime)
}

func (n *nerdctl) Kind() string { return "nerdctl" }
func (n *nerdctl) Bin() string  { return n.bin }

// PinsDigest is true here: containerd's store is content-addressed, so a
// reference of the form repo@sha256:... resolves to that exact object and, when
// it is already present, is served from the store without a re-pull. That is
// what lets brig boot the digest it verified rather than the tag, and it is the
// property hull's store does not share -- see the hull adapter.
func (n *nerdctl) PinsDigest() bool { return true }

// LocalDigest reports the digest the local store holds for ref.
//
// image inspect's RepoDigests is the reference the image was pulled by, digest
// and all: for a tag that is repo@sha256:<the digest the registry served then>,
// which is the same object cosign resolves the tag to now, so the two are
// comparable. Both nerdctl and docker answer the dockercompat shape, so one
// format string serves whichever binary newNerdctl settled on.
//
// A miss is not an error: an image that was never pulled, or one built locally
// with no repo digest at all, simply has nothing on disk to compare, so the
// verify path treats "" as "no local copy" and does not raise a mismatch over
// it. Only a genuine failure to ask is worth returning.
func (n *nerdctl) LocalDigest(ref string) (string, error) {
	cmd := exec.Command(n.bin, "image", "inspect", "--format", "{{index .RepoDigests 0}}", ref)
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// The overwhelmingly common cause is that the tag is simply not in the
		// store yet, which inspect reports as a failure. That is a miss, not a
		// fault, so it must not stop a boot.
		return "", nil
	}
	return repoDigest(out.String()), nil
}

// repoDigest pulls the bare digest out of an image inspect RepoDigests entry.
//
// The entry is repo@sha256:..., and only the digest half is comparable against
// what cosign resolved -- the repo half is just a name. A pulled-by-tag image
// with no recorded digest prints Go's zero value for the missing slice element
// rather than an empty string, so that is a miss too, reported as "".
func repoDigest(out string) string {
	line := strings.TrimSpace(out)
	if line == "" || line == "<no value>" {
		return ""
	}
	if _, digest, found := strings.Cut(line, "@"); found {
		return digest
	}
	return ""
}

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

// isDocker reports whether the binary we ended up with is docker rather than
// nerdctl. newNerdctl accepts either, and they differ where it matters below:
// docker does not carry OCI annotations through to the runtime, which is the
// same reason urunc grew urunc.json.
func (n *nerdctl) isDocker() bool { return filepath.Base(n.bin) == "docker" }

// RootfsType is ignored here, deliberately: it selects how a VM reaches its
// root filesystem, and that is urunc's decision on Linux rather than something
// nerdctl has a flag for. GenericBoot is not ignored -- urunc reads the same
// two annotations from the container's OCI spec that hull takes on its command
// line, so the Linux path is a pass-through.
func (n *nerdctl) Run(spec RunSpec) error {
	args, envVals, err := n.runArgs(spec)
	if err != nil {
		return err
	}
	cmd := exec.Command(n.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runArgs is the one place the run command line is built, for the same reason
// the hull adapter has one: a test and a boot cannot then drift apart.
func (n *nerdctl) runArgs(spec RunSpec) (args, env []string, err error) {
	args = []string{"run", "--detach", "--name", spec.Name,
		"--runtime", containerdRuntime(),
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
	if spec.GenericBoot {
		// urunc reads these from the container's OCI spec, so nerdctl only has
		// to carry them through. docker is refused rather than attempted: it
		// does not pass annotations to the runtime, so the sandbox would boot
		// without a kernel and fail somewhere further away from the cause.
		if n.isDocker() {
			return nil, nil, fmt.Errorf("this profile boots an unmodified image, which needs the " +
				"kernel passed as an OCI annotation; docker does not carry annotations through to " +
				"the runtime. Use nerdctl, or point BRIG_RUNTIME_BIN at it")
		}
		// oras rather than hull: hull does not build on Linux, so there is
		// nothing to ask. See bootfetch.go.
		annotations, err := bootAnnotations(nil, orasFetch)
		if err != nil {
			return nil, nil, err
		}
		for _, kv := range annotations {
			args = append(args, "--annotation", kv)
		}
	}
	for _, s := range spec.Shares {
		mount := s.Host + ":" + s.Guest
		if s.ReadOnly {
			mount += ":ro"
		}
		args = append(args, "-v", mount)
	}
	for _, t := range spec.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	envArgs, envVals := splitEnv("-e", spec.Env)
	args = append(args, envArgs...)
	// A container exits when its command does, and the sandbox has to outlive
	// the exec that uses it -- the whole point is that the VM keeps running
	// between invocations. Park it on a shell that never returns.
	//
	// The image is booted by the verified digest when verify resolved one, so
	// the object that boots is the object that was checked, whatever the tag has
	// moved to since. containerd resolves the digest against its store, so a
	// copy already present is not re-pulled.
	args = append(args, withDigest(spec.Image, spec.Digest), "sleep", "infinity")
	return args, envVals, nil
}

func (n *nerdctl) execArgs(spec ExecSpec) (args, env []string) {
	args = []string{"exec", "-i"}
	if spec.TTY {
		args = append(args, "-t")
	}
	if spec.Cwd != "" {
		args = append(args, "-w", spec.Cwd)
	}
	if spec.User != "" {
		args = append(args, "-u", spec.User)
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

// Feed runs a command with a value on its standard input. execArgs already
// carries -i, so the exec's stdin is open for spec.Stdin to fill; there is no
// agent-socket accept-before-guest window here, so unlike hull's Feed this
// needs no separate deadline.
func (n *nerdctl) Feed(spec ExecSpec) error {
	args, envVals := n.execArgs(spec)
	cmd := exec.Command(n.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	cmd.Stdin = spec.Stdin
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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
