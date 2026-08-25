//go:build darwin || linux

package wrap

import (
	"io/fs"
	"os"
	"path/filepath"
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

// TestWritableByFollowsOwnershipAndTheKernel pins the rule that decides how
// far up the path an attacker can reach. A directory is ours to write if we
// own it, whatever its bits, or if the kernel says so; a read-only filesystem
// says no; an answer brig cannot get counts as yes. As root, only what nobody
// but root could have written is trusted.
func TestWritableByFollowsOwnershipAndTheKernel(t *testing.T) {
	const us, other, root = 1000, 1001, 0
	cases := []struct {
		name   string
		info   statInfo
		uid    int
		access error
		want   bool
	}{
		{"owned by us", statInfo{0o755, us, us}, us, nil, true},
		{"owned by us, write bit off", statInfo{0o555, us, us}, us, syscall.EACCES, true},
		{"other user, kernel says no", statInfo{0o755, other, other}, us, syscall.EACCES, false},
		{"root-owned, kernel says no", statInfo{0o711, root, root}, us, syscall.EACCES, false},
		{"other user, kernel says yes", statInfo{0o755, other, other}, us, nil, true},
		{"other user, read-only filesystem", statInfo{0o777, other, other}, us, syscall.EROFS, false},
		{"other user, no answer", statInfo{0o755, other, other}, us, syscall.ENOENT, true},
		{"root, root-owned 755", statInfo{0o755, root, root}, root, nil, false},
		{"root, root-owned search only", statInfo{0o711, root, root}, root, nil, false},
		{"root, root-owned sticky tmp", statInfo{0o1777, root, root}, root, nil, true},
		{"root, root-owned group-writable", statInfo{0o775, root, root}, root, nil, true},
		{"root, user-owned", statInfo{0o755, us, us}, root, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writableBy(tc.info, tc.uid, tc.access); got != tc.want {
				t.Fatalf("writableBy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDirWritableByUsAsksTheKernel drives the real thing on a directory we
// own with its write bit off: the kernel says no, ownership says yes, and
// ownership wins because the owner can chmod it back.
func TestDirWritableByUsAsksTheKernel(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns nothing it cannot write")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Access(dir, wOK); err == nil {
		t.Fatalf("the kernel let us write a 0555 directory; is the filesystem ignoring modes?")
	}
	if !dirWritableByUs(dir, info) {
		t.Fatal("a directory we own was called not ours to write")
	}
}
