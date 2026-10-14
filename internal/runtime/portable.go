package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// The machine a teleportable session boots.
//
// hvi boots one of two machines. The native one is the host's own: on a Mac
// the in-kernel GICv3 and the host's CPU feature registers, on a Linux arm64
// host KVM's view of that host. A guest that booted there has cached the
// counter rate and the CPU features it saw, so its checkpoint restores only on
// the same kind of host. portable-arm64-v1 is one machine on both: a GICv2,
// a Cortex-A76 identity with a reduced feature set, and an hvi-time device
// for clocks and timers. A checkpoint of it restores on a Mac and on a Linux
// arm64 host alike, which is what a session that moves needs.
//
// The portable machine needs a guest kernel built for it, with the hvi-time
// clock driver in it. The boot bundle's kernel has none, so the portable
// kernel is a file of its own, beside it. Only the kernel differs: the initrd,
// and the guest agent in it, are the bundle's.
const (
	// MachinePortableArm64V1 is hvi's name for the portable arm64 machine,
	// as `hvi boot --profile` and `hull run --profile` take it.
	MachinePortableArm64V1 = "portable-arm64-v1"
	// MachineNative is what hvi boots when no profile is named.
	MachineNative = "native"

	// portableKernelPath is where the portable kernel sits under the boot
	// asset directory, which is where a run looks when BRIG_PORTABLE_KERNEL
	// is unset. It is hull's place for it: a directory named after the
	// machine profile, beside the bundle, which `hull assets pull --profile
	// portable-arm64-v1` fills.
	portableKernelPath = MachinePortableArm64V1 + "/Image"

	// PortableKernelDigest is the sha256 of the portable kernel brig boots:
	// Linux 6.12.95 with the four patches in hvi's tools/guest/kernel: the
	// EL0 counter, the hvi-time driver, one count per page generation, and
	// host-sleep publishes counted apart from restores. That README names
	// this Image. Without the third patch a guest with more than one vCPU
	// counts one move two or more times. It is pinned here
	// because nothing publishes that kernel yet. A sha256 file beside the
	// kernel would vouch for nothing, since whatever can rewrite the kernel
	// there can rewrite the file too. A later release fetches the kernel by
	// an OCI reference whose signature verifies, and that replaces the pin.
	PortableKernelDigest = "sha256:e85e4338feeb361c5329b4105fa3bda5cd72a235c9ae16c3c4ff94728b12aa1f"
)

// guestArch is the architecture of the guest a sandbox here boots, which is
// the host's. A variable so a test can ask what an arm64 host and an amd64
// host would each decide.
var guestArch = goruntime.GOARCH

// GuestArch is the architecture of the guest a sandbox here boots, in Go's
// spelling.
func GuestArch() string { return guestArch }

// TeleportableMachine is the machine a teleportable session boots on this
// host: portable-arm64-v1 on arm64, and "" on any other architecture, where
// there is no portable machine and hvi boots its native one. "" is also what
// a RunSpec carries for every session that is not teleportable.
func TeleportableMachine() string {
	if guestArch == "arm64" {
		return MachinePortableArm64V1
	}
	return ""
}

// PortableKernel is where the portable kernel is on this host, whether that
// file is there, and whether someone named the place rather than brig
// choosing it.
type PortableKernel struct {
	Path    string
	Present bool
	// Named is true when BRIG_PORTABLE_KERNEL or BRIG_BOOT_ASSETS chose the
	// place: somebody's build, weighed as BootAssets.Named is.
	Named bool
}

// portableKernel finds the portable kernel. BRIG_PORTABLE_KERNEL names the
// file and wins. Otherwise it is portableKernelPath under the boot asset
// directory, found the way the boot bundle is, with the same question put to
// the runtime. brig fetches nothing there: a missing file is a refusal that
// names the variable and hull's verb that installs it.
func portableKernel(locate assetLocator) (PortableKernel, error) {
	if named := os.Getenv("BRIG_PORTABLE_KERNEL"); named != "" {
		// The runtime opens it from another directory than brig's, as it
		// does the bundle's files.
		path, err := filepath.Abs(named)
		if err != nil {
			return PortableKernel{}, err
		}
		return PortableKernel{Path: path, Present: usableFile(path), Named: true}, nil
	}
	dir, explicit, err := bootAssetsDir(locate)
	if err != nil {
		return PortableKernel{}, err
	}
	path := filepath.Join(dir, filepath.FromSlash(portableKernelPath))
	return PortableKernel{Path: path, Present: usableFile(path), Named: explicit != ""}, nil
}

