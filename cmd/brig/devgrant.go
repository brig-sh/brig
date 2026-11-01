package main

import (
	"encoding/binary"
	"io/fs"
	"os"
	"syscall"
)

// A rootless run starts the monitor inside a user namespace that drops this
// user's supplementary groups. A device that this user opens only through the
// kvm group opens at the doctor prompt and fails inside the run. The bundle's
// installer asks the same question for the same reason: what reaches the
// monitor is the owner, a named-user ACL entry, or the bits for everyone.

// posixACLOf reads a file's access ACL as the kernel stores it, or nil. A seam.
var posixACLOf = posixACL

// rootlessDeviceDenied is a seam so that a test on a Linux runner does not
// read the runner's own /dev/kvm.
var rootlessDeviceDenied = rootlessDenied

// rootlessGrantFix is the fix on a device the run cannot open.
const rootlessGrantFix = "this user needs the device: run install.sh, whose rootless setup adds a udev rule with sudo, or ask root to grant it"

// rootlessDenied says why a device this user can open stays closed to a
// rootless run, or "" when it does not.
func rootlessDenied(dev string) string {
	info, err := os.Stat(dev)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || rootlessGrant(info.Mode(), st.Uid, uint32(geteuid()), posixACLOf(dev)) {
		return ""
	}
	return dev + " opens for this user only through a group, and a rootless run drops supplementary groups"
}

// rootlessGrant applies the kernel's ACL order to read and write, with every
// group left out: the owner's bits for the owner, then a named entry for this
// uid, limited by the mask, then the bits for everyone. The first that matches
// decides, so a named entry without write does not fall through to other.
func rootlessGrant(mode fs.FileMode, owner, uid uint32, acl []byte) bool {
	const rw = 0o6
	if owner == uid {
		return uint32(mode.Perm())>>6&rw == rw
	}
	if named, perm := aclUserPerm(acl, uid); named {
		return perm&rw == rw
	}
	return uint32(mode.Perm())&rw == rw
}

// aclUserPerm reads system.posix_acl_access: a version word of 2, then entries
// of a 16-bit tag, 16-bit permissions and a 32-bit id, little-endian. It returns
// whether there is an entry for uid and what it grants after the mask.
func aclUserPerm(acl []byte, uid uint32) (bool, uint32) {
	const (
		aclUser = 0x02
		aclMask = 0x10
	)
	if len(acl) < 4 || binary.LittleEndian.Uint32(acl) != 2 {
		return false, 0
	}
	named, perm, mask := false, uint32(0), uint32(0o7)
	for e := acl[4:]; len(e) >= 8; e = e[8:] {
		tag, p, id := binary.LittleEndian.Uint16(e), uint32(binary.LittleEndian.Uint16(e[2:])), binary.LittleEndian.Uint32(e[4:])
		switch {
		case tag == aclUser && id == uid:
			named, perm = true, p
		case tag == aclMask:
			mask = p
		}
	}
	return named, perm & mask
}
