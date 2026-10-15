//go:build !darwin

package runtime

import (
	"fmt"
	goruntime "runtime"
)

// newHost refuses the host kind here. The process sandbox is Seatbelt, which
// only macOS has.
func newHost(string) (Runtime, error) {
	return nil, fmt.Errorf("%w: the host kind is unsupported on this platform (%s): its "+
		"process sandbox is macOS's sandbox-exec. Unset BRIG_RUNTIME, or set isolation: "+
		"microvm in the profile", ErrBadRuntime, goruntime.GOOS)
}
