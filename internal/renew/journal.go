package renew

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// The renewal journal.
//
// The provider rotates the refresh token single-use. Between its answer and
// the write-back, the only valid pair is in this process's memory, and a
// crash there, or a host that loses power, would leave the store with a
// spent pair: the user's login would be dead. So each renewal is journaled
// in the store, under secret.JournalName, and never in a file of its own:
// the journal can hold a credential, and the store is where credentials
// live.
//
// A renewal walks the journal through these steps, each one store write:
//
//	1. journal "renewing" {startedAt, docHash}, created; it fails if one is there
//	2. the provider is called
//	3. journal "renewed", now also holding the new document
//	4. the secret takes the new document
//	5. the journal is deleted
//
// What a later process finds tells it what happened. recover acts on it.
//
//	journal "renewed", secret still the one it started from: 4 never landed.
//	  The journal's document is promoted, and the provider is not called.
//	journal "renewed", secret holds its document: 5 never landed. The
//	  journal is deleted, and nothing else happens.
//	journal "renewing", secret the one it started from: 2 may or may not
//	  have reached the provider. The secret is kept, the journal is
//	  deleted, and the secret is renewed once at the next Resolve. If the
//	  provider refuses it as spent, the renewal fails with ErrSpent and is
//	  not tried again.
//	any journal, secret changed since it started: the secret was imported
//	  again, which is newer. The journal is deleted.
//
// A journal of another process that still runs, younger than journalLife,
// is left alone, and no renewal starts here until it is gone or older. A
// journal of this cache that it does not know of comes from a create that
// landed and reported a failure. No refresh followed it, so it is deleted.
// A journal that cannot be read, damaged or not a renewed document of the
// secret's kind, is treated like a renewing one.

// journalVersion is the schema of the journal document.
const journalVersion = 1

// The phases a journal is written in.
const (
	phaseRenewing = "renewing"
	phaseRenewed  = "renewed"
)

// journal is the document stored under the journal's name.
type journal struct {
	V     int          `json:"v"`
	Phase string       `json:"phase"`
	Owner journalOwner `json:"owner"`
	// StartedAt is epoch milliseconds.
	StartedAt int64 `json:"startedAt"`
	// DocHash is the SHA-256 of the document the renewal started from.
	DocHash string `json:"docHash"`
	// Doc is the renewed document, in the renewed phase only.
	Doc []byte `json:"doc,omitempty"`
}

// journalOwner names the cache that wrote a journal: its process, the
// process's start time, and a nonce that tells two caches in one process
// apart. The start time tells a pid reused after a reboot from the owner.
type journalOwner struct {
	PID   int    `json:"pid"`
	Start string `json:"start,omitempty"`
	Nonce string `json:"nonce"`
}

// ownerAlive reports whether the process that wrote a journal still runs.
func ownerAlive(o journalOwner) bool {
	if !processAlive(o.PID) {
		return false
	}
	if o.Start == "" {
		return true
	}
	start, err := processStart(o.PID)
	// A pid that answers and has no start time to read counts as the
	// owner. Its journal ages out after journalLife.
	return err != nil || start == o.Start
}

// journalStep is how far the journal of an entry is from cleared.
type journalStep int

const (
	// journalNone: no journal of this entry is in the store.
	journalNone journalStep = iota
	// journalStarted: the journal says renewing, and the renewed document
	// is in memory only.
	journalStarted
	// journalRenewed: the journal holds the renewed document, and the
	// secret does not yet.
	journalRenewed
	// journalClear: the secret is current, and the journal is left to
	// delete.
	journalClear
)

// errBadJournal is a journal brig cannot read: damaged in the store, not
// JSON, or not a schema this brig knows.
var errBadJournal = errors.New("the renewal journal is not one brig can read")

