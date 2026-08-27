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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoRuntime is the one DetectFor failure that means "nothing is installed"
// rather than "you asked for the wrong thing": no runtime binary was on PATH
// and none was pinned. env and ls degrade to it -- report what they can and
// exit 0 -- and they must key off this with errors.Is, never off "err != nil".
// An unknown BRIG_RUNTIME or a runtimeBin that is not there is a mistake to
// fix, and env is the verb people run to find it, so those still surface with a
// non-zero exit even there. Matching the sentinel rather than any error is what
// stops the day the message or type changes from silently widening the swallow.
var ErrNoRuntime = errors.New("no runtime found on PATH")

// ErrBadRuntime is the other DetectFor failure: a runtime was asked for and is
// wrong, rather than absent. An unknown BRIG_RUNTIME, or a runtimeBin that is
// not on disk or is not executable, wrap this. It is kept apart from
// ErrNoRuntime because the two want different handling -- env and ls degrade
// over ErrNoRuntime and still fail over this -- and because a caller mapping a
// failure to an exit code treats "you have no runtime" and "the runtime you
// named is broken" as the same class to fix, so both reach it with errors.Is.
var ErrBadRuntime = errors.New("runtime unavailable")

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
	Name  string
	Image string
	// Digest is the registry digest verify resolved and checked for Image, or
	// "" when none was resolved. A runtime whose store is addressable by digest
	// boots Image@Digest instead of the tag, so the bytes that boot are the ones
	// that verified; one that is not ignores it and boots the tag. See
	// PinsDigest, and the note on the nerdctl and hull adapters.
	Digest   string
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
	// Running reports whether a sandbox of this name is up, and returns an
	// error when it could not find out at all.
	//
	// Three answers, not two, because a runtime that cannot be asked -- a binary
	// that is not there, a permission error, a daemon that is down -- is not a
	// runtime saying no. Both adapters used to fold the failure into false, so a
	// broken runtime read as an empty machine and EnsureRunning booted a second
	// sandbox onto a workspace the first was still holding (#49). A caller that
	// cannot tell must not boot: false with a non-nil error means "unknown", and
	// the boolean says nothing.
	Running(name string) (bool, error)
	// List returns every sandbox the runtime knows about, running or not.
	List() ([]Instance, error)
	// Run boots a sandbox, detached.
	Run(spec RunSpec) error
	// PinsDigest reports whether Run honours RunSpec.Digest -- that is, whether
	// this runtime's store is addressable by digest, so that booting
	// Image@Digest boots that exact object and resolves it against the local
	// store the way containerd does. brig only claims digest-level verification
	// on a runtime that returns true; on one that returns false it verifies and
	// boots the tag, and says so, rather than promise bytes it cannot pin.
	PinsDigest() bool
	// LocalDigest reports the digest the local store already holds for ref, or
	// "" when it holds nothing under that reference or cannot say. It is how the
	// verify path learns that the copy on disk is a different object from the
	// one the registry serves. Only meaningful on a PinsDigest runtime; the
	// caller does not ask the others.
	LocalDigest(ref string) (string, error)
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
		return nil, fmt.Errorf("%w: unknown BRIG_RUNTIME %q (want hull or nerdctl)", ErrBadRuntime, kind)
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
		return "", fmt.Errorf("%w: this profile's runtimeBin is %s, which is not there: %w", ErrBadRuntime, bin, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%w: this profile's runtimeBin is %s, which is not an executable", ErrBadRuntime, bin)
	}
	return bin, nil
}

// envInArgv is the escape hatch for a runtime build that does not yet take a
// bare KEY. It puts values back on the command line, where `ps` can read
// them, so it is opt-in and says so.
func envInArgv() bool { return os.Getenv("BRIG_ENV_ARGV") == "1" }

// inArgv is the single rule for whether a value travels on the command line:
// the hatch is on, and brig did not resolve the value on the user's behalf.
//
// Written once because two callers need it. splitEnv builds the command line
// from it, and ArgvExposed reports what that command line will carry before
// anything is spawned. A report derived from a second copy of the rule is a
// report that can be wrong in exactly the case it exists for.
func inArgv(v Var) bool { return envInArgv() && !v.Secret }

// ArgvExposed names the variables whose values this invocation would put on
// the runtime's command line, in the order they were given. It returns nothing
// when the hatch is off, and never names a Var marked Secret: splitEnv keeps
// those off the command line whatever the setting says.
//
// Exported so the exposure can be said out loud before the runtime is invoked.
// BRIG_ENV_ARGV is opted into once, in a shell profile, and then remembered by
// nobody: months later such a run looks like every other one, and the only
// places the difference shows are `ps` and the host's own argv log, neither of
// which anyone reads until after something has gone wrong. The names are what
// a caller can act on; the values stay here.
func ArgvExposed(vars []Var) []string {
	var names []string
	for _, v := range vars {
		if inArgv(v) {
			names = append(names, v.Name)
		}
	}
	return names
}

// splitEnv turns guest variables into the argv flags and the child-process
// environment that carry them, keeping values out of argv unless the escape
// hatch is set -- and even then, a Var marked Secret stays out of argv, because
// the escape hatch predates this feature and was only ever meant to expose
// ambient shell values, not keychain secrets.
func splitEnv(flag string, vars []Var) (args []string, env []string) {
	for _, v := range vars {
		if inArgv(v) {
			args = append(args, flag, v.Name+"="+v.Value)
			continue
		}
		args = append(args, flag, v.Name)
		env = append(env, v.Name+"="+v.Value)
	}
	return args, env
}

// withDigest rewrites image to name digest in place of its tag, so the runtime
// boots the exact object verify checked. An empty digest leaves image as it is:
// a runtime that cannot pin, or a path that resolved no digest, still boots the
// tag it was given.
//
// A digest and a tag do not coexist on a reference, so the tag goes first: an
// existing @digest is replaced, and a trailing :tag is cut -- but not a ":port"
// on the registry host, which the slash after the colon distinguishes from a
// tag. verify.refWithDigest does the same for the reference cosign checks; the
// two are kept apart because this one speaks the runtime's ref grammar and that
// one cosign's, and neither package should reach into the other for six lines.
func withDigest(image, digest string) string {
	if digest == "" {
		return image
	}
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	} else if i := strings.LastIndexByte(image, ':'); i >= 0 && !strings.Contains(image[i+1:], "/") {
		image = image[:i]
	}
	return image + "@" + digest
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

// sandboxPrefix is the mark every sandbox brig starts carries, and so the mark
// on anything else brig creates for one. cmd/brig enforces it on the name; the
// adapters read it back to recognise their own leftovers.
const sandboxPrefix = "brig-"

// NetworkPruner is a runtime that makes a network per sandbox and can tidy the
// ones nothing is on any more.
//
// Optional for the same reason TelemetryReporter is: a backend that makes no
// networks should not have to grow a stub method to say so. reset asserts for
// it and skips a runtime that does not implement it.
type NetworkPruner interface {
	// PruneNetworks removes the networks brig made that none of inUse is on,
	// and reports how many went.
	PruneNetworks(inUse []string) int
}
