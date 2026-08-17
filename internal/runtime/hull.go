package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hull drives the macOS microVM runtime.
type hull struct{ bin string }

func newHull(bin string) (Runtime, error) {
	if bin != "" {
		return &hull{bin: bin}, nil
	}
	if p, err := exec.LookPath("hull"); err == nil {
		return &hull{bin: p}, nil
	}
	return nil, fmt.Errorf("no microVM runtime found on PATH. brig drives hull " +
		"on macOS: see https://github.com/brig-sh/brig#macos. " +
		"Or point BRIG_RUNTIME_BIN at a build")
}

func (h *hull) Kind() string { return "hull" }
func (h *hull) Bin() string  { return h.bin }

// hypervisor is vz because that is the backend with a graphical console, a
// virtiofs share per directory and the notarised runner. A profile names
// another -- hvi for the Hypervisor Virtualization Interface, qemu for a qemu
// host -- and BRIG_HYPERVISOR beats the profile. Both are resolved before they
// reach here; see wrap.
func hypervisor(spec RunSpec) string { return orDefault(spec.Hypervisor, "vz") }

// The kernel and initrd a genericBoot profile needs live in boot.go: both
// backends take the same two annotations, so resolving them is not hull's
// business alone.

// pullAssets downloads the published boot bundle by asking hull to do it.
//
// hull needs the same kernel and initrd for its own `hull run`, so it already
// knows the reference, the asset directory and the registry credentials. brig
// driving that is one implementation instead of two that have to agree, and it
// means a genericBoot profile works on a clean machine with no manual step.
//
// Output goes to stderr: this is progress, and brig's stdout belongs to the
// workload.
func (h *hull) pullAssets(dir string) error {
	cmd := exec.Command(h.bin, "assets", "pull")
	// Pin hull to the directory brig resolved. That directory came from hull
	// itself via assetDir, so this is not brig overriding a choice -- it is
	// brig making sure the place it checked and the place hull writes are the
	// same one, even if something changed between the two calls.
	cmd.Env = mergeEnv(telemetryEnv(false), []string{"HULL_BOOT_ASSETS=" + dir})
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s assets pull: %w", h.bin, err)
	}
	return nil
}

// assetDir asks hull where its boot assets live.
//
// They sit under hull's store, so the answer moves with --store-dir and with
// hull's own environment variables. brig cannot see any of that, and a path
// compiled in here would drift the moment hull changed its mind -- silently,
// because brig would find an empty directory, fetch a second copy of the same
// bundle into it, and boot a kernel hull knows nothing about.
//
// Errors are the caller's to shrug off: an older hull has no `assets dir`, and
// that should fall back to the historical path rather than refuse to boot.
func (h *hull) assetDir() (string, error) {
	cmd := exec.Command(h.bin, "assets", "dir")
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s assets dir: %w", h.bin, err)
	}
	dir := strings.TrimSpace(out.String())
	if dir == "" {
		return "", fmt.Errorf("%s assets dir printed nothing", h.bin)
	}
	return dir, nil
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
	hv := hypervisor(spec)
	if err := supports(spec, hv); err != nil {
		return err
	}
	net := orDefault(spec.Net, "shared")
	// hvi has no egress without the gateway, so brig guarantees one rather
	// than booting a sandbox whose network silently swallows every
	// connection. See gateway.go.
	gatewaySock, gatewayCidr := "", ""
	if hv == "hvi" && net != "none" {
		sock, err := ensureGateway(h.bin)
		if err != nil {
			return err
		}
		// hull takes the two together and configures the guest statically, so
		// the address is ours to assign. See gatewayip.go.
		cidr, err := gatewayCIDR(spec.Name)
		if err != nil {
			return err
		}
		gatewaySock, gatewayCidr = sock, cidr
	}
	args, envVals, err := runArgs(spec, hv, net, gatewaySock, gatewayCidr, h.assetDir, h.pullAssets)
	if err != nil {
		return err
	}

	cmd := exec.Command(h.bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(spec.Counted), envVals)
	cmd.Stdout = nil // the instance id is not interesting; failures explain themselves
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// supports rejects a spec the chosen backend cannot honour, before anything
// is started. hull would refuse this too, but only at boot and without
// naming the variable that caused it -- and the user who set BRIG_HYPERVISOR
// is the only one who can undo it.
func supports(spec RunSpec, hv string) error {
	if spec.GUI && hv != "vz" {
		return fmt.Errorf("this profile opens a graphical window, which only the vz backend provides "+
			"(BRIG_HYPERVISOR is %q); unset it to run this profile", hv)
	}
	return nil
}

