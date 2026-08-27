//go:build darwin || linux

package wrap

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// wOK is W_OK from <unistd.h>, which the syscall package does not export on
// every platform this file builds for.
const wOK = 0x2

// writableBy reports whether a directory is the given user's to write: to
// rename an entry out of and plant something in its place.
//
// This is the question that decides how far up a workspace path an attacker
// can reach. The guest writes as the user brig runs as, so a directory that
// user can write is one where every entry is suspect, and a directory that
// user cannot write is one the guest cannot touch at all.
//
// The owner can always write it, whatever the mode bits say, because the owner
// can put the bits back. For anyone else the kernel is asked, and access is
// the result of access(2) on the directory. The kernel weighs the full group
// list and any ACL, neither of which brig can read for itself: os.Getgroups
// stops at NGROUPS_MAX on macOS while Open Directory membership does not, and
// an ACL grants write without touching the bits. The answer is a policy about
// the directory, not a check before a write, so the usual warning about
// access(2) does not apply. A read-only filesystem counts as not writable,
// since nothing can be swapped there either; any other failure counts as
// writable, which is the safe direction.
//
// Root is a different question. Root can write everything, and as root the
// guest's host-side writes are root's too, so ownership tells brig nothing
// about who put a link where. Trust then covers only what nobody but root
// could have written: root-owned, with no write bit for group or other. That
// keeps the links the machine is made of usable -- /tmp and /var on macOS,
// /home on an ostree system -- and protects nothing: a sandbox run as root
// can reach a nested workspace's parents like anything else, and nothing here
// is the control for that.
func writableBy(info fs.FileInfo, uid int, access error) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	if uid == 0 {
		return st.Uid != 0 || info.Mode().Perm()&0o022 != 0
	}
	if int(st.Uid) == uid {
		return true
	}
	switch {
	case access == nil:
		return true
	case errors.Is(access, syscall.EACCES), errors.Is(access, syscall.EROFS):
		return false
	}
	return true
}

// dirWritableByUs is writableBy for the user brig is running as. A variable so
// a test can declare a directory it owns to be one it cannot write, which is
// the only way to exercise the trusted-prefix walk without running as root.
var dirWritableByUs = func(path string, info os.FileInfo) bool {
	return writableBy(info, os.Getuid(), syscall.Access(path, wOK))
}
