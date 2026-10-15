package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// HostKind is the runtime kind that runs the agent as a host process under the
// operating system's own sandbox, instead of booting a microVM. It is chosen by
// name, with BRIG_RUNTIME=host or isolation: process in a profile, and never as
// a fallback.
const HostKind = "host"

// The values a profile's isolation field takes.
const (
	IsolationMicroVM = "microvm"
	IsolationProcess = "process"
)

// The host kind's network modes, as its capability records name them. They
// stand where a backend stands for the other kinds, because the host kind has
// no backend and its answer depends on the mode.
const (
	hostOffline  = "offline"
	hostBrokered = "brokered"
)

// hostWhy is why the host kind cannot enforce a policy that carries rules.
// Measured on macOS 26: Seatbelt refuses a profile whose remote address is
// anything but "*" or "localhost".
const hostWhy = "the process sandbox matches addresses, not names, and of addresses only " +
	"loopback: Seatbelt cannot hold a sandbox to a hostname or an address range"

// RunsImage reports whether rt boots the profile's image. The host kind runs
// the profile's binary from PATH, so there is no image to verify or pull.
func RunsImage(rt Runtime) bool { return rt == nil || rt.Kind() != HostKind }

// hostIsolation is the ISOLATION row of every host sandbox.
func hostIsolation() Isolation { return Isolation{BoundaryProcess, "shared kernel"} }

// hostSupports refuses what a process sandbox cannot honour, before anything
// is written.
func hostSupports(spec RunSpec) error {
	if spec.GUI {
		return errors.New("this profile opens a graphical window, and the host kind has no " +
			"console to show one; run it in a microVM (isolation: microvm)")
	}
	if len(spec.Publish) > 0 {
		return fmt.Errorf("this sandbox publishes %s, and the host kind has no network of its "+
			"own to publish from; withdraw the port with `brig network unpublish`, or run it "+
			"in a microVM", spec.Publish[0])
	}
	if len(spec.Tmpfs) > 0 {
		return errors.New("this profile declares volumes: or files:, which a microVM holds in a " +
			"tmpfs, and the host kind cannot mount one. Run it in a microVM (isolation: microvm)")
	}
	return requireEgress(spec, RunPath{HostKind, hostEgressMode(spec)})
}

// hostEgressMode is the record a policy on this run reads. Seatbelt can hold
// a sandbox to no network, or to loopback by port, and nothing finer. A policy
// with no rule asks no more than that, so it is enforced; a policy with a rule
// is not.
func hostEgressMode(spec RunSpec) string {
	switch {
	case spec.Net == "none":
		return hostOffline
	case spec.Net != "shared" && len(spec.Egress.Allow) == 0 && len(spec.Egress.Deny) == 0:
		return hostBrokered
	}
	return anyBackend
}

// resolverSocket is the socket of mDNSResponder, which every name lookup on
// macOS goes through. Denying it breaks DNS. It is the one unix socket a host
// sandbox may connect to: allowing all of them hands the agent your SSH agent
// and brigd.
const resolverSocket = "/private/var/run/mDNSResponder"

// sandboxPolicy is what a Seatbelt profile is generated from. Every path in it
// is real: Seatbelt matches the path the kernel resolved, so a deny written
// through a symlink would match nothing.
type sandboxPolicy struct {
	// Net is the runtime's posture: none, shared or isolated.
	Net string
	// Filtered is true when a policy applies, which takes shared away.
	Filtered      bool
	LoopbackPorts []int
	// StateDir is ~/.brig/host and SecretDir the instance's secret
	// directory. Both are denied for read and write.
	StateDir  string
	SecretDir string
	// Protect are brig's own directories: its state, its profiles and its
	// policies. They are denied for write, so the agent cannot change what
	// the next run of brig reads.
	Protect []string
	// Shares are written to again after Protect, because the guest home is
	// inside brig's state directory.
	Shares []string
	// Ancestors are the directories above StateDir and Protect that you can
	// rename. Renaming one moves the whole tree out from under a subpath
	// rule, so each is denied for write by its literal path.
	Ancestors []string
}

