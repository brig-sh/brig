package gcpcred

import (
	"context"
	"sync"
	"time"
)

const (
	// margin is what Get and Ensure require of a cached token. It is far
	// shorter than awscred's: the broker injects the token into each request
	// and the request uses it at once, so it need outlast only that request.
	// It cannot be longer either. gcloud hands back its own cached token and
	// refreshes it only near expiry, so a larger margin would mint on every
	// request in the last minutes of a token and get the same token back.
	margin = 2 * time.Minute
	// refetchFloor is how long a token stays servable after a mint, under the
	// margin, while it is valid. For the reason above, a mint can return a
	// token with less than margin left, and without a floor every request
	// would run gcloud.
	refetchFloor = time.Minute
)

type key struct{ sandbox, route string }

// entry holds the token of one sandbox and route, and the mint running for
// it, if any: that mint is the single flight.
type entry struct {
	mu     sync.Mutex
	flight *flight
	token  Token
	have   bool
	// ptr is what the held token was minted for. Re-importing the secret with
	// another mode or service account changes the pointer while a sandbox
	// runs, and a token of the old identity must not be served for the new
	// one.
	ptr Pointer
	// minted is when the held token arrived, for refetchFloor.
	minted time.Time
}

// flight is one mint. done closes once token and err are set.
type flight struct {
	ptr   Pointer
	done  chan struct{}
	token Token
	err   error
}

// Cache holds minted tokens per sandbox and route, in memory only.
type Cache struct {
	mint func(ctx context.Context, p Pointer) (Token, error)
	now  func() time.Time

	mu      sync.Mutex
	entries map[key]*entry
}

// NewCache returns an empty cache. mint is CLI.Mint outside tests. now is the
// clock, so tests need no sleeps.
func NewCache(mint func(ctx context.Context, p Pointer) (Token, error), now func() time.Time) *Cache {
	return &Cache{mint: mint, now: now, entries: map[key]*entry{}}
}

// Get returns a token with at least 2 minutes left, minting when the cached
// one has less. The exception is a token minted within refetchFloor: it is
// returned while it is valid even with less left, because gcloud hands back
// the same token near its expiry and minting again would not help. A failed
// mint is an error even when an older token is cached: the token still held
// is one about to expire, and handing it out would fail in the guest later
// and further from the cause. A caller whose context ends gets its error
// back; the mint goes on and fills the cache.
func (c *Cache) Get(ctx context.Context, sandboxID, route string, p Pointer) (Token, error) {
	return c.get(ctx, sandboxID, route, p)
}

// Ensure mints when Get would, so that registering a sandbox proves a mint
// works and the first request after it is served from the cache.
func (c *Cache) Ensure(ctx context.Context, sandboxID, route string, p Pointer) error {
	_, err := c.get(ctx, sandboxID, route, p)
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

func (c *Cache) get(ctx context.Context, sandboxID, route string, p Pointer) (Token, error) {
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
		if e.flight == nil && e.have && e.ptr == p && e.servable(c.now()) {
			tok := e.token
			e.mu.Unlock()
			return tok, nil
		}
		// Callers that arrive during a mint wait for it, so a burst of
		// requests from one sandbox is one gcloud run.
		f := e.flight
		if f == nil {
			f = &flight{ptr: p, done: make(chan struct{})}
			e.flight = f
			go c.run(ctx, e, f)
		}
		e.mu.Unlock()

		select {
		case <-f.done:
		case <-ctx.Done():
			// The mint goes on without this caller and fills the cache.
			return Token{}, ctx.Err()
		}
		if f.ptr == p {
			return f.token, f.err
		}
		// The mint was for another pointer: look again.
	}
}

// servable reports whether the held token can be served at now: it has
// margin left, or it arrived within refetchFloor and has not expired.
func (e *entry) servable(now time.Time) bool {
	left := e.token.Expiry.Sub(now)
	return left >= margin || (now.Sub(e.minted) < refetchFloor && left > 0)
}

// run runs one flight. It does not end with the request that started it: a
// gcloud mint, an impersonation in particular, can take longer than a guest
// client waits, and a mint killed by every hang-up would restart on every
// retry and never hand out a token. CLI.Mint's own timeout still bounds it.
func (c *Cache) run(ctx context.Context, e *entry, f *flight) {
	tok, err := c.mint(context.WithoutCancel(ctx), f.ptr)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		tok = Token{}
		e.token, e.have, e.ptr = Token{}, false, Pointer{}
	} else {
		e.token, e.have, e.ptr, e.minted = tok, true, f.ptr, c.now()
	}
	f.token, f.err = tok, err
	e.flight = nil
	close(f.done)
}