// runArgs is the one place the run command line is built, so that what a test
// asserts and what boots a sandbox cannot drift apart. It returns the argv
// and the environment carrying the guest variables, which never travel in
// argv; see splitEnv.
//
// fetch populates the boot assets when a genericBoot profile finds them
// missing. It is a parameter rather than a method so that this stays a pure
// argv-building function, drivable by a test with no hull on PATH; pass nil to
// resolve the assets without ever downloading.
func runArgs(spec RunSpec, hv, net, gatewaySock, gatewayCidr string, locate assetLocator, fetch assetFetcher) (args, env []string, err error) {
	args = []string{"run", "--detach", "--name", spec.Name,
		"--hypervisor", hv,
		"--net", net,
		"--pull", orDefault(spec.Pull, "missing"),
		"--mem", strconv.Itoa(spec.Mem),
		"--cpus", strconv.Itoa(spec.CPUs),
	}
	if spec.RootfsType != "" {
		args = append(args, "--rootfs-type", spec.RootfsType)
	}
	if spec.GenericBoot {
		annotations, err := bootAnnotations(locate, fetch)
		if err != nil {
			return nil, nil, err
		}
		for _, kv := range annotations {
			args = append(args, "--annotation", kv)
		}
	}
	// hull refuses one without the other: the socket says which network to
	// join, the CIDR says which address to take on it.
	if gatewaySock != "" {
		args = append(args, "--gateway-sock", gatewaySock, "--gateway-cidr", gatewayCidr)
	}
	for _, s := range spec.Shares {
		spec := s.Host + ":" + s.Guest
		if s.ReadOnly {
			// hull holds this on vz through VZSharedDirectory and on hvi
			// through the virtio-fs export, and refuses it outright on qemu
			// rather than mounting read-write behind our back.
			spec += ":ro"
		}
		args = append(args, "--shared-dir", spec)
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
	return args, envVals, nil
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

// agentCallTimeout bounds a single question put to the guest agent.
//
// It has to exist because the agent socket answers before the guest does. The
// socket belongs to the VMM, not to the guest, so it accepts as soon as the
// VMM is up -- seconds before the in-guest agent binds its listener. An exec
// aimed at that window sends its open request into nothing and then blocks in
// the frame loop with no deadline, forever.
//
// A caller that retries cannot see that: waitReady's own deadline is only
// consulted between probes, so the first probe never returning means the
// timeout never fires and `brig run` hangs with nothing printed. Bounding the
// call here is what makes the retry loop above a retry loop.
//
// Generous on purpose. This is not how long the guest may take to come up --
// that is ReadyTimeout, across many probes -- only how long one probe waits
// before deciding this attempt landed too early and another one is due.
const agentCallTimeout = 5 * time.Second

// agentCallWaitDelay is how long Run may keep waiting once the deadline has
// already killed the command.
//
// Killing the process is not enough on its own. When output is collected into
// a buffer rather than a file, Run does not return until the copy from the
// pipe ends, and the pipe stays open as long as anything holds its write end
// -- a grandchild the killed process left behind holds it just as well as the
// process did. Without this the timeout kills hull and Run blocks anyway,
// which is the same hang wearing a different hat; the test for it is exactly
// how this was found.
const agentCallWaitDelay = time.Second

// agentCall builds a command to the guest agent that is guaranteed to return.
func (h *hull) agentCall(spec ExecSpec) (*exec.Cmd, context.CancelFunc, []string) {
	args, envVals := h.execArgs(spec)
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.WaitDelay = agentCallWaitDelay
	return cmd, cancel, envVals
}

func (h *hull) Probe(spec ExecSpec) bool {
	cmd, cancel, envVals := h.agentCall(spec)
	defer cancel()
	cmd.Env = mergeEnv(telemetryEnv(false), envVals)
	return cmd.Run() == nil
}

func (h *hull) Output(spec ExecSpec) (string, error) {
	// Bounded for the same reason as Probe: this asks the guest which
	// workspace it mounts, on the same socket, and its caller treats silence
	// as "cannot say" rather than waiting on it.
	cmd, cancel, envVals := h.agentCall(spec)
	defer cancel()
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

func (h *hull) Stop(name string) error { return h.quiet("stop", name, true) }

// Remove also gives the sandbox's gateway address back. A stopped sandbox
// keeps its address so that starting it again is the same machine on the same
// network; a removed one has no such claim.
func (h *hull) Remove(name string) error {
	err := h.quiet("rm", name, false)
	releaseGatewayIP(name)
	return err
}

// quiet runs a verb whose success is the whole of its output, keeping hull's
// own explanation for the caller that has to report a failure.
//
// Quiet means nothing is printed when it works, not that hull is silenced: the
// command was built with no Stdout or Stderr at all, so a stop that failed
// took its reason -- the one sentence saying which instance and why -- to
// /dev/null, and the caller was left holding "exit status 1". Both streams are
// captured and folded into the error instead, which is where anyone reading a
// failed `brig stop` will look.
func (h *hull) quiet(verb, name string, counted bool) error {
	cmd := exec.Command(h.bin, verb, name)
	cmd.Env = mergeEnv(telemetryEnv(counted))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(out.String()); said != "" {
			return fmt.Errorf("%s %s %s: %w: %s", h.bin, verb, name, err, firstLines(said, 3))
		}
		return fmt.Errorf("%s %s %s: %w", h.bin, verb, name, err)
	}
	return nil
}

// firstLines keeps a runtime's explanation to something that fits in an error
// message: hull says what went wrong in its first line or two, and a stack
// trace or a usage dump behind it would bury the sentence that matters.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
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
