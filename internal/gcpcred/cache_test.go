package gcpcred

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// fakeMint mints a token named after the pointer's mode that expires life
// after the fake clock's now, and counts its calls. err, when set, fails the
// mint.
type fakeMint struct {
	clk  *fakeClock
	n    atomic.Int32
	life atomic.Int64
	err  atomic.Pointer[error]
}

func newFakeMint(clk *fakeClock, life time.Duration) *fakeMint {
	m := &fakeMint{clk: clk}
	m.life.Store(int64(life))
	return m
}

func (m *fakeMint) mint(_ context.Context, p Pointer) (Token, error) {
	m.n.Add(1)
	if e := m.err.Load(); e != nil {
		return Token{}, *e
	}
	return Token{Value: "tok-" + string(p.Mode), Expiry: m.clk.now().Add(time.Duration(m.life.Load()))}, nil
}

func mustGet(t *testing.T, c *Cache, sandbox, route string, p Pointer) Token {
	t.Helper()
	tok, err := c.Get(context.Background(), sandbox, route, p)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestGetMintsOnceForABurst(t *testing.T) {
	clk := &fakeClock{t: t0}
	var n atomic.Int32
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	c := NewCache(func(context.Context, Pointer) (Token, error) {
		n.Add(1)
		started <- struct{}{}
		<-release
		return Token{Value: "t", Expiry: clk.now().Add(time.Hour)}, nil
	}, clk.now)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "sb", "gcp", impersonate); err != nil {
				t.Error(err)
			}
		}()
	}
	// Let the goroutines pile up behind the one mint; a second mint would
	// bump n while the first is still blocked.
	<-started
	runtime.Gosched()
	close(release)
	wg.Wait()
	if n.Load() != 1 {
		t.Errorf("mints = %d, want 1", n.Load())
	}
}

func TestGetMintsAgainUnderTheMargin(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, time.Hour)
	c := NewCache(m.mint, clk.now)
	mustGet(t, c, "sb", "gcp", impersonate)
	clk.advance(57 * time.Minute) // 3 min left: over the margin
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 1 {
		t.Fatalf("mints at 3 min left = %d, want 1", m.n.Load())
	}
	clk.advance(90 * time.Second) // 1m30s left, long past refetchFloor
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 2 {
		t.Errorf("mints at 1m30s left = %d, want 2", m.n.Load())
	}
}

// gcloud hands back its own cached token until near its expiry, so a mint can
// return a token with less than the margin left. It is served while valid for
// refetchFloor, rather than minted again on every request.
func TestAMintCloseToExpiryIsServedForTheFloor(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, 90*time.Second)
	c := NewCache(m.mint, clk.now)
	mustGet(t, c, "sb", "gcp", impersonate)
	mustGet(t, c, "sb", "gcp", impersonate)
	clk.advance(59 * time.Second)
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 1 {
		t.Fatalf("mints within the floor = %d, want 1", m.n.Load())
	}
	clk.advance(2 * time.Second)
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 2 {
		t.Errorf("mints after the floor = %d, want 2", m.n.Load())
	}

	// A token that expires inside the floor is not served once expired.
	m.life.Store(int64(10 * time.Second))
	clk.advance(2 * time.Minute)
	mustGet(t, c, "sb", "gcp", impersonate)
	clk.advance(11 * time.Second)
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 4 {
		t.Errorf("mints after an expired token inside the floor = %d, want 4", m.n.Load())
	}
}

// Re-importing the secret with another identity while a sandbox runs changes
// the pointer. A token of the old identity must not be served for the new one.
func TestAPointerChangeIsNotServedTheOldToken(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, time.Hour)
	c := NewCache(m.mint, clk.now)
	if got := mustGet(t, c, "sb", "gcp", impersonate); got.Value != "tok-impersonate" {
		t.Fatalf("first = %q", got.Value)
	}
	got := mustGet(t, c, "sb", "gcp", self)
	if m.n.Load() != 2 {
		t.Errorf("mints = %d, want 2", m.n.Load())
	}
	if got.Value != "tok-self" {
		t.Errorf("served %q after the pointer changed to self", got.Value)
	}
	// Ensure holds to the same rule.
	if err := c.Ensure(context.Background(), "sb", "gcp", impersonate); err != nil {
		t.Fatal(err)
	}
	if m.n.Load() != 3 {
		t.Errorf("mints after Ensure with a changed pointer = %d, want 3", m.n.Load())
	}
}

