package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Boundary is what stands between the guest and the host kernel.
//
// Three values rather than two, because "brig cannot tell" is a real answer
// and the only honest one for a shim brig does not recognise. A reader acts on
// this row -- it is the difference between running an agent on an untrusted
// repository and not -- so a guess in the direction of the stronger boundary is
// the one mistake this type must not make.
type Boundary string

const (
	// BoundaryVM is a guest with a kernel of its own, booted by a hypervisor.
	BoundaryVM Boundary = "microVM"
	// BoundaryContainer is a guest sharing the host kernel.
	BoundaryContainer Boundary = "container"
	// BoundaryUnknown is everything brig has not established either way.
	BoundaryUnknown Boundary = "unknown"
)

// Isolation is the boundary a sandbox gets, and what establishes it.
//
// It reports what this invocation resolved -- the runtime binary in hand, the
// hypervisor backend the run settled on, the containerd shim the run will name
// -- and not what a profile or the documentation says a sandbox ought to be.
// brig does not look inside a running guest to confirm it, and the row says
// nothing that depends on doing so.
type Isolation struct {
	Boundary Boundary
	// Detail is what establishes the boundary, in the reader's terms: which
	// runtime, which backend or shim, and for anything short of a microVM what
	// that costs them.
	Detail string
}

// Line is the isolation as the envelope prints it: the boundary, then the
// detail in parentheses, which is the shape every other row of the block uses.
func (i Isolation) Line() string { return fmt.Sprintf("%s (%s)", i.Boundary, i.Detail) }

// uruncShim is the containerd shim that boots the container as a microVM, and
// the default containerdRuntime returns. See docs/runtimes.md for the contract
// between brig and urunc.
const uruncShim = "io.containerd.urunc.v2"

// uruncShimBinary is the binary containerd starts for uruncShim.
const uruncShimBinary = "containerd-shim-urunc-v2"

// sharedKernelShims are the shim binaries brig knows boot an ordinary
// container: the two runc shim versions. sharedKernelRuntimes are the runtime
// binaries brig knows do the same when nerdctl runs one as the runc shim's
// binary. A value that is neither and not urunc is not assumed to be either
// thing -- gVisor's runsc is neither a plain container nor a VM, and that is
// the case the unknown boundary is for. See shimBoundary.
var (
	sharedKernelShims = map[string]bool{
		"containerd-shim-runc-v1": true,
		"containerd-shim-runc-v2": true,
	}
	sharedKernelRuntimes = map[string]bool{
		"runc": true,
		"crun": true,
	}
)

// shimBinary is the shim binary containerd starts for a runtime name, by its
// BinaryName (in containerd's pkg/shim): the last two dot-separated parts. So
// io.containerd.runc.v2, x.runc.v2 and io.containerd.urunc.runc.v2 all start
// containerd-shim-runc-v2, and what comes before those two parts changes
// nothing. It returns "" for a name BinaryName rejects, and for a name with a
// slash in it: containerd refuses a relative one before BinaryName is asked,
// and runs an absolute one as the shim binary itself, so neither is a runtime
// name that starts a shim of that name.
func shimBinary(name string) string {
	parts := strings.Split(name, ".")
	if strings.Contains(name, "/") || len(parts) < 2 || parts[0] == "" {
		return ""
	}
	return "containerd-shim-" + parts[len(parts)-2] + "-" + parts[len(parts)-1]
}

// shimBoundary places a BRIG_CONTAINERD_RUNTIME value by what nerdctl and
// containerd start for it, which is not always what its name suggests.
//
// nerdctl hands a value that starts io.containerd. to containerd as a runtime
// name, and containerd starts the shim binary that name resolves to. nerdctl
// looks any other value up with exec.LookPath first, and runs what it finds as
// the runc shim's binary. A path or a bare name is that binary, so it is placed
// by its file name: /usr/bin/crun boots the same plain container that crun
// does. A dotted name is normally not found, so it reaches containerd as a
// runtime name, and one that resolves to the runc shim is a plain container.
// It is not credited with urunc, though: a binary of that name on PATH would
// run instead, and brig cannot see nerdctl's PATH.
func shimBoundary(shim string) Boundary {
	switch {
	case strings.HasPrefix(shim, "io.containerd."):
		switch bin := shimBinary(shim); {
		case bin == uruncShimBinary:
			return BoundaryVM
		case sharedKernelShims[bin]:
			return BoundaryContainer
		}
	case strings.Contains(shim, "/") || !strings.Contains(shim, "."):
		if sharedKernelRuntimes[filepath.Base(shim)] {
			return BoundaryContainer
		}
	default:
		if sharedKernelShims[shimBinary(shim)] {
			return BoundaryContainer
		}
	}
	return BoundaryUnknown
}

// sharesHostKernel reports whether shim is one brig knows gives the guest the
// host's own kernel. See shimBoundary.
func sharesHostKernel(shim string) bool { return shimBoundary(shim) == BoundaryContainer }

// containerdIsolation reports what driver over shim actually gives the guest.
//
// Pure, and separate from the adapter, because naming the boundary is the part
// worth being sure about and it should be testable without a containerd.
//
// Three cases, and the third is why this is not a boolean. urunc boots the
// container as a microVM, so that is a kernel of its own. runc and crun share
// the host kernel; brig will not boot one (see refuseSharedKernel), and this
// row still names it on `brig info`, which resolves the shim without booting.
// Anything else -- a kata shim, a gVisor shim, a fork of urunc under another
// name -- may well be a VM, and brig has no way to establish that from a shim
// name, so it says so instead of picking the answer the reader would prefer.
func containerdIsolation(driver, shim string) Isolation {
	over := driver + " over containerd"
	switch shimBoundary(shim) {
	case BoundaryVM:
		return Isolation{BoundaryVM, fmt.Sprintf("%s, %s", over, shim)}
	case BoundaryContainer:
		return Isolation{BoundaryContainer, fmt.Sprintf(
			"%s, %s: the guest shares the host kernel", over, shim)}
	default:
		return Isolation{BoundaryUnknown, fmt.Sprintf(
			"%s, %s: brig cannot tell whether that shim boots a kernel of its own", over, shim)}
	}
}