// sandboxProfile writes the Seatbelt profile for one host sandbox.
//
// The base is allow default: the agent runs as you and sees your files, and
// only the things below are taken away. A later rule wins in Seatbelt, so the
// shares come after Protect, and the denies that must hold whatever else is
// allowed come last.
func sandboxProfile(p sandboxPolicy) (string, error) {
	paths := append([]string{p.StateDir, p.SecretDir}, p.Protect...)
	paths = append(append(paths, p.Shares...), p.Ancestors...)
	for _, path := range paths {
		if err := sbplPath(path); err != nil {
			return "", err
		}
	}
	ports := slices.Clone(p.LoopbackPorts)
	slices.Sort(ports)
	ports = slices.Compact(ports)
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("loopback port %d is not a port", port)
		}
	}

	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	switch {
	case p.Net == "none":
		b.WriteString("(deny network-outbound)\n")
	case p.Net == "shared" && !p.Filtered:
		b.WriteString("(deny network-outbound (remote unix-socket))\n")
		fmt.Fprintf(&b, "(allow network-outbound (remote unix-socket (path-literal %q)))\n", resolverSocket)
	default:
		b.WriteString("(deny network-outbound)\n")
		fmt.Fprintf(&b, "(allow network-outbound (remote unix-socket (path-literal %q)))\n", resolverSocket)
		for _, port := range ports {
			fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
		}
	}
	// Nothing listens: a socket bound on any address takes connections from
	// the network this Mac is on.
	b.WriteString("(deny network-inbound)\n")
	// A process outside the sandbox, the credential broker included, runs as
	// you and would take a signal from it. Only processes in the same
	// sandbox can be signalled.
	b.WriteString("(deny signal)\n(allow signal (target same-sandbox))\n")
	// open(1) and AppleEvents hand work to a process outside the sandbox. An
	// app started through LaunchServices runs unconfined.
	b.WriteString("(deny lsopen)\n(deny appleevent-send)\n")
	for _, d := range p.Protect {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", d)
	}
	for _, d := range p.Shares {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", d)
	}
	for _, d := range p.Ancestors {
		fmt.Fprintf(&b, "(deny file-write* (literal %q))\n", d)
	}
	// Every bpf device, not only the first four: macOS clones more on
	// demand, and each one can read and inject packets.
	b.WriteString("(deny file-read* file-write* (regex #\"^/dev/bpf[0-9]+$\"))\n")
	// The login keychain answers any process that runs as you, so without
	// this the agent reads the secrets brig stores with `security`.
	b.WriteString("(deny mach-lookup (global-name \"com.apple.SecurityServer\"))\n")
	fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %q))\n", p.SecretDir)
	fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %q))\n", p.StateDir)
	return b.String(), nil
}

// sbplPath refuses a path that cannot be written as an SBPL string as it is.
// A quote or a backslash would end or change the literal, so the deny would
// name some other path.
func sbplPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q is not an absolute path", p)
	}
	if strings.ContainsAny(p, "\"\\") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%q has a character a sandbox profile cannot hold", p)
	}
	return nil
}

// hostRecord is spec.json: what Run fixed for the sandbox. It carries no
// environment, so no credential reaches the disk through it.
type hostRecord struct {
	Name          string      `json:"name"`
	Net           string      `json:"net"`
	LoopbackPorts []int       `json:"loopbackPorts,omitempty"`
	Shares        []hostShare `json:"shares"`
}

// hostShare is one share, by the guest path brig speaks and the real host
// path the process sees.
type hostShare struct {
	Guest string `json:"guest"`
	Host  string `json:"host"`
}

// hostPath maps a guest path to the host path of the share it is under, and
// returns p unchanged when no share covers it. A host process sees the real
// paths, so brig maps /work/<name> and the guest home back to them. The
// longest guest path wins, so a hostmount inside the home maps through its
// own share.
func hostPath(shares []hostShare, p string) string {
	best := -1
	for i, s := range shares {
		if (p == s.Guest || strings.HasPrefix(p, strings.TrimSuffix(s.Guest, "/")+"/")) &&
			(best < 0 || len(s.Guest) > len(shares[best].Guest)) {
			best = i
		}
	}
	if best < 0 {
		return p
	}
	return shares[best].Host + strings.TrimPrefix(p, shares[best].Guest)
}

// hostPathList is hostPath over a colon-separated list, such as PATH.
func hostPathList(shares []hostShare, v string) string {
	parts := strings.Split(v, ":")
	for i, part := range parts {
		parts[i] = hostPath(shares, part)
	}
	return strings.Join(parts, ":")
}

// hostBaseEnv names what the agent keeps of brig's own environment. The rest
// stays behind: the shell that runs brig may export credentials nobody named
// for the agent.
var hostBaseEnv = []string{"PATH", "TERM", "COLORTERM", "LANG", "TZ", "USER", "LOGNAME",
	"SHELL", "TMPDIR", "__CF_USER_TEXT_ENCODING"}

