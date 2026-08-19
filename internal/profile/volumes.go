package profile

import (
	"cmp"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The kinds of volume a profile may declare, one primitive per entry.
//
// Flat rather than nested so that `volume` -- a named volume several agents
// attach -- can land without rewriting the shape. It is parsed and refused
// today so adding it needs no migration.
const (
	// VolumeTmpfs is memory the guest writes to and nothing else sees.
	// Covering the agent's config directory with one keeps a credential
	// written there off host disk by construction, with nothing to verify
	// afterwards.
	VolumeTmpfs = "tmpfs"
	// VolumeHostMount is one path bound back out to the host through the
	// workspace, naming an exception to the tmpfs above it -- the state worth
	// keeping across boots.
	VolumeHostMount = "hostmount"
	// VolumeNamed is a shared, named volume. Reserved: parsed, and refused.
	VolumeNamed = "volume"
)

// defaultTmpfsSize is what a tmpfs takes when its volume names no size:.
//
// size= is not decoration, and is why this is tmpfs and not ramfs: ramfs
// silently ignores it, so a guest process could exhaust VM memory through
// brig's own mount.
//
// 64m suits a directory holding configuration. It does not suit every one a
// profile might cover: an agent's home accumulates caches and per-job scratch,
// and exhaustion surfaces inside the guest as ENOSPC from the agent's own
// tools, with nothing on the host watching for it. That is why size: exists
// rather than a larger constant -- the right number is a property of what the
// profile puts under the mount, and only its author knows that.
const defaultTmpfsSize = "64m"

// tmpfsSizePattern is a mount size: digits, optionally k, m or g. Kept strict
// because mount(8) answers an option it cannot parse by failing the mount,
// which surfaces as a boot failure rather than as a profile error.
var tmpfsSizePattern = regexp.MustCompile(`^[1-9][0-9]*[kKmMgG]?$`)

// TmpfsOptions are the mount options this tmpfs takes. nodev and nosuid are
// the ordinary hygiene for a directory the sandbox writes to.
func (v Volume) TmpfsOptions() string {
	size := v.Size
	if size == "" {
		size = defaultTmpfsSize
	}
	return "size=" + size + ",mode=0700,nodev,nosuid"
}

// Volume is one mount brig makes inside the guest, under GuestHome.
//
// The list is flat and the order in the file is taste: brig mounts parents
// before children by path depth. Declaration order would be a trap -- a
// profile listing .claude/sessions above .claude would mount the tmpfs over
// the hostmount it had just made, and silently lose the state it named.
type Volume struct {
	Kind string `json:"kind"`
	// Path is relative to GuestHome, e.g. ".claude/sessions". Not absolute,
	// and it may not escape: the same rules a files: binding carries.
	Path string `json:"path,omitempty"`
	// Source is where the mount comes from, and it is the field that only the
	// reserved kind needs. A hostmount's source is implicit -- the same path
	// in the workspace, which is where that state already lives -- so
	// spelling it is either a no-op or a redirection brig does not implement.
	// For kind: volume it names the volume.
	Source string `json:"source,omitempty"`
	// Size is the tmpfs size, for example "256m". Empty takes
	// defaultTmpfsSize. Only kind: tmpfs has one -- a hostmount's size is the
	// workspace's, which is the host filesystem's.
	Size string `json:"size,omitempty"`
	// File marks a hostmount whose target is a file rather than a directory:
	// history.jsonl needs a touch where sessions needs a mkdir, and a bind
	// onto a target of the wrong kind fails. Decided by what is already in
	// the workspace when something is there, so this is only consulted for a
	// path that does not exist yet -- guessing from an extension would be a
	// heuristic with a silent failure on the other side of it.
	File bool `json:"file,omitempty"`
}

// MountOrder is the volumes sorted the way they must be mounted: parents
// first, by path depth.
//
// Depth rather than a topological sort because a parent's path is strictly
// shorter in components than any descendant's, so depth already answers the
// only question ordering has. Stable, so two entries at the same depth keep
// the order their author wrote -- there is no relationship between them to
// get wrong, and reordering them would make a diff of the mount log noisy for
// no reason.
func MountOrder(list []Volume) []Volume {
	out := slices.Clone(list)
	slices.SortStableFunc(out, func(a, b Volume) int {
		return cmp.Compare(pathDepth(a.Path), pathDepth(b.Path))
	})
	return out
}

func pathDepth(p string) int { return strings.Count(path.Clean(p), "/") }

// Under reports whether child sits below parent. Equality is not nesting: a
// hostmount on the same path as the tmpfs above it replaces the tmpfs rather
// than punching a hole in it, which is not what either entry says.
func Under(child, parent string) bool {
	return child != parent && strings.HasPrefix(path.Clean(child), path.Clean(parent)+"/")
}

// Tmpfs and HostMounts are the two kinds the delivery mechanics walk
// separately: one covers, the others are pinned before the cover and bound
// back through it.
func (p Profile) Tmpfs() []Volume      { return p.volumesOfKind(VolumeTmpfs) }
func (p Profile) HostMounts() []Volume { return p.volumesOfKind(VolumeHostMount) }

func (p Profile) volumesOfKind(kind string) []Volume {
	var out []Volume
	for _, v := range MountOrder(p.Volumes) {
		if v.Kind == kind {
			out = append(out, v)
		}
	}
	return out
}

// EphemeralPath reports whether a path under GuestHome is covered by one of
// the profile's tmpfs volumes and not carved back out by a hostmount under it.
//
// This is the fail-closed predicate a files: binding is checked against, and
// it is the whole safety argument for the design: a credential written at a
// path this returns true for cannot reach host disk, because there is no path
// from the tmpfs to the host share to check.
func (p Profile) EphemeralPath(target string) bool {
	covered := false
	for _, v := range p.Volumes {
		switch v.Kind {
		case VolumeTmpfs:
			if Under(target, v.Path) || target == v.Path {
				covered = true
			}
		}
	}
	if !covered {
		return false
	}
	for _, v := range p.Volumes {
		if v.Kind == VolumeHostMount && (target == v.Path || Under(target, v.Path)) {
			return false
		}
	}
	return true
}

// FileBinding is one file the guest sees, and which stored secret fills it.
//
// A file has one source, so this takes ref: and never refs: -- the chain
// exists to give one environment variable a shell override and a store
// fallback, and a file has no shell to override it.
//
// It is an ordinary file, written into a tmpfs the profile declares under
// volumes:, never a bind mount onto its own path. Agents rewrite a credential
// file atomically (temp file, then rename), and rename onto a mountpoint
// returns EBUSY -- so a bind mount there breaks in-guest refresh, and the temp
// file lands in the workspace, which is host disk. Covering the directory
// fixes both at once.
type FileBinding struct {
	// Ref is `secrets.<name>`. env. is not accepted: a file binding exists to
	// put a stored credential where an agent reads one, and reading the
	// value out of brig's own environment to write it into the guest would be
	// the shell-wrapper habit this feature removes.
	Ref string `json:"ref"`
	// Path is relative to GuestHome, e.g. ".claude/.credentials.json".
	Path string `json:"path"`
	// Mode is the octal permission the file is created with, e.g. "0600".
	// A string rather than a number because YAML reads 0600 as decimal 600,
	// which is 0o1130 -- a mode nobody meant and that nothing would report.
	Mode string `json:"mode,omitempty"`
}

// DefaultFileMode is what a binding gets when it names none: owner-only, which
// is what agents write their own credential files with.
const DefaultFileMode = 0o600

// FileMode parses Mode, defaulting to DefaultFileMode.
func (b FileBinding) FileMode() (fs.FileMode, error) {
	if b.Mode == "" {
		return DefaultFileMode, nil
	}
	n, err := strconv.ParseUint(b.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal, e.g. \"0600\"", b.Mode)
	}
	if n > 0o777 {
		return 0, fmt.Errorf("mode %q sets more than the permission bits", b.Mode)
	}
	return fs.FileMode(n), nil
}

// guestPath checks a path that names something under GuestHome.
//
// Relative, and in its simplest form. Absolute is refused rather than
// interpreted because a profile is a shareable artifact: /etc/x means one
// thing in the guest and another to whoever reads the file. `..` is refused
// because everything downstream of it -- a bind mount run as root, a file
// created at the target -- treats the path as being inside the workspace.
func guestPath(what, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s has no path:", what)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%s path %q is absolute; paths are relative to guestHome", what, p)
	case strings.HasPrefix(p, "~"):
		return fmt.Errorf("%s path %q starts with ~; paths are relative to guestHome "+
			"already, so write it without one", what, p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s path %q escapes guestHome", what, p)
	}
	if clean != p {
		return fmt.Errorf("%s path %q is not in its simplest form; write it as %q", what, p, clean)
	}
	return nil
}
