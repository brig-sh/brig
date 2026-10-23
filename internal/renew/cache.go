// Package renew keeps renewable credentials current on the host.
//
// It holds the refresher: Cache, which owns each renewable secret in a
// process, and the lifecycles that renew one against its provider. A refresh
// rotates a credential the host may share, so no package on the run path
// links this one (internal/hostsrc/arch_test.go). A run delivers what the
// store holds and never calls a provider.
package renew

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// renewAhead is how long before its expiry a document is renewed. A request
// sent with a token that expires in seconds can be refused upstream. A
// document this cache renewed with a shorter life is renewed halfway
// through it instead, see renewAt.
const renewAhead = 10 * time.Minute

// retryAfter spaces the retries of a failed renewal and of a failed
// write-back. Without it, every request would be one more call to the token
// endpoint, or one more keychain dump.
const retryAfter = 30 * time.Second

// recheck is how long a document is served before the store is read again,
// so a delete or a re-import reaches the cache within it.
const recheck = 5 * time.Minute

// storeTimeout bounds one store call. A hung security(1) would otherwise hold
// the secret's turn, and every caller behind it.
const storeTimeout = 30 * time.Second

// Cache holds one renewable document per secret in memory and renews it.
//
// The provider rotates a refresh token single-use, so every renewal makes the
// previous document invalid. Two sandboxes that share a credential must renew
// it through one owner, one at a time. Cache is that owner within a process.
// Two callers that find the same document due wait on one renewal.
//
// The store stays the source of truth. Cache reads it again before every
// renewal, before every write-back, and at least every recheck. A deleted
// secret stops being served. A document someone else stored, a re-import for
// instance, replaces the one in memory, and a pending write-back never
// overwrites it.
//
// A renewed document is written back before it is served. If the write, or
// the store read before it, fails, Cache keeps the new document in memory
// and retries the write after retryAfter. Until then the store holds a
// document the provider already refuses, and Pending names the secret.
type Cache struct {
	broker       secret.Broker
	store        secret.Store
	renewers     map[string]secret.Renewer
	now          func() time.Time
	storeTimeout time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

// entry is the state of one renewable secret.
type entry struct {
	// turn is held by the one caller that reads the store for, renews or
	// writes back this secret. The others wait on it and then find the
	// result.
	turn chan struct{}

	// The fields below are guarded by Cache.mu.

	// doc is the document served, and stored is what the store held at the
	// last read or write. They differ while a write-back is pending.
	doc, stored []byte
	expires     time.Time
	// issued is when this cache renewed doc, and zero for a document read
	// from the store, whose start of life is not known.
	issued    time.Time
	checkedAt time.Time
	// failure is the last renewal error, set at failedAt. spent means the
	// failed renewal reached the provider, so it is not retried.
	failure       error
	failedAt      time.Time
	spent         bool
	writeFailedAt time.Time
}

// NewCache returns a cache that reads through b and writes renewed documents
// back to s. renewers maps a secret name to its lifecycle. A name with no
// Renewer is read through b on every call and never held.
func NewCache(b secret.Broker, s secret.Store, renewers map[string]secret.Renewer) *Cache {
	return &Cache{
		broker:       b,
		store:        s,
		renewers:     renewers,
		now:          time.Now,
		storeTimeout: storeTimeout,
		entries:      map[string]*entry{},
	}
}

var _ secret.Broker = (*Cache)(nil)

// Resolve returns the current document of name, renewed first when it
// expires within renewAhead.
//
// If that renewal fails, the current document is served until its own
// expiry. After it, Resolve returns the renewal's error.
func (c *Cache) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	r, ok := c.renewers[name]
	if !ok {
		return c.broker.Resolve(ctx, name, w)
	}
	e := c.entry(name)
	if doc, ok := c.ready(e); ok {
		return doc, nil
	}
	if err := e.take(ctx); err != nil {
		return nil, err
	}
	defer e.release()

	read := false
	if c.stale(e) {
		if err := c.sync(ctx, name, w, e, r); err != nil {
			return nil, err
		}
		read = true
	}
	if err := c.flush(ctx, name, w, e, r); err != nil {
		return nil, err
	}
	if c.due(e) {
		// Read the store before renewing: a re-import there is newer than
		// the document in memory and needs no renewal.
		if !read {
			if err := c.sync(ctx, name, w, e, r); err != nil {
				return nil, err
			}
		}
		if c.due(e) {
			// The error is kept on the entry. It matters once the current
			// document has expired, which serve reports.
			_ = c.renew(ctx, name, w, e, r)
		}
	}
	return c.serve(name, e)
}

