package runtime

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func scratchAlloc(t *testing.T, first, last int) netAlloc {
	t.Helper()
	return netAlloc{
		path:  filepath.Join(t.TempDir(), "alloc.json"),
		first: first,
		last:  last,
		space: "the test space",
		unit:  "slots",
	}
}

// Two boots starting together must not be handed the same slot. Without a lock
// both read the same map, both pick the same lowest free number and both write
// -- the rename makes the last one win, so one sandbox loses its record and the
// two are addressed identically on one virtual network.
func TestSlotIsUniqueUnderConcurrentAllocation(t *testing.T) {
	alloc := scratchAlloc(t, 0, 63)

	const n = 24
	got := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot, err := alloc.slot(string(rune('a' + i)))
			if err != nil {
				t.Errorf("slot: %v", err)
				return
			}
			got[i] = slot
		}()
	}
	wg.Wait()

	seen := map[int]int{}
	for i, slot := range got {
		if prev, dup := seen[slot]; dup {
			t.Fatalf("sandboxes %d and %d were both given slot %d", prev, i, slot)
		}
		seen[slot] = i
	}
	// And every one of them is on record, so none was lost to a racing write.
	if recorded := len(alloc.read()); recorded != n {
		t.Errorf("%d assignments recorded, want %d", recorded, n)
	}
}

// lookup answers what a sandbox has without giving it anything. A predicate
// that allocated would spend one of a bounded number of slots on every run
// that is refused afterwards, and only `brig rm` gives one back.
func TestLookupDoesNotAllocate(t *testing.T) {
	alloc := scratchAlloc(t, 0, 3)

	if _, ok := alloc.lookup("brig-s"); ok {
		t.Error("lookup invented a slot for a sandbox that has none")
	}
	if n := len(alloc.read()); n != 0 {
		t.Fatalf("lookup wrote %d assignments", n)
	}
	// Asking many times must not exhaust a space of four.
	for range 20 {
		_, _ = alloc.lookup("brig-s")
	}
	if n := len(alloc.read()); n != 0 {
		t.Fatalf("repeated lookups consumed %d slots", n)
	}

	// And once allocated, lookup finds exactly that.
	want, err := alloc.slot("brig-s")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := alloc.lookup("brig-s"); !ok || got != want {
		t.Errorf("lookup = %d (ok=%t), want %d", got, ok, want)
	}
}

// A slot outside the recorded bounds is not honoured: a map written when the
// space was larger, or hand-edited, must not address a sandbox outside it.
func TestOutOfRangeSlotsAreIgnored(t *testing.T) {
	alloc := scratchAlloc(t, 0, 3)
	if err := os.WriteFile(alloc.path, []byte(`{"brig-s": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := alloc.lookup("brig-s"); ok {
		t.Error("lookup honoured a slot outside the space")
	}
	slot, err := alloc.slot("brig-s")
	if err != nil {
		t.Fatal(err)
	}
	if slot < alloc.first || slot > alloc.last {
		t.Errorf("slot %d is outside [%d,%d]", slot, alloc.first, alloc.last)
	}
}

// The two allocators are one implementation, so a fix reaches both. They were
// ninety duplicated lines differing only in the filename, the bounds and the
// wording -- which is how the missing lock above stayed missing in both.
func TestTheTwoAllocatorsAreSeparateStores(t *testing.T) {
	scratchIsolatedDir(t)

	shared, err := sharedIPs()
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := isolatedNets()
	if err != nil {
		t.Fatal(err)
	}
	if shared.path == isolated.path {
		t.Fatal("the shared addresses and the isolated networks share a store")
	}
	// Bounds differ, and each is the space it hands out.
	if shared.first != firstGuestHost || shared.last != lastGuestHost {
		t.Errorf("shared bounds are [%d,%d]", shared.first, shared.last)
	}
	if isolated.first != 0 || isolated.last != sandboxNets-1 {
		t.Errorf("isolated bounds are [%d,%d]", isolated.first, isolated.last)
	}
	// An address on one does not consume a network on the other.
	if _, err := shared.slot("brig-s"); err != nil {
		t.Fatal(err)
	}
	if _, ok := isolated.lookup("brig-s"); ok {
		t.Error("taking a shared address allocated an isolated network too")
	}
}
