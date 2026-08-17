//go:build darwin || linux

package wrap

import (
	"io/fs"
	"syscall"
)

// ownedByRoot reports whether a path belongs to root, which is how
// checkWorkspacePath tells a link the operating system ships -- /tmp and /var
// on macOS -- from one a sandbox planted. A guest writes as the user brig runs
// as; it cannot create a root-owned link in a directory it can reach.
func ownedByRoot(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
