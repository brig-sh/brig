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
	"os"
	"strings"
)

// Share is a host directory made visible in the guest.
type Share struct {
	Host  string
	Guest string
}

// Var is one environment variable destined for the guest.
//
// The value travels through the child process's own environment, never
// through argv: an implementation puts Name=Value in the environment of the
// runtime process it spawns and names only the bare Name on the command line.
// Values in argv would be readable in `ps` by any process on the host, which
// is the limitation the bash wrapper documented and this package exists to
// close.
type Var struct {
	Name  string
	Value string
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
	// Bin is the executable actually being driven, which on macOS may still
	// be urunc-macos until hull lands.
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

// Detect picks a runtime for this host.
//
// BRIG_RUNTIME forces the backend (hull | nerdctl) and BRIG_RUNTIME_BIN forces
// the executable, which is how you point brig at a build that is not on PATH.
func Detect() (Runtime, error) {
	kind := os.Getenv("BRIG_RUNTIME")
	if kind == "" {
		kind = defaultKind()
	}
	bin := os.Getenv("BRIG_RUNTIME_BIN")
	switch kind {
	case "hull":
		return newHull(bin)
	case "nerdctl":
		return newNerdctl(bin)
	default:
		return nil, fmt.Errorf("unknown BRIG_RUNTIME %q (want hull or nerdctl)", kind)
	}
}

// envInArgv is the escape hatch for a runtime build that does not yet take a
// bare KEY. It puts values back on the command line, where `ps` can read
// them, so it is opt-in and says so.
func envInArgv() bool { return os.Getenv("BRIG_ENV_ARGV") == "1" }

// splitEnv turns guest variables into the argv flags and the child-process
// environment that carry them, keeping values out of argv unless the escape
// hatch is set.
func splitEnv(flag string, vars []Var) (args []string, env []string) {
	for _, v := range vars {
		if envInArgv() {
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
// Both spellings are set. hull reads HULL_TELEMETRY_*; the urunc-macos builds
// brig still falls back to read URUNC_TELEMETRY_*, and a variable the runtime
// does not know is simply ignored. Drop the URUNC_ pair when that fallback
// goes.
func telemetryEnv(counted bool) []string {
	suppress := "1"
	if counted {
		suppress = ""
	}
	return []string{
		"HULL_TELEMETRY_PRODUCT=brig",
		"HULL_TELEMETRY_SUPPRESS=" + suppress,
		"URUNC_TELEMETRY_PRODUCT=brig",
		"URUNC_TELEMETRY_SUPPRESS=" + suppress,
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
