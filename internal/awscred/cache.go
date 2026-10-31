package awscred

import (
	"context"
	"sync"
	"time"
)

const (
	// getMargin is what Get requires of a cached key. The guest's SDK
	// refreshes on its own schedule, so a key handed out must outlast one
	// refresh interval with room to spare.
	getMargin = 20 * time.Minute
	// ensureMargin is what a registration requires. It is lower than
	// getMargin so that a registration right after a fetch does not fetch
	// again, while a key that is about to need replacing still does.
	ensureMargin = 15 * time.Minute
)

type key struct{ sandbox, route string }

// entry holds the keys of one sandbox and route, and the fetch running for
// them, if any: that fetch is the single flight.
type entry struct {
	mu     sync.Mutex
	flight *flight
	creds  Credentials
	have   bool
	// ptr is what the held keys were fetched for. Re-importing the secret
	// with another mode or role changes the pointer while a sandbox runs, and
	// keys of the old identity must not be served for the new one.
	ptr Pointer
}

// flight is one fetch. done closes once creds and err are set.
type flight struct {
	ptr   Pointer
	done  chan struct{}
	creds Credentials
	err   error
}

// Cache holds fetched credentials per sandbox and route, in memory only.
type Cache struct {
	cli CLI
	now func() time.Time

	mu      sync.Mutex
	entries map[key]*entry
}

// NewCache returns an empty cache. now is the clock, so tests need no sleeps.
func NewCache(cli CLI, now func() time.Time) *Cache {
	return &Cache{cli: cli, now: now, entries: map[key]*entry{}}
}

// Get returns credentials with at least 20 minutes left, fetching when the
// cached ones have less. A failed fetch is an error even when older keys are
// cached: the keys still held are ones about to expire, and handing them out
// would fail in the guest later and further from the cause. A caller whose
// context ends gets its error back; the fetch goes on and fills the cache.
func (c *Cache) Get(ctx context.Context, sandboxID, route string, p Pointer) (Credentials, error) {
	return c.get(ctx, sandboxID, route, p, getMargin)
}

// Ensure fetches only when under 15 minutes remain.
func (c *Cache) Ensure(ctx context.Context, sandboxID, route string, p Pointer) error {
	_, err := c.get(ctx, sandboxID, route, p, ensureMargin)
	return err
}

// Forget drops everything held for a sandbox once it is gone.
func (c *Cache) Forget(sandboxID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.sandbox == sandboxID {
			delete(c.entries, k)
		}
	}
}

func (c *Cache) get(ctx context.Context, sandboxID, route string, p Pointer, margin time.Duration) (Credentials, error) {
	k := key{sandboxID, route}
	c.mu.Lock()
	e := c.entries[k]
	if e == nil {
		e = &entry{}
		c.entries[k] = e
	}
	c.mu.Unlock()

	for {
		e.mu.Lock()
		if e.flight == nil && e.have && e.ptr == p && e.creds.Expiration.Sub(c.now()) >= margin {
			creds := e.creds
			e.mu.Unlock()
			return creds, nil
		}
		// Callers that arrive during a fetch wait for it, so a burst of
		// requests from one sandbox is one STS call.
		f := e.flight
		if f == nil {
			f = &flight{ptr: p, done: make(chan struct{})}
			e.flight = f
			go c.fetch(ctx, e, f, sandboxID)
		}
		e.mu.Unlock()

		select {
		case <-f.done:
		case <-ctx.Done():
			// The fetch goes on without this caller and fills the cache.
			return Credentials{}, ctx.Err()
		}
		if f.ptr == p {
			return f.creds, f.err
		}
		// The fetch was for another pointer: look again.
	}
}

// fetch runs one flight. It does not end with the request that started it:
// the guest's SDK gives up on the endpoint after one or two seconds, an
// assume-role through SSO can take longer, and a fetch killed by every
// hang-up would restart on every retry and never hand out keys. fetchTimeout
// still bounds it.
func (c *Cache) fetch(ctx context.Context, e *entry, f *flight, sandboxID string) {
	creds, err := c.cli.Fetch(context.WithoutCancel(ctx), f.ptr, SessionName(sandboxID))
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.creds, e.have, e.ptr = Credentials{}, false, Pointer{}
	} else {
		e.creds, e.have, e.ptr = creds, true, f.ptr
	}
	f.creds, f.err = creds, err
	e.flight = nil
	close(f.done)
}
