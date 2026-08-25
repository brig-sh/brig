//go:build darwin || linux

package wrap

import (
	"io/fs"
	"os"
	"syscall"
)

// writableBy reports whether a directory can be written by the given user,
// from its POSIX mode bits and ownership alone.
//
// This is the question that decides how far up a workspace path an attacker
// can reach. The guest writes as the user brig runs as, so a directory that
// user can write is a directory the guest can rename an entry out of and plant
// something in its place, and every entry inside it is therefore suspect. A
// directory that user cannot write is one the guest cannot touch at all, and
// its entries can be trusted to stay what they are.
//
// Only the mode bits are consulted. An ACL that grants write to a directory
// whose bits say otherwise is not seen, so a workspace under such a directory
// is treated as safer than it is. That is a known limit rather than an
// oversight: reading ACLs portably is a project of its own.
func writableBy(info fs.FileInfo, uid int, groups []int) bool {
	if uid == 0 {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	if int(st.Uid) == uid {
		return true
	}
	perm := info.Mode().Perm()
	if perm&0o002 != 0 {
		return true
	}
	if perm&0o020 != 0 {
		for _, g := range groups {
			if int(st.Gid) == g {
				return true
			}
		}
	}
	return false
}

// dirWritableByUs is writableBy for the user brig is running as. A variable so
// a test can declare a directory it owns to be one it cannot write, which is
// the only way to exercise the trusted-prefix walk without running as root.
// The path is there for the test's benefit; production ignores it.
var dirWritableByUs = func(path string, info os.FileInfo) bool {
	groups, err := os.Getgroups()
	if err != nil {
		groups = nil
	}
	return writableBy(info, os.Getuid(), append(groups, os.Getgid()))
}
