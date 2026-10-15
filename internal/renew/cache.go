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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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

// journalLife is how long the journal of another live process holds off a
// renewal here. A refresh takes seconds. A journal older than this was left
// by a process that stopped without clearing it, and must not hold the
// secret for good.
const journalLife = 10 * time.Minute

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
// overwrites it. A renewal whose read fails waits for one that succeeds,
// retryAfter later at the earliest. A secret deleted, or stored again in a
// form the renewer cannot read, during a renewal is an error for its
// caller.
//
// A renewed document is written back before it is served. If the write, or
// the store read before it, fails, Cache keeps the new document in memory
// and retries the write after retryAfter. Until then the store holds a
// document the provider already refuses, and Pending names the secret.
//
// Each renewal is journaled in the store, next to the secret, so a crash
// between the provider's answer and the write-back does not lose the only
// valid pair. See journal.go.
type Cache struct {
	broker       secret.Broker
	store        secret.Store
	renewers     map[string]secret.Renewer
	now          func() time.Time
	storeTimeout time.Duration
	// owner names this cache in the journals it writes.
	owner journalOwner
	// alive reports whether the process that wrote a journal still runs.
	alive func(journalOwner) bool

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

	// journal is how far the journal of the last renewal is from cleared,
	// and the fields after it are what the journal says.
	journal      journalStep
	startedAt    time.Time
	from         string
	journalOwner journalOwner
	// recovered is set once the journal in the store has been read. A
	// read that failed at recoverFailedAt is tried again after retryAfter.
	recovered       bool
	recoverFailedAt time.Time
	// blockedAt is when the journal of another live process was last
	// found. No renewal starts while it is set.
	blockedAt time.Time
	// suspect is a document a stopped process may have renewed: its
	// journal says a refresh started from it and holds no answer. One
	// renewal finds out whether the provider still takes it.
	suspect bool
	// adopted is a journal left by another process, until it is cleared.
	adopted bool
}