// usableFile is bootArtifactsPresent's standard for one file: a zero-length
// file counts as missing.
func usableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// PortableKernelOf reports the portable kernel a run with rt would boot, for
// brig doctor. It looks and does not fetch, as BootAssetsDir does.
func PortableKernelOf(rt Runtime) (PortableKernel, error) {
	return portableKernel(assetLocatorFor(rt))
}

// forMachine swaps the kernel of a resolved pair for the one machine boots.
// The native machine keeps the bundle's. The portable one takes the portable
// kernel, which must be there: a teleportable session that booted the
// bundle's kernel would boot, and then never move.
func forMachine(assets BootAssets, machine string, locate assetLocator) (BootAssets, error) {
	switch machine {
	case "":
		return assets, nil
	case MachinePortableArm64V1:
	default:
		return BootAssets{}, fmt.Errorf("brig knows no machine profile %q", machine)
	}
	pk, err := portableKernel(locate)
	if err != nil {
		return BootAssets{}, fmt.Errorf("cannot locate the portable kernel: %w", err)
	}
	if !pk.Present {
		return BootAssets{}, &PortableKernelMissingError{Path: pk.Path, Named: pk.Named}
	}
	assets.Kernel = pk.Path
	assets.Machine = machine
	assets.KernelNamed = pk.Named
	return assets, nil
}

// PortableKernelMissingError is a teleportable boot with no portable kernel
// to boot.
type PortableKernelMissingError struct {
	Path  string
	Named bool
}

func (e *PortableKernelMissingError) Error() string {
	if e.Named {
		return fmt.Sprintf("a teleportable session boots the %s machine, and its kernel %s is missing "+
			"or empty. Point BRIG_PORTABLE_KERNEL at the portable kernel", MachinePortableArm64V1, e.Path)
	}
	return fmt.Sprintf("a teleportable session boots the %s machine, which needs the portable kernel "+
		"at %s, and it is not there. Set BRIG_PORTABLE_KERNEL to the portable kernel's path, or install it "+
		"there with `%s`", MachinePortableArm64V1, e.Path, PortableKernelPullLine(MachinePortableArm64V1))
}

// PortableKernelPullLine is the hull line that installs a local portable
// kernel for machine in the store brig drives, and records it there: hull
// boots the profile only with that record. Nothing publishes the kernel, so
// hull needs the file named in HULL_PORTABLE_KERNEL.
func PortableKernelPullLine(machine string) string {
	return "HULL_PORTABLE_KERNEL=PATH " + HullLine("assets", "pull", "--profile", machine)
}

// MachineInspector reads which machine a sandbox runs, from the runtime's
// record of it, for brig info.
type MachineInspector interface {
	// Machine is MachineNative, a profile name such as
	// MachinePortableArm64V1, or "" with a nil error when the record does
	// not say.
	Machine(name string) (string, error)
}

var _ MachineInspector = (*hull)(nil)

// Machine reads which machine the sandbox runs. hull records the profile it
// booted or restored the instance under, and a move carries it. A record
// from a hull that keeps none leaves what hvi says: hvi writes its machine
// profile to the --profile-out file hull records, and that file names the
// profile. Neither leaves the argv: a boot with no --profile is the native
// machine. A restore's argv says nothing about the machine, since the
// checkpoint carries it, so that answers "".
func (h *hull) Machine(name string) (string, error) {
	out, stderr, err := inspectNetwork(h.bin, hullArgs("inspect", name)...)
	if err != nil {
		if strings.Contains(stderr, "instance not found") {
			return "", nil
		}
		return "", fmt.Errorf("inspect machine of %s: %w: %s", name, err, firstLines(stderr, 3))
	}
	return machineOf(out, os.ReadFile)
}

// machineOf is Machine's reading of one `hull inspect` record, apart from
// the call so a test can hand it records hull would print.
func machineOf(record []byte, read func(string) ([]byte, error)) (string, error) {
	var state struct {
		CmdLine     []string `json:"cmdLine"`
		Profile     string   `json:"profile"`
		ProfilePath string   `json:"profilePath"`
	}
	if err := json.Unmarshal(record, &state); err != nil {
		return "", fmt.Errorf("inspect machine: invalid hull response: %w", err)
	}
	if state.Profile != "" {
		return state.Profile, nil
	}
	if state.ProfilePath != "" {
		if data, err := read(state.ProfilePath); err == nil {
			var p struct {
				Profile string `json:"profile"`
			}
			if json.Unmarshal(data, &p) == nil && p.Profile != "" {
				return p.Profile, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect machine: %w", err)
		}
	}
	if len(state.CmdLine) < 2 || filepath.Base(state.CmdLine[0]) != "hvi" || state.CmdLine[1] != "boot" {
		return "", nil
	}
	if profile, ok := argvValue(state.CmdLine[2:], "--profile"); ok && profile != "" {
		return profile, nil
	}
	return MachineNative, nil
}