// hostEnv is the whole environment of a host sandbox process: the allowed
// part of brig's own, then the spec's variables with guest paths mapped to
// host ones. A secret value is never rewritten.
func hostEnv(vars []Var, shares []hostShare) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(hostBaseEnv, k) || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	for _, v := range vars {
		val := v.Value
		if !v.Secret {
			val = hostPathList(shares, val)
		}
		env = withoutEnv(env, v.Name)
		env = append(env, v.Name+"="+val)
	}
	return env
}

// hostScript enters the working directory inside the sandbox and runs the
// command. The command's words reach sh as positional parameters, so sh never
// parses them, and exec -- runs a first word such as -l as a command. A
// directory that cannot be entered exits 125, apart from exec's 126 and 127.
const hostScript = `cd -- "$1" || { printf 'brig: cannot enter %s\n' "$1" >&2; exit 125; }; shift; exec -- "$@"`

// hostCommand maps a command brig names by its Linux path, such as /bin/true,
// to its name when this host has nothing at that path. macOS keeps those
// binaries elsewhere, and PATH finds them.
func hostCommand(cmd []string) []string {
	if len(cmd) == 0 || !filepath.IsAbs(cmd[0]) {
		return cmd
	}
	if _, err := os.Stat(cmd[0]); err == nil {
		return cmd
	}
	return append([]string{filepath.Base(cmd[0])}, cmd[1:]...)
}

// hostArgv is the one command line every host exec runs. A value never goes
// on it, only the command and its working directory.
func hostArgv(bin, profile, cwd string, cmd []string) []string {
	if cwd == "" {
		cwd = "."
	}
	return append([]string{bin, "-f", profile, "/bin/sh", "-c", hostScript, "brig-host", cwd}, hostCommand(cmd)...)
}

// hostDir is where host sandboxes live: ~/.brig/host, or host under
// BRIG_STATE_DIR.
func hostDir() (string, error) {
	if dir := os.Getenv("BRIG_STATE_DIR"); dir != "" {
		return filepath.Join(dir, "host"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep host sandboxes in: %w", err)
	}
	return filepath.Join(home, ".brig", "host"), nil
}

// HostDir is where host sandboxes live: ~/.brig/host, or host under
// BRIG_STATE_DIR. The credential broker keeps its files in HostBrokerDir below
// it, because a host sandbox can neither read nor write this directory.
func HostDir() (string, error) { return hostDir() }

// CheckHostDirLinks refuses a host directory that is a link, or whose state
// directory above it is one. The state directory is protected by its real
// path, and a link could be swapped for another, so the profile would protect
// the old target. The broker keeps its keys below it for the same reason.
func CheckHostDirLinks(dir string) error {
	if info, err := os.Lstat(filepath.Dir(dir)); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a link, and the host kind keeps its "+
			"profiles there. Point BRIG_STATE_DIR at the real directory", filepath.Dir(dir))
	}
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a link, and the host kind keeps its "+
			"profiles there. Remove the link, and brig creates a directory there", dir)
	}
	return nil
}

// HostBrokerDir is the name of the broker's directory under HostDir. It is
// never a sandbox, so no sandbox may take it.
const HostBrokerDir = "broker"

// hostName refuses a sandbox name that is not one path component of the
// shape brig gives its sandboxes, and the broker's directory.
func hostName(name string) error {
	if name == HostBrokerDir {
		return fmt.Errorf("%q is reserved for the credential broker", name)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.')
	}) {
		return fmt.Errorf("%q is not a host sandbox name", name)
	}
	return nil
}

// marshalRecord and parseRecord keep spec.json in one shape.
func marshalRecord(r hostRecord) ([]byte, error) {
	blob, err := json.MarshalIndent(r, "", "  ")
	return append(blob, '\n'), err
}

func parseRecord(blob []byte) (hostRecord, error) {
	var r hostRecord
	if err := json.Unmarshal(blob, &r); err != nil {
		return hostRecord{}, fmt.Errorf("spec.json is not readable: %w", err)
	}
	return r, nil
}

// pidLine is a pid file's content: the pid and the start time the system
// reports for it. A pid alone can be reused by an unrelated process, which
// Stop must never signal.
func pidLine(pid int, start string) string { return strconv.Itoa(pid) + " " + start + "\n" }
