//go:build !darwin && !linux

package wrap

import "io/fs"

// ownedByRoot has no answer on a platform without POSIX ownership, so it says
// no: every symlink on the path is then checked, which is the strict half of
// the decision. brig ships for darwin and linux; this keeps the build honest
// elsewhere.
func ownedByRoot(fs.FileInfo) bool { return false }