// NewCache returns a cache that reads through b and writes renewed documents
// back to s. renewers maps a secret name to its lifecycle. A name with no
// Renewer is read through b on every call and never held.
//
// NewCache does not touch the store. The first Resolve, ForceRenew or
// Recover of a secret settles the journal a stopped process left for it.
func NewCache(b secret.Broker, s secret.Store, renewers map[string]secret.Renewer) *Cache {
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	return &Cache{
		broker:       b,
		store:        s,
		renewers:     renewers,
		now:          time.Now,
		storeTimeout: storeTimeout,
		owner:        journalOwner{PID: os.Getpid(), Start: selfStart(), Nonce: hex.EncodeToString(nonce)},
		alive:        ownerAlive,
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

	if c.unsettled(e) {
		if err := c.recover(ctx, name, w, e, r); err != nil {
			return nil, err
		}
	}
	read := false
	if c.stale(e) {
		var err error
		if read, err = c.syncRead(ctx, name, w, e, r); err != nil {
			return nil, err
		}
	}
	if err := c.flush(ctx, name, w, e, r); err != nil {
		return nil, err
	}
	if c.due(e) {
		// Read the store before renewing: a re-import there is newer than
		// the document in memory and needs no renewal.
		if !read {
			var err error
			if read, err = c.syncRead(ctx, name, w, e, r); err != nil {
				return nil, err
			}
		}
		switch {
		case !read:
			// A read that failed cannot tell a re-import from nothing
			// changed. A renewal now could spend the refresh token on a
			// pair that the next good read drops for the re-import, so it
			// waits for a read that succeeds, like a write-back does.
			c.holdRenewal(e)
		case c.due(e):
			// A renewal error is kept on the entry. It matters once the
			// current document has expired, which serve reports. An error
			// that cleared the entry is returned: the read before the
			// write-back found the secret deleted, or stored again in a
			// form the renewer cannot read.
			if err := c.renew(ctx, name, w, e, r); err != nil && c.cleared(e) {
				return nil, err
			}
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
//
// A renewed document not yet written back is never renewed again before the
// write lands, so the store always holds the journal or the secret of the
// one refresh in flight.
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

	if c.unsettled(e) {
		if err := c.recover(ctx, name, w, e, r); err != nil {
			return nil, err
		}
	}
	read, err := c.syncRead(ctx, name, w, e, r)
	if err != nil {
		return nil, err
	}
	if err := c.flush(ctx, name, w, e, r); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if read && errors.Is(e.failure, errUnread) {
		// A hold waits for a read that succeeds, and this one did.
		e.failure, e.failedAt = nil, time.Time{}
	}
	current := !bytes.Equal(e.doc, stale)
	doc, spent, failure := bytes.Clone(e.doc), e.spent, e.failure
	pending := c.pendingLocked(e)
	blocked := !e.blockedAt.IsZero()
	backoff := c.backingOff(e.failure, e.failedAt)
	c.mu.Unlock()
	switch {
	case current:
		return doc, nil
	case !read:
		// As in Resolve, a renewal waits for a read that succeeds.
		return nil, fmt.Errorf("%q could not be renewed: %w", name, errUnread)
	case spent:
		return nil, fmt.Errorf("%q is not renewed again after a refresh that failed at the provider: %w",
			name, failure)
	case pending:
		return nil, fmt.Errorf("%q was renewed and the new document is not yet in the store, "+
			"so it is not renewed again until the write lands", name)
	case blocked:
		return nil, fmt.Errorf("%q is being renewed by another process, whose journal is in the store", name)
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

// Recover settles the journal another process left for name, and does
// nothing more when there is none. Resolve and ForceRenew do the same before
// their first renewal, so calling Recover only moves that work earlier: a
// broker calls it when it starts serving a secret.
func (c *Cache) Recover(ctx context.Context, name string) error {
	r, ok := c.renewers[name]
	if !ok {
		return nil
	}
	e := c.entry(name)
	if err := e.take(ctx); err != nil {
		return err
	}
	defer e.release()
	if !c.unsettled(e) {
		return nil
	}
	if err := c.recover(ctx, name, secret.Workload{}, e, r); err != nil {
		return err
	}
	return c.flush(ctx, name, secret.Workload{}, e, r)
}

// Pending returns the names of the secrets whose renewed document is held in
// memory and not yet written back, sorted. A journal still to write or to
// delete counts: until it lands, no renewal of the secret starts.
func (c *Cache) Pending() []string {
	return c.names(c.pendingLocked)
}

// Failing returns the names of the secrets whose last renewal failed, sorted.
// It carries no error text, so a report built from it holds nothing the
// provider said.
func (c *Cache) Failing() []string {
	return c.names(func(e *entry) bool { return e.failure != nil })
}

// Recovering returns the names of the secrets whose journal is not settled,
// sorted: one another process holds, one a stopped process left that this
// cache is clearing, one whose journal could not be read, and a document it
// is testing with one renewal because a stopped process may already have
// renewed it.
func (c *Cache) Recovering() []string {
	return c.names(func(e *entry) bool {
		return !e.blockedAt.IsZero() || e.suspect || e.adopted || !e.recoverFailedAt.IsZero()
	})
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
	case e.doc == nil, now.Sub(e.checkedAt) >= recheck, c.unsettledLocked(e),
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

// unsettled reports whether the journal in the store has to be read before
// anything else: never read yet, or held by another process when last read.
func (c *Cache) unsettled(e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsettledLocked(e)
}

func (c *Cache) unsettledLocked(e *entry) bool {
	now := c.now()
	if !e.recovered {
		return e.recoverFailedAt.IsZero() || now.Sub(e.recoverFailedAt) >= retryAfter
	}
	return !e.blockedAt.IsZero() && now.Sub(e.blockedAt) >= retryAfter
}

func (c *Cache) due(e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dueLocked(e)
}

// dueLocked reports whether the document should be renewed now. A spent
// entry is never due: renewing it would send a refresh token the provider
// may already have rotated. Nor is one whose last renewal is still on its
// way into the store, or one another process holds the journal of.
func (c *Cache) dueLocked(e *entry) bool {
	if e.doc == nil || e.spent || c.backingOff(e.failure, e.failedAt) ||
		c.pendingLocked(e) || !e.blockedAt.IsZero() {
		return false
	}
	return e.suspect || (!e.expires.IsZero() && c.now().After(e.renewAt()))
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

// pendingLocked reports whether a store write of the last renewal, or of the
// journal being cleared, has yet to land.
func (c *Cache) pendingLocked(e *entry) bool {
	return e.journal != journalNone || (e.doc != nil && !bytes.Equal(e.doc, e.stored))
}

// writeDue reports whether a write is pending and not backing off.
func (c *Cache) writeDue(e *entry) bool {
	return c.pendingLocked(e) &&
		(e.writeFailedAt.IsZero() || c.now().Sub(e.writeFailedAt) >= retryAfter)
}

func (c *Cache) backingOff(failure error, at time.Time) bool {
	return failure != nil && c.now().Sub(at) < retryAfter
}

// errUnread is the renewal failure while the store cannot be read.
var errUnread = errors.New("the store could not be read before the renewal")

// holdRenewal records errUnread as the renewal's failure, so the entry is
// not due again until retryAfter. A read in ForceRenew that succeeds lifts
// it sooner.
func (c *Cache) holdRenewal(e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.failure, e.failedAt, e.spent = errUnread, c.now(), false
}

// cleared reports whether the entry holds no document.
func (c *Cache) cleared(e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.doc == nil
}

func (c *Cache) serve(name string, e *entry) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.doc == nil {
		// Every path that clears the entry returns its error first. An
		// empty credential is never served in place of one.
		return nil, fmt.Errorf("%q: %w", name, secret.ErrNotFound)
	}
	if c.expiredLocked(e) {
		switch {
		case e.failure != nil:
			return nil, fmt.Errorf("%q expired and could not be renewed: %w", name, e.failure)
		case !e.blockedAt.IsZero():
			return nil, fmt.Errorf("%q expired while another process holds its renewal journal", name)
		}
		return nil, fmt.Errorf("%q expired", name)
	}
	return bytes.Clone(e.doc), nil
}

// syncRead reads the store and reconciles the entry with it, and reports
// whether the store was read. The caller holds the entry's turn.
//
// A secret no longer in the store is dropped, and syncRead returns
// ErrNotFound. A read cut short by the caller's context is returned and
// changes nothing. Any other read failure keeps the document in memory,
// until the next recheck, and is returned only when there is no document to
// serve. Otherwise read is false, and nothing that a re-import would change
// is decided on the document in memory.
func (c *Cache) syncRead(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) (read bool, err error) {
	v, err := c.read(ctx, name, w)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case errors.Is(err, secret.ErrNotFound):
		e.reset(nil, time.Time{}, time.Time{})
		return false, err
	case err != nil && ctx.Err() != nil:
		// The caller was canceled or ran out of time, which says nothing
		// about the store. Nothing is recorded: a hold here would refuse
		// the renewal of every other caller while the store is healthy.
		return false, err
	case err != nil:
		if e.doc == nil {
			return false, err
		}
		e.checkedAt = c.now()
		return false, nil
	}
	return true, c.reconcileLocked(name, e, v, r)
}

// read reads the secret, bounded by storeTimeout.
func (c *Cache) read(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, c.storeTimeout)
	defer cancel()
	return c.broker.Resolve(rctx, name, w)
}

// reconcileLocked takes v, just read from the store, into the entry. The
// caller holds c.mu.
func (c *Cache) reconcileLocked(name string, e *entry, v []byte, r secret.Renewer) error {
	e.checkedAt = c.now()
	if e.doc != nil && (bytes.Equal(v, e.stored) || bytes.Equal(v, e.doc)) {
		// Nobody else wrote. If the store holds doc, a write-back that
		// reported a failure landed after all.
		e.stored = v
		return nil
	}
	// The first read, or a document someone else stored. Whatever memory
	// held is older than it.
	var expires time.Time
	if x, ok := r.(secret.Expirer); ok {
		var err error
		if expires, err = x.Expiry(v); err != nil {
			e.reset(nil, time.Time{}, time.Time{})
			return fmt.Errorf("%q: %w", name, err)
		}
	}
	e.reset(v, expires, e.checkedAt)
	return nil
}

// reset replaces the entry's state with doc as both served and stored. It
// leaves turn alone, which waiting callers read without c.mu, and the
// journal, which is still in the store. The caller holds c.mu.
func (e *entry) reset(doc []byte, expires, checkedAt time.Time) {
	e.doc, e.stored, e.expires, e.issued, e.checkedAt = doc, doc, expires, time.Time{}, checkedAt
	e.failure, e.failedAt, e.spent, e.writeFailedAt = nil, time.Time{}, false, time.Time{}
	e.suspect = false
}

// flush moves the last renewal on into the store, one durable step per
// write, unless the store now holds a document someone else wrote. The
// caller holds the entry's turn.
//
// The steps are: the journal takes the renewed document, the secret takes
// it, the journal goes. A write that fails stops the walk, and the next
// flush after retryAfter resumes it where it stopped. Every write of a
// renewal goes through here, under the turn, so a retry never lands over a
// later step.
//
// It returns an error only when the secret has gone from the store.
func (c *Cache) flush(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	c.mu.Lock()
	due := c.writeDue(e)
	c.mu.Unlock()
	if !due {
		return nil
	}
	// The read runs on a context the caller cannot cancel, like the writes.
	// A read that failed cannot tell a re-import from nothing changed, so
	// the writes wait for a read that succeeds.
	read, err := c.syncRead(context.WithoutCancel(ctx), name, w, e, r)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			// The secret was deleted. Its journal goes with it.
			c.dropJournal(ctx, name, e)
		}
		return err
	}
	if !read {
		c.mu.Lock()
		e.writeFailedAt = c.now()
		c.mu.Unlock()
		return nil
	}
	// The writes run on a context the caller cannot cancel. A security(1)
	// killed halfway leaves the two keychain items out of step.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.storeTimeout)
	defer cancel()
	store := secret.WithContext(wctx, c.store)
	for {
		c.mu.Lock()
		if !c.writeDue(e) {
			c.mu.Unlock()
			return nil
		}
		if e.journal != journalNone && bytes.Equal(e.doc, e.stored) {
			// The secret holds the document, or one someone else stored
			// that is newer. Only the journal is left.
			e.journal = journalClear
		}
		step, doc, expires, j := e.journal, bytes.Clone(e.doc), e.expires, journalOf(e)
		c.mu.Unlock()

		var err error
		switch step {
		case journalStarted:
			// The renewed document goes into the journal before the secret
			// is touched, so a crash from here on leaves it in the store.
			j.Phase, j.Doc = phaseRenewed, doc
			err = writeJournal(store, name, j, expires, true)
		case journalClear:
			err = store.Delete(secret.JournalName(name))
			if errors.Is(err, secret.ErrNotFound) {
				err = nil
			}
		default:
			err = writeBack(store, name, doc, expires)
		}

		c.mu.Lock()
		if err != nil {
			e.writeFailedAt = c.now()
			c.mu.Unlock()
			return nil
		}
		e.writeFailedAt = time.Time{}
		switch step {
		case journalStarted:
			e.journal = journalRenewed
		case journalClear:
			e.journal, e.adopted = journalNone, false
		default:
			e.stored = doc
			if e.journal == journalRenewed {
				e.journal = journalClear
			}
		}
		c.mu.Unlock()
	}
}

// renew replaces the held document with a renewed one and writes it back.
// The caller holds the entry's turn.
//
// The journal is written before the provider is called, and a renewal whose
// journal cannot be written is not sent: without it, a crash after the
// provider's answer would leave nothing in the store that holds the new
// refresh token.
//
// The renewal runs on a context the caller cannot cancel. A refresh the
// provider has already answered rotates the refresh token, and a canceled
// read of that answer would lose the only valid pair.
func (c *Cache) renew(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	c.mu.Lock()
	current := bytes.Clone(e.doc)
	expires := e.expires
	pending := c.pendingLocked(e)
	started := c.now()
	c.mu.Unlock()
	// Both callers check this first, and the create below fails while the
	// pending journal is in the store. This guard is the third of three.
	if pending {
		return errors.New("the last renewal is not yet in the store")
	}

	// The create runs on a context the caller cannot cancel, like every
	// other store write. On macOS it is two security(1) calls, and a
	// caller that went away between them would leave half a journal.
	j := journal{V: journalVersion, Phase: phaseRenewing, Owner: c.owner,
		StartedAt: started.UnixMilli(), DocHash: docHash(current)}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.storeTimeout)
	err := writeJournal(secret.WithContext(wctx, c.store), name, j, expires, false)
	cancel()
	switch {
	case errors.Is(err, secret.ErrExists):
		// A journal is in the store although the last read found none:
		// another process journaled a renewal since, or this cache's own
		// create landed and reported a failure. The journal is read again
		// to tell which. The renewal backs off and is reported failing.
		c.mu.Lock()
		e.recovered, e.recoverFailedAt = false, time.Time{}
		c.mu.Unlock()
		if err := c.recover(ctx, name, w, e, r); err != nil {
			return err
		}
		err = errors.New("the refresh was not sent, because a renewal journal is already in the store")
		c.mu.Lock()
		e.failure, e.failedAt, e.spent = err, c.now(), false
		c.mu.Unlock()
		return err
	case err != nil:
		err = fmt.Errorf("the refresh was not sent, because its journal could not be written: %w", err)
		c.mu.Lock()
		e.failure, e.failedAt, e.spent = err, c.now(), false
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	e.journal, e.startedAt, e.from, e.journalOwner = journalStarted, started, j.DocHash, c.owner
	c.mu.Unlock()

	next, err := r.Renew(context.WithoutCancel(ctx), current)
	if err == nil && len(next) == 0 {
		err = fmt.Errorf("the renewal returned an empty document: %w", secret.ErrSpent)
	}
	if err != nil {
		c.mu.Lock()
		e.failure, e.failedAt = err, c.now()
		e.spent = errors.Is(err, secret.ErrSpent)
		if e.spent {
			// The provider answered, so a suspect document is settled.
			e.suspect = false
		}
		// Nothing new to keep, so the journal only has to go.
		e.journal = journalClear
		c.mu.Unlock()
		_ = c.flush(ctx, name, w, e, r)
		return err
	}
	// The old document is dead at the provider now, so the new one is kept
	// even when its expiry cannot be read. It is then renewed on a 401.
	var nextExpires time.Time
	if x, ok := r.(secret.Expirer); ok {
		nextExpires, _ = x.Expiry(next)
	}
	c.mu.Lock()
	e.doc, e.expires, e.issued = next, nextExpires, c.now()
	e.failure, e.failedAt, e.spent, e.writeFailedAt = nil, time.Time{}, false, time.Time{}
	e.suspect = false
	c.mu.Unlock()
	return c.flush(ctx, name, w, e, r)
}

// writeBack replaces a renewed value and keeps its provenance, with the
// expiry moved to expiresAt.
//
// The write is not conditional on what the store holds. Only one cache
// writes a secret: a broker keeps one cache per secret, and the broker lock
// keeps one broker per state directory. flush reads the store just before,
// so a re-import is not overwritten.
//
// Store.Update writes the zero provenance. An imported credential would then
// read back as hand-created, and the next `brig secret import` would refuse
// it without -y. So the provenance is read from List, which decrypts
// nothing, and written again. A secret with no provenance keeps none.
func writeBack(s secret.Store, name string, value []byte, expiresAt time.Time) error {
	if _, ok := s.(secret.Annotator); !ok {
		return s.Update(name, value)
	}
	p, err := provenanceOf(s, name, expiresAt)
	if err != nil {
		return err
	}
	return secret.WriteWithProvenance(s, name, value, p, true)
}

// provenanceOf returns the provenance of name, read from List, with the
// expiry moved to expiresAt. A store that carries none returns the zero
// value.
func provenanceOf(s secret.Store, name string, expiresAt time.Time) (secret.Provenance, error) {
	if _, ok := s.(secret.Annotator); !ok {
		return secret.Provenance{}, nil
	}
	all, err := s.List()
	if err != nil {
		return secret.Provenance{}, fmt.Errorf("reading the provenance of %q: %w", name, err)
	}
	i := slices.IndexFunc(all, func(sec secret.Secret) bool { return sec.Name == name })
	if i < 0 {
		return secret.Provenance{}, secret.ErrNotFound
	}
	p := all[i].Provenance
	if !p.IsZero() {
		p.ExpiresAt = 0
		if !expiresAt.IsZero() {
			p.ExpiresAt = expiresAt.UnixMilli()
		}
	}
	return p, nil
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