// The token still held is one about to expire. Handing it out after a failed
// mint would fail in the guest later and further from the cause.
func TestAFailedMintIsAnErrorEvenWithATokenHeld(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, 90*time.Second)
	c := NewCache(m.mint, clk.now)
	mustGet(t, c, "sb", "gcp", impersonate)
	clk.advance(refetchFloor + time.Second)
	boom := errors.New("gcloud failed")
	m.err.Store(&boom)
	got, err := c.Get(context.Background(), "sb", "gcp", impersonate)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the mint's error", err)
	}
	if got.Value != "" {
		t.Errorf("a failed mint served %q", got.Value)
	}
	if err := c.Ensure(context.Background(), "sb", "gcp", impersonate); !errors.Is(err, boom) {
		t.Errorf("Ensure err = %v, want the mint's error", err)
	}
}

// Two routes of one sandbox, and a second sandbox, can name the same pointer.
// Each is its own entry, so forgetting one sandbox leaves the other's token.
func TestSharedPointerKeysAreSeparate(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, time.Hour)
	c := NewCache(m.mint, clk.now)
	mustGet(t, c, "sb1", "r1", impersonate)
	mustGet(t, c, "sb1", "r2", impersonate)
	mustGet(t, c, "sb2", "r1", impersonate)
	if m.n.Load() != 3 {
		t.Fatalf("mints = %d, want 3", m.n.Load())
	}
	c.Forget("sb1")
	mustGet(t, c, "sb2", "r1", impersonate)
	if m.n.Load() != 3 {
		t.Errorf("mints after Forget(sb1) for sb2 = %d, want 3", m.n.Load())
	}
	mustGet(t, c, "sb1", "r1", impersonate)
	mustGet(t, c, "sb1", "r2", impersonate)
	if m.n.Load() != 5 {
		t.Errorf("mints after Forget(sb1) for sb1 = %d, want 5", m.n.Load())
	}
}

func TestForgetDropsOneSandbox(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, time.Hour)
	c := NewCache(m.mint, clk.now)
	mustGet(t, c, "a", "gcp", impersonate)
	mustGet(t, c, "b", "gcp", impersonate)
	c.Forget("a")
	mustGet(t, c, "a", "gcp", impersonate)
	mustGet(t, c, "b", "gcp", impersonate)
	if m.n.Load() != 3 {
		t.Errorf("mints = %d, want 3", m.n.Load())
	}
}

// Registration proves a mint works, and the request right after it is served
// that token rather than minting again.
func TestEnsureFillsTheCacheForGet(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newFakeMint(clk, time.Hour)
	c := NewCache(m.mint, clk.now)
	if err := c.Ensure(context.Background(), "sb", "gcp", impersonate); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, "sb", "gcp", impersonate)
	if m.n.Load() != 1 {
		t.Errorf("mints = %d, want 1", m.n.Load())
	}
}

// A gcloud mint can take seconds, longer than a guest client waits. If the
// client's hang-up killed the mint, every retry would start it again and the
// guest would never get a token. The mint must finish and fill the cache.
func TestACancelledCallerDoesNotCancelTheMint(t *testing.T) {
	clk := &fakeClock{t: t0}
	var n atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	c := NewCache(func(ctx context.Context, _ Pointer) (Token, error) {
		n.Add(1)
		started <- struct{}{}
		// Like exec.CommandContext: a cancelled context kills gcloud.
		select {
		case <-release:
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
		return Token{Value: "t", Expiry: clk.now().Add(time.Hour)}, nil
	}, clk.now)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "sb", "gcp", impersonate)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that hung up got %v, want context.Canceled", err)
	}
	close(release)

	got := mustGet(t, c, "sb", "gcp", impersonate)
	if got.Value != "t" {
		t.Fatalf("after the hang-up: %q", got.Value)
	}
	if n.Load() != 1 {
		t.Errorf("mints = %d, want 1: the hang-up restarted the mint", n.Load())
	}
}
