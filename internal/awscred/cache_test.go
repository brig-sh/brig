package awscred

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
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

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

var rolePtr = Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "arn:aws:iam::0:role/r"}

// newCache returns a cache whose CLI mints keys that expire an hour after the
// fake clock's now, and records the session name of every fetch.
func newCache(clk *fakeClock) (*Cache, *atomic.Int32, *[]string, *sync.Mutex) {
	var n atomic.Int32
	var sessions []string
	var mu sync.Mutex
	cli := CLI{run: func(_ context.Context, _ string, args, _ []string) ([]byte, []byte, error) {
		n.Add(1)
		mu.Lock()
		for i, a := range args {
			if a == "--role-session-name" {
				sessions = append(sessions, args[i+1])
			}
		}
		mu.Unlock()
		exp := clk.now().Add(time.Hour).Format(time.RFC3339)
		return []byte(fmt.Sprintf(`{"Credentials":{"AccessKeyId":"A","SecretAccessKey":"S","SessionToken":"T","Expiration":%q}}`, exp)), nil, nil
	}}
	return NewCache(cli, clk.now), &n, &sessions, &mu
}

func TestCacheReusesFreshKeys(t *testing.T) {
	clk := &fakeClock{t: t0}
	c, n, _, _ := newCache(clk)
	for range 2 {
		if _, err := c.Get(context.Background(), "a", "aws", rolePtr); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("fetches = %d", n.Load())
	}
}

func TestCacheRefreshesUnder20Minutes(t *testing.T) {
	clk := &fakeClock{t: t0}
	c, n, _, _ := newCache(clk)
	c.Get(context.Background(), "a", "aws", rolePtr)
	clk.advance(40 * time.Minute) // exactly 20 left: still served
	c.Get(context.Background(), "a", "aws", rolePtr)
	if n.Load() != 1 {
		t.Fatalf("fetches at 20 min left = %d", n.Load())
	}
	clk.advance(time.Minute)
	c.Get(context.Background(), "a", "aws", rolePtr)
	if n.Load() != 2 {
		t.Errorf("fetches at 19 min left = %d", n.Load())
	}
}

func TestCacheNeverServesExpired(t *testing.T) {
	clk := &fakeClock{t: t0}
	var fail atomic.Bool
	inner, _, _, _ := newCache(clk)
	run := inner.cli.run
	inner.cli.run = func(ctx context.Context, b string, a, e []string) ([]byte, []byte, error) {
		if fail.Load() {
			return nil, []byte("boom"), fmt.Errorf("exit status 1")
		}
		return run(ctx, b, a, e)
	}
	if _, err := inner.Get(context.Background(), "a", "aws", rolePtr); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	clk.advance(61 * time.Minute)
	got, err := inner.Get(context.Background(), "a", "aws", rolePtr)
	if err == nil || got.SecretAccessKey != "" {
		t.Errorf("served %+v, %v", got, err)
	}
}

func TestCacheEnsureThreshold(t *testing.T) {
	clk := &fakeClock{t: t0}
	c, n, _, _ := newCache(clk)
	if err := c.Ensure(context.Background(), "a", "aws", rolePtr); err != nil {
		t.Fatal(err)
	}
	clk.advance(16 * time.Minute) // 44 left
	c.Ensure(context.Background(), "a", "aws", rolePtr)
	if n.Load() != 1 {
		t.Fatalf("fetches at 44 min left = %d", n.Load())
	}
	clk.advance(30 * time.Minute) // 14 left
	c.Ensure(context.Background(), "a", "aws", rolePtr)
	if n.Load() != 2 {
		t.Errorf("fetches at 14 min left = %d", n.Load())
	}
}

func TestCacheIsPerSandboxAndRoute(t *testing.T) {
	clk := &fakeClock{t: t0}
	c, n, sessions, mu := newCache(clk)
	c.Get(context.Background(), "a", "aws", rolePtr)
	c.Get(context.Background(), "b", "aws", rolePtr)
	c.Get(context.Background(), "a", "aws2", rolePtr)
	mu.Lock()
	defer mu.Unlock()
	if n.Load() != 3 || (*sessions)[0] != "a" || (*sessions)[1] != "b" {
		t.Errorf("fetches %d, sessions %v", n.Load(), *sessions)
	}
}

