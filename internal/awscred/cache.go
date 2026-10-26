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

// entry serialises fetches for one key: its mutex is the single flight.
type entry struct {
	mu    sync.Mutex
	creds Credentials
	have  bool
	// ptr is what the held keys were fetched for. Re-importing the secret
	// with another mode or role changes the pointer while a sandbox runs, and
	// keys of the old identity must not be served for the new one.
	ptr Pointer
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
// would fail in the guest later and further from the cause.
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

	// Callers that arrive during a fetch wait here, then find its result
	// fresh, so a burst of requests from one sandbox is one STS call.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.have && e.ptr == p && e.creds.Expiration.Sub(c.now()) >= margin {
		return e.creds, nil
	}
	creds, err := c.cli.Fetch(ctx, p, SessionName(sandboxID))
	if err != nil {
		e.creds, e.have, e.ptr = Credentials{}, false, Pointer{}
		return Credentials{}, err
	}
	e.creds, e.have, e.ptr = creds, true, p
	return creds, nil
}
