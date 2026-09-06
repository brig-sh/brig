package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// One allocator for the two things brig hands out on the gateway networks: an
// address on the shared one, and a whole /30 for an isolated sandbox.
//
// They were separate copies of the same ninety lines -- the same JSON map, the
// same write through a temporary file, the same tolerance of a corrupt file,
// the same scan for the lowest free slot -- differing only in the filename, the
// bounds and the wording of the "full" message. Two copies means a fix reaches
// one of them, which is how the missing lock below stayed missing in both.
//
// The map is keyed by sandbox name and persisted, so a sandbox that is stopped
// and started again comes back on what it had. Removing a sandbox frees it.

// netAlloc is a persisted map of sandbox name to slot number, within bounds.
type netAlloc struct {
	// path is the JSON map. lock is path + ".lock".
	path string
	// first and last bound the slots, inclusive.
	first, last int
	// what is being handed out, for the message when there is none left:
	// "the sandbox network 198.18.0.0/24", "the isolated address space ...".
	space string
	// unit is the noun in that message: "addresses", "networks".
	unit string
}

// slot returns the number assigned to this sandbox, assigning one if it has
// none, and persists the result.
//
// The whole read-modify-write is under a lock. Without it two boots starting
// together both read the same map, both pick the same lowest free slot, and
// both write -- the rename makes the last one win, so one sandbox loses its
// record and the two are addressed identically on the same network. That is a
// duplicate address on a virtual network, which fails in a way nobody enjoys
// diagnosing.
func (a netAlloc) slot(name string) (int, error) {
	unlock, err := a.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()

	assigned := a.read()
	if slot, ok := assigned[name]; ok && slot >= a.first && slot <= a.last {
		return slot, nil
	}
	slot, err := a.lowestFree(assigned)
	if err != nil {
		return 0, err
	}
	assigned[name] = slot
	if err := a.write(assigned); err != nil {
		return 0, fmt.Errorf("could not record the sandbox network: %w", err)
	}
	return slot, nil
}

// lookup is slot without the allocation: it answers what this sandbox already
// has, and nothing when it has none.
//
// It exists so that asking a question about a sandbox cannot consume one of
// the slots. A predicate that allocated would spend a network on every run
// that is refused afterwards -- for a bad image, a failed verification -- and
// only `brig rm` gives one back.
func (a netAlloc) lookup(name string) (int, bool) {
	slot, ok := a.read()[name]
	return slot, ok && slot >= a.first && slot <= a.last
}

// release gives a removed sandbox's slot back. Losing this is not fatal -- it
// costs one slot -- so callers ignore the error rather than failing a removal
// over bookkeeping.
func (a netAlloc) release(name string) {
	unlock, err := a.lock()
	if err != nil {
		return
	}
	defer unlock()

	assigned := a.read()
	if _, ok := assigned[name]; !ok {
		return
	}
	delete(assigned, name)
	_ = a.write(assigned)
}

// lock takes an exclusive lock on the store, and returns the release.
//
// A lock file beside the map rather than the map itself, so that the atomic
// rename in write does not swap the file out from under a held descriptor.
func (a netAlloc) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (a netAlloc) read() map[string]int {
	assigned := map[string]int{}
	blob, err := os.ReadFile(a.path)
	if err != nil {
		return assigned
	}
	// A corrupt file is not worth failing a boot over: the assignments are
	// derivable again, and the worst case is a sandbox moving.
	if err := json.Unmarshal(blob, &assigned); err != nil {
		return map[string]int{}
	}
	return assigned
}

func (a netAlloc) write(assigned map[string]int) error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(assigned, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file and renamed, so a crash mid-write
	// cannot leave a half-parsed map behind.
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".alloc-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.path)
}

func (a netAlloc) lowestFree(assigned map[string]int) (int, error) {
	taken := make(map[int]bool, len(assigned))
	for _, slot := range assigned {
		taken[slot] = true
	}
	for slot := a.first; slot <= a.last; slot++ {
		if !taken[slot] {
			return slot, nil
		}
	}
	// Sorted only so the message is stable enough to be worth reading.
	names := make([]string, 0, len(assigned))
	for name := range assigned {
		names = append(names, name)
	}
	sort.Strings(names)
	return 0, fmt.Errorf("%s is full: %d %s are in use (%v). "+
		"Remove some sandboxes with `brig rm`", a.space, len(names), a.unit, names)
}
