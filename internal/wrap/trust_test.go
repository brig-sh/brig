//go:build darwin || linux

package wrap

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

// statInfo is a FileInfo whose Sys is a Stat_t, which is all writableBy reads.
type statInfo struct {
	mode fs.FileMode
	uid  uint32
	gid  uint32
}

func (s statInfo) Name() string       { return "dir" }
func (s statInfo) Size() int64        { return 0 }
func (s statInfo) Mode() fs.FileMode  { return fs.ModeDir | s.mode }
func (s statInfo) ModTime() time.Time { return time.Time{} }
func (s statInfo) IsDir() bool        { return true }
func (s statInfo) Sys() any           { return &syscall.Stat_t{Uid: s.uid, Gid: s.gid} }

// TestWritableByFollowsPOSIXModeBits pins the rule that decides how far up the
// path an attacker can reach. A directory is writable by us if we own it, or a
// mode bit grants write to a group we are in or to everyone. Nothing else.
func TestWritableByFollowsPOSIXModeBits(t *testing.T) {
	const us, other, root = 1000, 1001, 0
	ourGroups := []int{1000, 20}
	cases := []struct {
		name string
		info statInfo
		uid  int
		want bool
	}{
		{"owned by us", statInfo{0o755, us, us}, us, true},
		{"root-owned, mode 755", statInfo{0o755, root, root}, us, false},
		{"root-owned, search only", statInfo{0o711, root, root}, us, false},
		{"root-owned, world-writable sticky tmp", statInfo{0o1777, root, root}, us, true},
		{"other user, group-writable, our group", statInfo{0o775, other, 20}, us, true},
		{"other user, group-writable, not our group", statInfo{0o775, other, 999}, us, false},
		{"other user, mode 755", statInfo{0o755, other, other}, us, false},
		{"we are root, so everything", statInfo{0o755, other, other}, root, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writableBy(tc.info, tc.uid, ourGroups); got != tc.want {
				t.Fatalf("writableBy = %v, want %v", got, tc.want)
			}
		})
	}
}