// ForceRenew renews name once, after the provider refused stale, the
// document a request was sent with.
//
// If the cache already holds a different document, another caller renewed
// it first, or the store holds a new one, and that document is returned
// without a renewal. This is the path for an upstream 401.
func (c *Cache) ForceRenew(ctx context.Context, name string, w secret.Workload, stale []byte) ([]byte, error) {
	r, ok := c.renewers[name]
	if !ok {
		return nil, fmt.Errorf("%q has no lifecycle and cannot be renewed", name)
	}
	e := c.entry(name)
	if err := e.take(ctx); err != nil {
		return nil, err
	}
	defer e.release()

	if err := c.sync(ctx, name, w, e, r); err != nil {
		return nil, err
	}
	c.mu.Lock()
	current := !bytes.Equal(e.doc, stale)
	doc, spent, failure := bytes.Clone(e.doc), e.spent, e.failure
	backoff := c.backingOff(e.failure, e.failedAt)
	c.mu.Unlock()
	switch {
	case current:
		return doc, nil
	case spent:
		return nil, fmt.Errorf("%q is not renewed again after a refresh that failed at the provider: %w",
			name, failure)
	case backoff:
		return nil, fmt.Errorf("%q could not be renewed: %w", name, failure)
	}
	if err := c.renew(ctx, name, w, e, r); err != nil {
		return nil, fmt.Errorf("%q could not be renewed: %w", name, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(e.doc), nil
}

// Pending returns the names of the secrets whose renewed document is held in
// memory and not yet written back, sorted.
func (c *Cache) Pending() []string {
	return c.names(func(e *entry) bool { return e.doc != nil && !bytes.Equal(e.doc, e.stored) })
}

// Failing returns the names of the secrets whose last renewal failed, sorted.
// It carries no error text, so a report built from it holds nothing the
// provider said.
func (c *Cache) Failing() []string {
	return c.names(func(e *entry) bool { return e.failure != nil })
}

func (c *Cache) names(match func(*entry) bool) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, e := range c.entries {
		if match(e) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (c *Cache) entry(name string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok {
		e = &entry{turn: make(chan struct{}, 1)}
		c.entries[name] = e
	}
	return e
}

// ready returns the held document when nothing has to happen first.
func (c *Cache) ready(e *entry) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	switch {
	case e.doc == nil, now.Sub(e.checkedAt) >= recheck,
		c.writeDue(e), c.dueLocked(e), c.expiredLocked(e):
		return nil, false
	}
	return bytes.Clone(e.doc), true
}

func (c *Cache) stale(e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.doc == nil || c.now().Sub(e.checkedAt) >= recheck
}

func (c *Cache) due(e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dueLocked(e)
}

// dueLocked reports whether the document should be renewed now. A spent
// entry is never due: renewing it would send a refresh token the provider
// may already have rotated.
func (c *Cache) dueLocked(e *entry) bool {
	return e.doc != nil && !e.expires.IsZero() && c.now().After(e.renewAt()) &&
		!e.spent && !c.backingOff(e.failure, e.failedAt)
}

// renewAt is when the document is renewed: renewAhead before its expiry, or
// halfway through its life when the cache renewed it and that is later. A
// provider that issues a token shorter-lived than renewAhead would otherwise
// see one refresh per request.
func (e *entry) renewAt() time.Time {
	ahead := renewAhead
	if !e.issued.IsZero() {
		ahead = min(ahead, max(e.expires.Sub(e.issued)/2, 0))
	}
	return e.expires.Add(-ahead)
}

func (c *Cache) expiredLocked(e *entry) bool {
	return !e.expires.IsZero() && !c.now().Before(e.expires)
}

// writeDue reports whether a write-back is pending and not backing off.
func (c *Cache) writeDue(e *entry) bool {
	return e.doc != nil && !bytes.Equal(e.doc, e.stored) &&
		(e.writeFailedAt.IsZero() || c.now().Sub(e.writeFailedAt) >= retryAfter)
}

func (c *Cache) backingOff(failure error, at time.Time) bool {
	return failure != nil && c.now().Sub(at) < retryAfter
}

func (c *Cache) serve(name string, e *entry) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expiredLocked(e) {
		if e.failure != nil {
			return nil, fmt.Errorf("%q expired and could not be renewed: %w", name, e.failure)
		}
		return nil, fmt.Errorf("%q expired", name)
	}
	return bytes.Clone(e.doc), nil
}

// sync reads the store and reconciles the entry with it. The caller holds
// the entry's turn.
//
// A secret no longer in the store is dropped, and sync returns ErrNotFound.
// Any other read failure keeps the document in memory, until the next
// recheck, and is returned only when there is no document to serve.
func (c *Cache) sync(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	_, err := c.syncRead(ctx, name, w, e, r)
	return err
}

// syncRead is sync, and also reports whether the store was read. It is false
// when a read failed and the entry kept its document.
func (c *Cache) syncRead(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) (bool, error) {
	rctx, cancel := context.WithTimeout(ctx, c.storeTimeout)
	defer cancel()
	v, err := c.broker.Resolve(rctx, name, w)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case errors.Is(err, secret.ErrNotFound):
		e.reset(nil, time.Time{}, time.Time{})
		return false, err
	case err != nil:
		if e.doc == nil {
			return false, err
		}
		e.checkedAt = c.now()
		return false, nil
	}
	e.checkedAt = c.now()
	if e.doc != nil && (bytes.Equal(v, e.stored) || bytes.Equal(v, e.doc)) {
		// Nobody else wrote. If the store holds doc, a write-back that
		// reported a failure landed after all.
		e.stored = v
		return true, nil
	}
	// The first read, or a document someone else stored. Whatever memory
	// held is older than it.
	var expires time.Time
	if x, ok := r.(secret.Expirer); ok {
		if expires, err = x.Expiry(v); err != nil {
			e.reset(nil, time.Time{}, time.Time{})
			return true, fmt.Errorf("%q: %w", name, err)
		}
	}
	e.reset(v, expires, e.checkedAt)
	return true, nil
}