func TestCacheSingleFlight(t *testing.T) {
	clk := &fakeClock{t: t0}
	var n atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 20)
	exp := clk.now().Add(time.Hour).Format(time.RFC3339)
	c := NewCache(CLI{run: func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		n.Add(1)
		started <- struct{}{}
		<-release
		return []byte(fmt.Sprintf(`{"Credentials":{"AccessKeyId":"A","SecretAccessKey":"S","SessionToken":"T","Expiration":%q}}`, exp)), nil, nil
	}}, clk.now)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "a", "aws", rolePtr); err != nil {
				t.Error(err)
			}
		}()
	}
	// Let the goroutines pile up behind the one fetch; a second fetch would
	// bump n while the first is still blocked.
	<-started
	runtime.Gosched()
	close(release)
	wg.Wait()
	if n.Load() != 1 {
		t.Errorf("fetches = %d", n.Load())
	}
}

func TestCacheForget(t *testing.T) {
	clk := &fakeClock{t: t0}
	c, n, _, _ := newCache(clk)
	c.Get(context.Background(), "a", "aws", rolePtr)
	c.Get(context.Background(), "b", "aws", rolePtr)
	c.Forget("a")
	c.Get(context.Background(), "a", "aws", rolePtr)
	c.Get(context.Background(), "b", "aws", rolePtr)
	if n.Load() != 3 {
		t.Errorf("fetches = %d, want 3", n.Load())
	}
}

// Re-importing a secret with another mode while a sandbox runs changes what
// the pointer asks for. Keys fetched for the old pointer must not outlive it.
func TestCacheRefetchesOnPointerChange(t *testing.T) {
	clk := &fakeClock{t: t0}
	var n int
	cli := CLI{run: func(_ context.Context, _ string, args, _ []string) ([]byte, []byte, error) {
		n++
		id := "SELF"
		if slices.Contains(args, "assume-role") {
			id = "ROLE"
		}
		exp := clk.now().Add(time.Hour).Format(time.RFC3339)
		return []byte(fmt.Sprintf(`{"Credentials":{"AccessKeyId":%q,"SecretAccessKey":"S","SessionToken":"T","Expiration":%q},"AccessKeyId":%q,"SecretAccessKey":"S","SessionToken":"T","Expiration":%q}`, id, exp, id, exp)), nil, nil
	}}
	c := NewCache(cli, clk.now)
	self := Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf}
	first, err := c.Get(context.Background(), "a", "aws", self)
	if err != nil || first.AccessKeyID != "SELF" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := c.Get(context.Background(), "a", "aws", rolePtr)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("fetches = %d, want 2", n)
	}
	if second.AccessKeyID != "ROLE" {
		t.Errorf("served keys of %q after the pointer changed to a role", second.AccessKeyID)
	}
	// Ensure holds to the same rule: the held keys are the role's, so the
	// self pointer must fetch again.
	if err := c.Ensure(context.Background(), "a", "aws", self); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("fetches after Ensure with a changed pointer = %d, want 3", n)
	}
}

// The guest's SDK gives up on the endpoint after a second or two, and an
// assume-role through SSO can take longer. If the guest's hang-up killed the
// fetch, every retry would start it again and the guest would never get keys.
// The fetch must finish and fill the cache for the next request.
func TestCacheFetchOutlivesTheCaller(t *testing.T) {
	clk := &fakeClock{t: t0}
	var n atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	exp := clk.now().Add(time.Hour).Format(time.RFC3339)
	c := NewCache(CLI{run: func(ctx context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
		n.Add(1)
		started <- struct{}{}
		// Like exec.CommandContext: a cancelled context kills aws.
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return []byte(fmt.Sprintf(`{"Credentials":{"AccessKeyId":"A","SecretAccessKey":"S","SessionToken":"T","Expiration":%q}}`, exp)), nil, nil
	}}, clk.now)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "a", "aws", rolePtr)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that hung up got %v, want context.Canceled", err)
	}
	close(release)

	got, err := c.Get(context.Background(), "a", "aws", rolePtr)
	if err != nil || got.AccessKeyID != "A" {
		t.Fatalf("after the hang-up: %+v, %v", got, err)
	}
	if n.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the hang-up restarted the fetch", n.Load())
	}
}
