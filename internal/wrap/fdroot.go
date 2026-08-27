package wrap

import (
	"fmt"
	"os"
	"runtime"
)

// rootFromFile builds a root from a descriptor that is already open, without
// looking the name up again.
//
// The descent opens each component with O_DIRECTORY so a fifo cannot block it,
// and then needs a root to carry on from. os.Root has no constructor that takes
// a descriptor, so the obvious way is to open the name a second time -- which
// puts a second lookup in the middle of a walk whose whole purpose is to stop
// a name being resolved more than once. The kernel names an open descriptor as
// a path, so the second lookup can be pointed at the descriptor rather than at
// the name, and there is nothing left for a swap to change.
//
// The path is per-platform: /proc/self/fd on Linux, /dev/fd elsewhere. Both
// resolve to the object the descriptor holds, so a rename of the original name
// does not move it.
func rootFromFile(f *os.File) (*os.Root, error) {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	r, err := os.OpenRoot(fmt.Sprintf("%s/%d", dir, f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("cannot open the directory brig just opened (%s/%d): %w",
			dir, f.Fd(), err)
	}
	return r, nil
}
