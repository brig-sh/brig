package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fullStore opens a store holding MaxMemories memories of MaxBody bytes
// each. The files are written directly: Put would rebuild the index after
// each one.
func fullStore(t *testing.T) *Files {
	t.Helper()
	f, _, dir := openTest(t)
	body := strings.Repeat("x", MaxBody-len("needle")) + "needle"
	for i := 0; i < MaxMemories; i++ {
		m := Memory{Name: fmt.Sprintf("m%03d", i), Description: strings.Repeat("d", MaxDescription), Body: body}
		if err := os.WriteFile(filepath.Join(dir, m.Name+suffix), encode(m), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

const heapBound = 8 << 20

// Index reads the frontmatter of each memory and never its body, so the
// index of a full store costs a fraction of the bodies it holds.
func TestIndexReadsNoBodies(t *testing.T) {
	f := fullStore(t)
	ctx := context.Background()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	idx, err := f.Index(ctx)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != MaxMemories || idx[0].Description != strings.Repeat("d", MaxDescription) {
		t.Fatalf("Index has %d entries, first %+v", len(idx), idx[0])
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > heapBound {
		t.Errorf("Index allocated %d MiB for %d MiB of bodies, want under %d MiB",
			got>>20, MaxMemories*MaxBody>>20, heapBound>>20)
	}
}

// Search reads one memory at a time and drops it, so a search of a full
// store never holds every body at once.
func TestSearchHoldsOneBodyAtATime(t *testing.T) {
	f := fullStore(t)
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	hits, err := f.Search(context.Background(), "NEEDLE")
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != MaxMemories {
		t.Fatalf("Search found %d, want %d", len(hits), MaxMemories)
	}
	if grew := peak.Load() - min(peak.Load(), base.HeapAlloc); grew > 2*heapBound {
		t.Errorf("Search grew the heap by %d MiB for %d MiB of bodies, want under %d MiB",
			grew>>20, MaxMemories*MaxBody>>20, 2*heapBound>>20)
	}
}

// A store call stops between files once its context is done.
func TestIndexAndSearchHonourContext(t *testing.T) {
	f := fullStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Index(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Index with a done context = %v, want context.Canceled", err)
	}
	if _, err := f.Search(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Search with a done context = %v, want context.Canceled", err)
	}
}
