// Package runtime is the seam between brig's product logic and whatever
// actually runs the container.
//
// brig is not a container runtime and does not want to be one. It adds
// workspace-as-home, host-resolved credentials with per-exec forwarding, the
// billing denylist and the git plumbing, and delegates every mechanical
// operation -- boot this image, exec in it, stop it -- to hull on macOS and
// nerdctl on Linux. Both sit behind this one interface, so the logic above it
// is written once and neither OS gets a forked copy.
package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Share is a host directory made visible in the guest.
type Share struct {
	Host  string
	Guest string
	// ReadOnly asks the runtime to export the directory the guest cannot
	// write to. Only meaningful for directories that belong to the host: the
	// workspace is the agent's own and stays writable.
	ReadOnly bool
}

// Var is one environment variable destined for the guest.
//
// The value travels through the child process's own environment, never
// through argv: an implementation puts Name=Value in the environment of the
// runtime process it spawns and names only the bare Name on the command line.
// Values in argv would be readable in `ps` by any process on the host.
type Var struct {
	Name  string
	Value string
	// Secret marks a value brig resolved on the user's behalf -- from the
	// store it owns, or from the host credential -- so that BRIG_ENV_ARGV
	// never puts it in argv whatever it says: hull durably logs every exec's
	// argv to a host file, and an ambient shell value put there deliberately
	// is one thing, but a stored credential outliving the sandbox in a file
	// the user never sees is a different severity of leak.
	Secret bool
}

// RunSpec is a request to boot a sandbox.
type RunSpec struct {
	Name     string
	Image    string
	Pull     string // missing (default) | always | never
	Net      string // none | shared
	Mem      int    // MB
	CPUs     int
	Shares   []Share
	Env      []Var
	GUI      bool
	GUITitle string
	// Hypervisor is the macOS backend to boot on: vz, hvi or qemu. Empty
	// means the runtime's own default. Already resolved by the caller, which
	// is where a setting beats a profile; see wrap.
	Hypervisor string
	// RootfsType is how the guest root reaches the VM: block (an ext4 image
	// the guest writes to), virtiofs or 9pfs. Empty leaves the runtime's own
	// default, which is what almost every profile wants; a profile sets it
	// when the sandbox installs packages and needs a real writable disk.
	// Meaningless to a container runtime, which ignores it.
	RootfsType string
	// Tmpfs are tmpfs mounts to create with the sandbox, as
	// "<path>:<options>". Only the container runtimes take these: hull has no
	// create-time tmpfs, so brig mounts one there with a privileged exec
	// instead. A runtime that cannot honour it ignores it, and the caller
	// verifies the result in the guest either way -- which is what makes the
	// asymmetry safe rather than merely tolerated.
	Tmpfs []string
	// GenericBoot asks the runtime to boot this image as an ordinary OCI
	// image rather than one built to be a guest -- no kernel inside it, no
	// urunc metadata. The runtime supplies the kernel and initrd; see
	// bootArtifacts.
	GenericBoot bool
	// Counted marks an operation that is a user action rather than brig's own
	// plumbing, so telemetry counts one command once. See telemetryEnv.
	Counted bool
}

// ExecSpec is a request to run something inside a sandbox.
type ExecSpec struct {
	Name    string
	Cmd     []string
	Cwd     string
	TTY     bool
	Env     []Var
	Counted bool
	// CanAsk reports whether brig's own stdin is a terminal, so hull can put
	// its consent question to a person before anything is sent. Kept apart from
	// TTY on purpose: TTY gives the guest a pseudo-terminal, which a login shell
	// wants even when brig is driven from a script, whereas the question can
	// only be answered on a real terminal. Reading TTY for both jobs counted a
	// scripted `brig shell` as askable and let hull's on-by-default send the
	// first-boot event. Only Replace reads it; see telemetryEnvFor.
	CanAsk bool
	// Stdin, when set, is fed to the command inside the guest. It is how a
	// credential reaches the guest without appearing in argv: hull durably
	// logs every exec's argv to a host file, so a value there outlives the
	// sandbox in a file the user never sees. Only Feed reads it.
	Stdin io.Reader
	// User runs the command as a guest user other than the image's own --
	// "root", for the one privileged exec that mounts the tmpfs. Empty leaves
	// the image's configured user, which is what every other exec wants.
	User string
}

// Instance is one sandbox as the runtime sees it.
type Instance struct {
	Name  string
	State string
}

// Runtime is the container mechanics brig delegates.
type Runtime interface {
	// Kind is the backend name, e.g. "hull" or "nerdctl".
	Kind() string
	// Bin is the executable actually being driven.
	Bin() string
	// Running reports whether a sandbox of this name is up.
	Running(name string) bool
	// List returns every sandbox the runtime knows about, running or not.
	List() ([]Instance, error)
	// Run boots a sandbox, detached.
	Run(spec RunSpec) error
	// Probe runs a command and reports only whether it succeeded, with all
	// output discarded. Used for reachability checks.
	Probe(spec ExecSpec) bool
	// Output runs a command and returns its stdout.
	Output(spec ExecSpec) (string, error)
	// Feed runs a command with spec.Stdin on its standard input and discards
	// its output, returning only whether it succeeded. The one method that
	// carries a value into the guest off the command line.
	Feed(spec ExecSpec) error
	// Replace hands this process over to the runtime: the exec'd binary takes
	// the terminal, the signals and the exit status. It does not return on
	// success. This is how the agent's own TUI gets a real tty without brig
	// sitting in the middle of it.
	Replace(spec ExecSpec) error
	// Stop stops a sandbox, and Remove clears the instance holding the name.
	Stop(name string) error
	Remove(name string) error
	// LogsHint is the command to suggest when a sandbox will not come up.
	LogsHint(name string) string
}

