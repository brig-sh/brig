package runtime

import (
	"fmt"
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

// sharedKernelShims are the shims brig knows boot an ordinary container: the
// two runc shim versions, and the bare binary names docker and containerd also
// accept. A shim outside this list and not urunc is not assumed to be either
// thing -- gVisor's runsc is neither a plain container nor a VM, and that is
// the case the unknown boundary is for. See containerdIsolation.
var sharedKernelShims = map[string]bool{
	"io.containerd.runc.v1": true,
	"io.containerd.runc.v2": true,
	"runc":                  true,
	"crun":                  true,
}

// containerdIsolation reports what driver over shim actually gives the guest.
//
// Pure, and separate from the adapter, because naming the boundary is the part
// worth being sure about and it should be testable without a containerd.
//
// Three cases, and the third is why this is not a boolean. urunc boots the
// container as a microVM, so that is a kernel of its own. runc and crun share
// the host kernel, and BRIG_CONTAINERD_RUNTIME pointing at one is the case this
// row exists for: it is a supported thing to ask for and it used to be
// invisible. Anything else -- a kata shim, a gVisor shim, a fork of urunc under
// another name -- may well be a VM, and brig has no way to establish that from
// a shim name, so it says so instead of picking the answer the reader would
// prefer.
func containerdIsolation(driver, shim string) Isolation {
	over := driver + " over containerd"
	switch {
	case shim == uruncShim || strings.HasPrefix(shim, "io.containerd.urunc."):
		return Isolation{BoundaryVM, fmt.Sprintf("%s, %s", over, shim)}
	case sharedKernelShims[shim]:
		return Isolation{BoundaryContainer, fmt.Sprintf(
			"%s, %s: the guest shares the host kernel", over, shim)}
	default:
		return Isolation{BoundaryUnknown, fmt.Sprintf(
			"%s, %s: brig cannot tell whether that shim boots a kernel of its own", over, shim)}
	}
}