func docHash(doc []byte) string {
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// journalOf returns the renewing journal of the entry's last renewal. The
// caller holds c.mu.
func journalOf(e *entry) journal {
	return journal{V: journalVersion, Phase: phaseRenewing, Owner: e.journalOwner,
		StartedAt: e.startedAt.UnixMilli(), DocHash: e.from}
}

// writeJournal stores j as the journal of name, with the secret's own
// provenance and the expiry moved to expires.
//
// An update of a journal someone removed creates it again: the renewed
// document still has to be in the store before the secret is written.
func writeJournal(s secret.Store, name string, j journal, expires time.Time, update bool) error {
	blob, err := json.Marshal(j)
	if err != nil {
		return err
	}
	p, err := provenanceOf(s, name, expires)
	if err != nil {
		return err
	}
	jname := secret.JournalName(name)
	err = secret.WriteWithProvenance(s, jname, blob, p, update)
	if update && errors.Is(err, secret.ErrNotFound) {
		err = secret.WriteWithProvenance(s, jname, blob, p, false)
	}
	return err
}

// readJournal returns the journal of name, ErrNotFound, or errBadJournal.
func (c *Cache) readJournal(ctx context.Context, name string) (journal, error) {
	rctx, cancel := context.WithTimeout(ctx, c.storeTimeout)
	defer cancel()
	blob, err := secret.WithContext(rctx, c.store).Read(secret.JournalName(name))
	if errors.Is(err, secret.ErrDamaged) {
		// Half a journal: on macOS a key item whose sealed item never
		// landed. It holds nothing to recover.
		return journal{}, errBadJournal
	}
	if err != nil {
		return journal{}, err
	}
	var j journal
	if err := json.Unmarshal(blob, &j); err != nil || j.V != journalVersion ||
		(j.Phase != phaseRenewing && j.Phase != phaseRenewed) {
		return journal{}, errBadJournal
	}
	return j, nil
}

// recover reads the journal of name and settles it. The caller holds the
// entry's turn. The comment at the top of this file lists what each state
// found means.
//
// A store that cannot be read is tried again after retryAfter, and serving
// goes on meanwhile: a renewal creates its journal before anything else, and
// that create fails while another journal is there.
func (c *Cache) recover(ctx context.Context, name string, w secret.Workload, e *entry, r secret.Renewer) error {
	j, jerr := c.readJournal(ctx, name)
	c.mu.Lock()
	now := c.now()
	switch {
	case errors.Is(jerr, secret.ErrNotFound):
		e.recovered, e.recoverFailedAt, e.blockedAt = true, time.Time{}, time.Time{}
		c.mu.Unlock()
		return nil
	case jerr != nil && ctx.Err() != nil:
		// The caller went away, which says nothing about the store, so
		// the journal is not reported as unreadable.
		c.mu.Unlock()
		return jerr
	case jerr != nil && !errors.Is(jerr, errBadJournal):
		e.recoverFailedAt = now
		c.mu.Unlock()
		return nil
	case jerr == nil && j.Owner == c.owner:
		e.recovered, e.recoverFailedAt, e.blockedAt = true, time.Time{}, time.Time{}
		if e.journal == journalNone {
			// This cache's own journal, from a create that landed and
			// reported a failure. The provider is called only after a
			// create succeeds, so no refresh was sent: delete it.
			e.journal = journalClear
		}
		c.mu.Unlock()
		return nil
	case jerr == nil && young(now, j) && c.alive(j.Owner):
		// Another process is renewing, or was a moment ago.
		e.recovered, e.recoverFailedAt, e.blockedAt = true, time.Time{}, now
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	// The journal was left by a process that stopped, or outlived any
	// renewal. What the secret holds now decides what it means.
	v, err := c.read(ctx, name, w)
	if errors.Is(err, secret.ErrNotFound) {
		c.mu.Lock()
		e.reset(nil, time.Time{}, time.Time{})
		e.recovered, e.recoverFailedAt, e.blockedAt = true, time.Time{}, time.Time{}
		e.journal = journalClear
		c.mu.Unlock()
		c.dropJournal(ctx, name, e)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && ctx.Err() != nil {
		return err
	}
	if err != nil {
		e.recoverFailedAt = now
		if e.doc == nil {
			return err
		}
		return nil
	}
	e.recovered, e.recoverFailedAt, e.blockedAt, e.adopted = true, time.Time{}, time.Time{}, true
	if jerr == nil {
		e.startedAt, e.from, e.journalOwner = time.UnixMilli(j.StartedAt), j.DocHash, j.Owner
	}
	if err := c.reconcileLocked(name, e, v, r); err != nil {
		return err
	}
	current := docHash(v)
	switch {
	case jerr != nil:
		// Unreadable: whether a refresh started from v cannot be told, so
		// v is tested the way a renewing journal's document is.
		e.journal, e.suspect = journalClear, true
	case j.Phase == phaseRenewed && j.DocHash == current && len(j.Doc) > 0:
		// The provider answered and the secret never took the answer. The
		// journal holds the only valid pair. flush promotes it, unless it
		// is not a document of the secret's kind: then the journal counts
		// as unreadable, like a secret stored with such a value.
		var expires time.Time
		if x, ok := r.(secret.Expirer); ok {
			var err error
			if expires, err = x.Expiry(j.Doc); err != nil {
				e.journal, e.suspect = journalClear, true
				return nil
			}
		}
		// The refresh was answered about when it started.
		e.doc, e.expires, e.issued, e.journal = j.Doc, expires, time.UnixMilli(j.StartedAt), journalRenewed
		e.failure, e.failedAt, e.spent = nil, time.Time{}, false
	case j.Phase == phaseRenewing && j.DocHash == current:
		// The refresh may or may not have reached the provider, which may
		// have rotated v's refresh token. Do not guess: keep v, clear the
		// journal, and test v with one renewal.
		e.journal, e.suspect = journalClear, true
	default:
		// Promoted before the journal was deleted, or the secret was
		// stored again since the refresh started. Either way the secret
		// is current.
		e.journal = journalClear
	}
	return nil
}

// young reports whether j started less than journalLife before now. A
// journal that starts in the future is not young: a clock that stepped back
// must not hold the secret past its expiry.
func young(now time.Time, j journal) bool {
	age := now.Sub(time.UnixMilli(j.StartedAt))
	return age >= 0 && age < journalLife
}

// dropJournal deletes the journal of name, for a secret that is gone.
func (c *Cache) dropJournal(ctx context.Context, name string, e *entry) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.storeTimeout)
	defer cancel()
	err := secret.WithContext(wctx, c.store).Delete(secret.JournalName(name))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		e.writeFailedAt = c.now()
		return
	}
	e.journal, e.adopted = journalNone, false
}