// Preference is what a profile asked for. A setting in the environment beats
// it, which is brig's usual order: a flag, then BRIG_*, then the profile.
type Preference struct {
	// Bin is the runtime binary a profile named, if any.
	Bin string
}

// Detect picks a runtime for this host, with nothing asked of it.
//
// The commands that work across every sandbox rather than one profile's --
// listing them, removing them all -- use this: there is no profile in hand to
// take a preference from.
func Detect() (Runtime, error) { return DetectFor(Preference{}) }

// DetectFor picks a runtime, honouring what a profile asked for.
//
// BRIG_RUNTIME forces the backend (hull | nerdctl) and BRIG_RUNTIME_BIN forces
// the executable, which is how you point brig at a build that is not on PATH.
// A profile's runtimeBin does the same thing without a variable per shell, and
// loses to the variable when both are set.
func DetectFor(pref Preference) (Runtime, error) {
	kind := os.Getenv("BRIG_RUNTIME")
	if kind == "" {
		kind = defaultKind()
	}
	bin := os.Getenv("BRIG_RUNTIME_BIN")
	if bin == "" && pref.Bin != "" {
		resolved, err := runtimeBinFromProfile(pref.Bin)
		if err != nil {
			return nil, err
		}
		bin = resolved
	}
	switch kind {
	case "hull":
		return newHull(bin)
	case "nerdctl":
		return newNerdctl(bin)
	default:
		return nil, fmt.Errorf("unknown BRIG_RUNTIME %q (want hull or nerdctl)", kind)
	}
}

// runtimeBinFromProfile resolves the path a profile named.
//
// A leading ~ is expanded, because this field is written by hand in a config
// file and that is where people write one. The path is checked here so a
// mistyped one is reported against the profile that carries it, rather than
// surfacing later as a failed exec with no hint about where the path came
// from.
func runtimeBinFromProfile(bin string) (string, error) {
	if strings.HasPrefix(bin, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand %q: no home directory: %w", bin, err)
		}
		bin = filepath.Join(home, bin[2:])
	}
	info, err := os.Stat(bin)
	if err != nil {
		return "", fmt.Errorf("this profile's runtimeBin is %s, which is not there: %w", bin, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("this profile's runtimeBin is %s, which is not an executable", bin)
	}
	return bin, nil
}

// envInArgv is the escape hatch for a runtime build that does not yet take a
// bare KEY. It puts values back on the command line, where `ps` can read
// them, so it is opt-in and says so.
func envInArgv() bool { return os.Getenv("BRIG_ENV_ARGV") == "1" }

// splitEnv turns guest variables into the argv flags and the child-process
// environment that carry them, keeping values out of argv unless the escape
// hatch is set -- and even then, a Var marked Secret stays out of argv, because
// the escape hatch predates this feature and was only ever meant to expose
// ambient shell values, not keychain secrets.
func splitEnv(flag string, vars []Var) (args []string, env []string) {
	for _, v := range vars {
		if envInArgv() && !v.Secret {
			args = append(args, flag, v.Name+"="+v.Value)
			continue
		}
		args = append(args, flag, v.Name)
		env = append(env, v.Name+"="+v.Value)
	}
	return args, env
}

// telemetryEnv attributes events to brig and suppresses the wrapper's own
// plumbing -- reachability probes, ps lookups, cleanup -- so one brig command
// counts once. Only the operations a user asked for are counted. DO_NOT_TRACK
// and the runtime's own opt-out pass through untouched and always win.
//
// Being an operation the user asked for is necessary but not sufficient: an
// operation that runs with no terminal is also not counted until an answer
// about telemetry is on file, because a question nobody can be asked is not a
// question anyone has answered. That second rule is telemetryEnvFor, in
// telemetry.go, and every counted hull call goes through it.
func telemetryEnv(counted bool) []string {
	suppress := "1"
	if counted {
		suppress = ""
	}
	return []string{
		"HULL_TELEMETRY_PRODUCT=brig",
		"HULL_TELEMETRY_SUPPRESS=" + suppress,
	}
}

// mergeEnv layers additions onto the current environment, last one winning.
func mergeEnv(additions ...[]string) []string {
	seen := map[string]int{}
	out := append([]string(nil), os.Environ()...)
	for i, kv := range out {
		if k, _, ok := strings.Cut(kv, "="); ok {
			seen[k] = i
		}
	}
	for _, add := range additions {
		for _, kv := range add {
			k, _, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if i, dup := seen[k]; dup {
				out[i] = kv
				continue
			}
			seen[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}