// reset replaces the entry's state with doc as both served and stored. It
// leaves turn alone, which waiting callers read without c.mu. The caller
// holds c.mu.
func (e *entry) reset(doc []byte, expires, checkedAt time.Time) {
	e.doc, e.stored, e.expires, e.issued, e.checkedAt = doc, doc, expires, time.Time{}, checkedAt
	e.failure, e.failedAt, e.spent, e.writeFailedAt = nil, time.Time{}, false, time.Time{}
}

// flush writes a pending document back, unless the store now holds a
// document someone else wrote. The caller holds the entry's turn.
//
// It returns an error only when the secret has gone from the store.
func (c *Cache) flush(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	c.mu.Lock()
	due := c.writeDue(e)
	c.mu.Unlock()
	if !due {
		return nil
	}
	// The read runs on a context the caller cannot cancel, like the write.
	// A read that failed cannot tell a re-import from nothing changed, so
	// the write waits for a read that succeeds.
	read, err := c.syncRead(context.WithoutCancel(ctx), name, w, e, r)
	if err != nil {
		return err
	}
	if !read {
		c.mu.Lock()
		e.writeFailedAt = c.now()
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	due = c.writeDue(e)
	doc, expires := bytes.Clone(e.doc), e.expires
	c.mu.Unlock()
	if !due {
		return nil
	}
	// The write runs on a context the caller cannot cancel. A security(1)
	// killed halfway leaves the two keychain items out of step.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.storeTimeout)
	defer cancel()
	err = writeBack(secret.WithContext(wctx, c.store), name, doc, expires)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		e.writeFailedAt = c.now()
		return nil
	}
	e.stored, e.writeFailedAt = doc, time.Time{}
	return nil
}

// renew replaces the held document with a renewed one and writes it back.
// The caller holds the entry's turn.
//
// The renewal runs on a context the caller cannot cancel. A refresh the
// provider has already answered rotates the refresh token, and a canceled
// read of that answer would lose the only valid pair.
func (c *Cache) renew(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	c.mu.Lock()
	current := bytes.Clone(e.doc)
	c.mu.Unlock()
	next, err := r.Renew(context.WithoutCancel(ctx), current)
	if err == nil && len(next) == 0 {
		err = fmt.Errorf("the renewal returned an empty document: %w", secret.ErrSpent)
	}
	if err != nil {
		c.mu.Lock()
		e.failure, e.failedAt = err, c.now()
		e.spent = errors.Is(err, secret.ErrSpent)
		c.mu.Unlock()
		return err
	}
	// The old document is dead at the provider now, so the new one is kept
	// even when its expiry cannot be read. It is then renewed on a 401.
	var expires time.Time
	if x, ok := r.(secret.Expirer); ok {
		expires, _ = x.Expiry(next)
	}
	c.mu.Lock()
	e.doc, e.expires, e.issued = next, expires, c.now()
	e.failure, e.failedAt, e.spent, e.writeFailedAt = nil, time.Time{}, false, time.Time{}
	c.mu.Unlock()
	return c.flush(ctx, name, w, e, r)
}

// writeBack replaces a renewed value and keeps its provenance, with the
// expiry moved to expiresAt.
//
// Store.Update writes the zero provenance. An imported credential would then
// read back as hand-created, and the next `brig secret import` would refuse
// it without -y. So the provenance is read from List, which decrypts
// nothing, and written again. A secret with no provenance keeps none.
func writeBack(s secret.Store, name string, value []byte, expiresAt time.Time) error {
	if _, ok := s.(secret.Annotator); !ok {
		return s.Update(name, value)
	}
	all, err := s.List()
	if err != nil {
		return fmt.Errorf("reading the provenance of %q: %w", name, err)
	}
	i := slices.IndexFunc(all, func(sec secret.Secret) bool { return sec.Name == name })
	if i < 0 {
		return secret.ErrNotFound
	}
	p := all[i].Provenance
	if !p.IsZero() {
		p.ExpiresAt = 0
		if !expiresAt.IsZero() {
			p.ExpiresAt = expiresAt.UnixMilli()
		}
	}
	return secret.WriteWithProvenance(s, name, value, p, true)
}

func (e *entry) take(ctx context.Context) error {
	select {
	case e.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *entry) release() { <-e.turn }
