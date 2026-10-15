package renew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// credJournal is the store name of the fixture secret's journal.
var credJournal = secret.JournalName(credName)

// storedJournal returns the journal in the store, and false when there is
// none.
func (f *cacheFixture) storedJournal(t *testing.T) (journal, bool) {
	t.Helper()
	blob, err := f.store.Read(credJournal)
	if errors.Is(err, secret.ErrNotFound) {
		return journal{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(blob, &j); err != nil {
		t.Fatalf("the journal is not JSON: %v", err)
	}
	return j, true
}

// restart returns a second cache over the fixture's store and provider, as
// the broker that starts after the first one stopped. The first one's
// process counts as gone.
func (f *cacheFixture) restart() *Cache {
	c := NewCache(f.broker, f.store, map[string]secret.Renewer{credName: f.renewer})
	c.now = f.clock.Now
	c.alive = func(journalOwner) bool { return false }
	return c
}

// crashMidRenewal renews through the fixture's cache and stops it where a
// crash would: the writes named in fail are refused from the moment the
// provider is called. The cache is not used again.
func (f *cacheFixture) crashMidRenewal(t *testing.T, fail func()) {
	t.Helper()
	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started := f.renewer.started
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.cache.Resolve(context.Background(), credName, testWorkload)
	}()
	<-started
	fail()
	close(f.renewer.release)
	<-done
	f.renewer.mu.Lock()
	f.renewer.started, f.renewer.release = nil, nil
	f.renewer.mu.Unlock()
	f.cache = nil
}

func resolveWith(t *testing.T, c *Cache) []byte {
	t.Helper()
	doc, err := c.Resolve(context.Background(), credName, testWorkload)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return doc
}

// A crash after the provider answered and before any write leaves only the
// renewing journal. Whether the provider rotated the stored refresh token
// cannot be told from the store, so the next broker keeps the stored
// document, clears the journal and renews once. Here the provider had
// rotated it: the answer is ErrSpent, reported, and not asked again.
func TestACrashBeforeAnyWriteRenewsOnceAndReportsSpent(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.crashMidRenewal(t, func() {
		f.store.failWritesOf(credJournal, 1)
		f.store.failWritesOf(credName, 1)
	})
	if j, ok := f.storedJournal(t); !ok || j.Phase != phaseRenewing || j.DocHash != docHash(f.initial) {
		t.Fatalf("after the crash the journal is %+v (present %v), want renewing from the stored document", j, ok)
	}

	c := f.restart()
	if err := c.Recover(context.Background(), credName); err != nil {
		t.Fatal(err)
	}
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Errorf("recovery changed the secret to %q", got)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("recovery left the journal")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("recovery called the provider: %d calls, want the crashed one only", n)
	}
	if got := c.Recovering(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Recovering = %v before the test renewal, want [%s]", got, credName)
	}

	if got := resolveWith(t, c); !bytes.Equal(got, f.initial) {
		t.Errorf("Resolve = %q, want the stored document until it expires", got)
	}
	if n := f.renewer.callCount(); n != 2 {
		t.Fatalf("the provider got %d calls, want the crashed one and one test renewal", n)
	}
	if got := c.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Failing = %v, want [%s]", got, credName)
	}
	if got := c.Recovering(); len(got) != 0 {
		t.Errorf("Recovering = %v after the provider answered, want none", got)
	}
	if _, err := c.ForceRenew(context.Background(), credName, testWorkload, f.initial); !errors.Is(err, secret.ErrSpent) {
		t.Errorf("ForceRenew = %v, want ErrSpent", err)
	}
	for range 4 {
		f.clock.Advance(retryAfter)
		_, _ = c.Resolve(context.Background(), credName, testWorkload)
	}
	if n := f.renewer.callCount(); n != 2 {
		t.Errorf("a spent document was renewed %d more time(s)", n-2)
	}
	f.clock.Advance(5 * time.Minute)
	if _, err := c.Resolve(context.Background(), credName, testWorkload); !errors.Is(err, secret.ErrSpent) {
		t.Errorf("Resolve after expiry = %v, want ErrSpent", err)
	}
	f.renewer.mu.Lock()
	reused := f.renewer.reused
	f.renewer.mu.Unlock()
	if reused != 1 {
		t.Errorf("the rotated document was sent %d time(s) after the crash, want exactly once", reused)
	}
}

// A renewing journal whose refresh never left the host: the test renewal
// succeeds, and the login lives on.
func TestAJournalWhoseRefreshNeverLeftRenewsOnce(t *testing.T) {
	f := newCacheFixture(t, 8*time.Hour)
	j := journal{V: journalVersion, Phase: phaseRenewing, Owner: journalOwner{PID: 1 << 30, Nonce: "gone"},
		StartedAt: f.clock.Now().UnixMilli(), DocHash: docHash(f.initial)}
	plantJournal(t, f.store, j)

	c := f.restart()
	renewed := resolveWith(t, c)
	if bytes.Equal(renewed, f.initial) {
		t.Fatal("the document a stopped process may have renewed was not tested with a renewal")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want one", n)
	}
	if got := f.store.value(credName); !bytes.Equal(got, renewed) {
		t.Error("the test renewal was not written back")
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal is still in the store")
	}
	if len(c.Recovering()) != 0 || len(c.Failing()) != 0 {
		t.Errorf("Recovering %v, Failing %v after a good renewal", c.Recovering(), c.Failing())
	}
	f.renewer.checkNoReuse(t)
}

func plantJournal(t *testing.T, s *memStore, j journal) {
	t.Helper()
	blob, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(credJournal, blob, importedFrom, false); err != nil {
		t.Fatal(err)
	}
}

// A crash after the journal took the provider's answer and before the
// secret did: the journal's document is promoted, the provider is not
// called, and the old document is gone from the store.
func TestACrashAfterTheJournalPromotesIt(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.crashMidRenewal(t, func() { f.store.failWritesOf(credName, 1) })
	j, ok := f.storedJournal(t)
	if !ok || j.Phase != phaseRenewed || len(j.Doc) == 0 {
		t.Fatalf("after the crash the journal is %+v, want renewed with the new document", j)
	}

	c := f.restart()
	if err := c.Recover(context.Background(), credName); err != nil {
		t.Fatal(err)
	}
	if got := f.store.value(credName); !bytes.Equal(got, j.Doc) {
		t.Errorf("the secret is %q after recovery, want the journal's document", got)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal is still in the store")
	}
	if got := resolveWith(t, c); !bytes.Equal(got, j.Doc) {
		t.Errorf("Resolve = %q, want the promoted document", got)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want only the one before the crash", n)
	}
	if want := f.clock.Now().Add(8 * time.Hour).Unix(); f.store.provenance(credName).ExpiresAt/1000 != want {
		t.Errorf("the promoted secret's provenance = %+v, want its expiry moved", f.store.provenance(credName))
	}
	if len(c.Recovering()) != 0 || len(c.Pending()) != 0 {
		t.Errorf("Recovering %v, Pending %v after the promotion", c.Recovering(), c.Pending())
	}
	f.renewer.checkNoReuse(t)
}

// A crash after the promotion and before the journal was deleted: the
// journal goes, and nothing else is written or called.
func TestACrashAfterPromotionOnlyClearsTheJournal(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.crashMidRenewal(t, func() { f.store.failDeletesOf(credJournal, 1) })
	renewed := f.store.value(credName)
	if bytes.Equal(renewed, f.initial) {
		t.Fatal("the secret was not promoted before the crash")
	}
	if _, ok := f.storedJournal(t); !ok {
		t.Fatal("the journal is not in the store after the crash")
	}
	writes, _ := f.store.counts()

	c := f.restart()
	if err := c.Recover(context.Background(), credName); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal is still in the store")
	}
	if w, _ := f.store.counts(); w != writes {
		t.Errorf("recovery made %d writes, want none", w-writes)
	}
	if got := resolveWith(t, c); !bytes.Equal(got, renewed) {
		t.Errorf("Resolve = %q, want the promoted document", got)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want only the one before the crash", n)
	}
	if len(c.Recovering()) != 0 {
		t.Errorf("Recovering = %v, want none", c.Recovering())
	}
}

// A promotion whose write fails: the journal keeps the document, which is
// served from memory, and the write is retried until it lands. A refusal
// upstream meanwhile does not renew again, so the store never lacks the one
// valid pair.
func TestAFailedPromotionIsServedAndRetried(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.crashMidRenewal(t, func() { f.store.failWritesOf(credName, 1) })
	j, _ := f.storedJournal(t)

	c := f.restart()
	f.store.failWritesOf(credName, 2)
	if got := resolveWith(t, c); !bytes.Equal(got, j.Doc) {
		t.Fatalf("Resolve = %q, want the journal's document from memory", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Fatal("the refused promotion changed the secret")
	}
	if kept, ok := f.storedJournal(t); !ok || !bytes.Equal(kept.Doc, j.Doc) {
		t.Fatal("the journal was cleared before the promotion landed")
	}
	if !slices.Equal(c.Pending(), []string{credName}) || !slices.Equal(c.Recovering(), []string{credName}) {
		t.Errorf("Pending %v, Recovering %v, want [%s] in both", c.Pending(), c.Recovering(), credName)
	}

	var wg sync.WaitGroup
	for _, stale := range [][]byte{f.initial, j.Doc, f.initial, j.Doc} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := c.ForceRenew(context.Background(), credName, testWorkload, stale)
			switch {
			case bytes.Equal(stale, f.initial) && !bytes.Equal(doc, j.Doc):
				t.Errorf("ForceRenew with the old document = %q, %v; want the journal's", doc, err)
			case bytes.Equal(stale, j.Doc) && err == nil:
				t.Error("ForceRenew renewed a document that is not in the store yet")
			}
		}()
	}
	wg.Wait()
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls during the pending promotion, want only the crashed one", n)
	}

	f.clock.Advance(retryAfter)
	resolveWith(t, c)
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Fatal("the second refused promotion changed the secret")
	}
	f.clock.Advance(retryAfter)
	if got := resolveWith(t, c); !bytes.Equal(got, j.Doc) {
		t.Errorf("Resolve = %q, want the promoted document", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, j.Doc) {
		t.Errorf("the secret is %q after the retry, want the journal's document", got)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal outlived the promotion")
	}
	if len(c.Pending()) != 0 || len(c.Recovering()) != 0 {
		t.Errorf("Pending %v, Recovering %v after the promotion landed", c.Pending(), c.Recovering())
	}
	f.renewer.checkNoReuse(t)
}

// Two caches over one store, in two live processes: while one renews, the
// other finds its journal, does not renew, and reports the secret as
// recovering. After the journal is gone both serve the one renewed document.
func TestTwoCachesOverOneStoreAgree(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	other := NewCache(f.broker, f.store, map[string]secret.Renewer{credName: f.renewer})
	other.now = f.clock.Now

	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started, release := f.renewer.started, f.renewer.release
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan []byte, 1)
	go func() { done <- f.resolve(t) }()
	<-started

	served := make(chan []byte, 1)
	go func() { served <- resolveWith(t, other) }()
	select {
	case got := <-served:
		if !bytes.Equal(got, f.initial) {
			t.Errorf("the second cache served %q while the first renewed, want the stored document", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second cache renewed past the first one's journal, and waits on the provider")
	}
	if got := other.Recovering(); !slices.Equal(got, []string{credName}) {
		t.Errorf("the second cache's Recovering = %v, want [%s]", got, credName)
	}
	if _, err := other.ForceRenew(context.Background(), credName, testWorkload, f.initial); err == nil {
		t.Error("the second cache renewed past the first one's journal")
	}
	once.Do(func() { close(release) })
	first := <-done

	f.clock.Advance(retryAfter)
	if got := resolveWith(t, other); !bytes.Equal(got, first) {
		t.Errorf("the second cache served %q, want the first one's renewal %q", got, first)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls from two caches, want one", n)
	}
	if len(other.Recovering()) != 0 {
		t.Errorf("Recovering = %v after the journal was cleared", other.Recovering())
	}
	f.renewer.checkNoReuse(t)
}

// A journal of a live process holds off a renewal for journalLife, and no
// longer: a process that hangs, or a pid that was reused, must not hold the
// secret for good.
func TestAYoungJournalBlocksAndAStaleOneIsIgnored(t *testing.T) {
	f := newCacheFixture(t, 12*time.Minute)
	plantJournal(t, f.store, journal{V: journalVersion, Phase: phaseRenewing,
		Owner:     journalOwner{PID: 4242, Nonce: "live"},
		StartedAt: f.clock.Now().UnixMilli(), DocHash: docHash(f.initial)})
	f.cache.alive = func(o journalOwner) bool { return o.PID == 4242 }

	for range 3 {
		if got := f.resolve(t); !bytes.Equal(got, f.initial) {
			t.Fatalf("Resolve = %q, want the stored document", got)
		}
		f.clock.Advance(retryAfter)
	}
	f.clock.Advance(journalLife - 3*retryAfter - time.Second)
	f.resolve(t)
	if n := f.renewer.callCount(); n != 0 {
		t.Fatalf("a renewal ran past a journal %v old: %d call(s)", journalLife-time.Second, n)
	}
	if got := f.cache.Recovering(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Recovering = %v, want [%s]", got, credName)
	}

	f.clock.Advance(retryAfter)
	renewed := f.resolve(t)
	if bytes.Equal(renewed, f.initial) {
		t.Error("the stale journal still held off the renewal")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want one", n)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the stale journal is still in the store")
	}
	if len(f.cache.Recovering()) != 0 {
		t.Errorf("Recovering = %v after the renewal", f.cache.Recovering())
	}
}

// The journal is written before the provider is called. A store that
// refuses it gets no refresh, so no answer can be lost.
func TestARefreshIsNotSentWithoutItsJournal(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.failNext(1)
	if got := f.resolve(t); !bytes.Equal(got, f.initial) {
		t.Errorf("Resolve = %q, want the stored document", got)
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Fatalf("the provider was called %d time(s) with no journal in the store", n)
	}
	if got := f.cache.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Failing = %v, want [%s]", got, credName)
	}
	f.clock.Advance(retryAfter)
	if got := f.resolve(t); bytes.Equal(got, f.initial) {
		t.Error("the renewal was not tried again once the store took writes")
	}
	f.renewer.checkNoReuse(t)
}

// The journal carries the secret's provenance, with the expiry of the
// document it holds, like every other item brig writes.
func TestTheJournalCarriesProvenance(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started := f.renewer.started
	f.store.failWritesOf(credName, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.resolve(t)
	}()
	<-started
	if p := f.store.provenance(credJournal); p.From != importedFrom.From ||
		p.ExpiresAt != f.clock.Now().Add(5*time.Minute).UnixMilli() {
		t.Errorf("the renewing journal's provenance = %+v, want the secret's", p)
	}
	close(f.renewer.release)
	<-done
	if p := f.store.provenance(credJournal); p.From != importedFrom.From ||
		p.ExpiresAt != f.clock.Now().Add(8*time.Hour).UnixMilli() {
		t.Errorf("the renewed journal's provenance = %+v, want the secret's with the new expiry", p)
	}
}

// A secret deleted while its journal waits takes the journal with it.
func TestADeletedSecretTakesItsJournal(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.failWritesOf(credName, 1)
	f.resolve(t)
	if _, ok := f.storedJournal(t); !ok {
		t.Fatal("no journal while the write-back is pending")
	}
	if err := f.store.Delete(credName); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(retryAfter)
	if _, err := f.cache.Resolve(context.Background(), credName, testWorkload); !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("Resolve after the delete = %v, want ErrNotFound", err)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal, which holds the renewed document, outlived the secret")
	}
	if _, err := f.store.Read(credName); !errors.Is(err, secret.ErrNotFound) {
		t.Error("the deleted secret came back")
	}
}

// A journal create that lands half, as a keychain key item whose sealed
// item never did, reads back damaged. The refresh is reported failing, the
// half journal is cleared as unreadable, and the next renewal goes out. It
// must not block every renewal while reporting nothing.
func TestAHalfWrittenJournalIsReportedAndCleared(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.halfWriteOf(credJournal, 1)
	if got := f.resolve(t); !bytes.Equal(got, f.initial) {
		t.Fatalf("Resolve = %q, want the stored document", got)
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Fatalf("a refresh went out after its journal create failed: %d call(s)", n)
	}
	if got := f.cache.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Failing = %v, want [%s]", got, credName)
	}
	_, lists := f.store.counts()
	for range 20 {
		f.resolve(t)
	}
	if _, l := f.store.counts(); l != lists {
		t.Errorf("20 requests inside retryAfter listed the store %d times, want none", l-lists)
	}

	for range 3 {
		f.clock.Advance(retryAfter)
		f.resolve(t)
	}
	renewed := f.resolve(t)
	if bytes.Equal(renewed, f.initial) {
		t.Fatal("the half journal still blocks the renewal")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want one", n)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the half journal is still in the store")
	}
	if len(f.cache.Failing()) != 0 || len(f.cache.Recovering()) != 0 {
		t.Errorf("Failing %v, Recovering %v after the renewal", f.cache.Failing(), f.cache.Recovering())
	}
	f.renewer.checkNoReuse(t)
}

// The journal create runs on a context the caller cannot cancel. A caller
// that goes away mid-create would otherwise leave half a journal.
func TestAJournalCreateOutlivesItsCaller(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	var createErr error
	f.store.beforeWrite = func(wctx context.Context, name string) {
		if name != credJournal {
			return
		}
		once.Do(func() {
			cancel()
			createErr = wctx.Err()
		})
	}
	_, _ = f.cache.Resolve(ctx, credName, testWorkload)
	if createErr != nil {
		t.Errorf("the journal create saw its caller's cancel: %v", createErr)
	}
}

// A create that landed and reported a failure leaves a journal of this
// cache that it does not know of. No refresh followed it, so it is deleted
// and the renewal goes on, instead of failing with "exists" for good.
func TestAnOrphanJournalOfThisCacheIsDeleted(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.landThenFailOf(credJournal, 1)
	f.resolve(t)
	if _, ok := f.storedJournal(t); !ok {
		t.Fatal("the create did not land")
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Fatalf("a refresh went out after its journal create reported a failure")
	}
	for range 3 {
		f.clock.Advance(retryAfter)
		f.resolve(t)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want one", n)
	}
	if got := f.store.value(credName); bytes.Equal(got, f.initial) {
		t.Error("the renewal did not land")
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the orphan journal is still in the store")
	}
	f.renewer.checkNoReuse(t)
}

// A renewed journal of a stopped broker over a secret imported again since:
// the import is newer, so it stays and the journal goes.
func TestARenewedJournalLosesToAReimport(t *testing.T) {
	f := newCacheFixture(t, 2*time.Hour)
	plantJournal(t, f.store, journal{V: journalVersion, Phase: phaseRenewed,
		Owner: journalOwner{PID: 999999}, StartedAt: f.clock.Now().UnixMilli(),
		DocHash: docHash(f.initial), Doc: fakeDoc(5, f.clock.Now().Add(8*time.Hour))})
	fresh := fakeDoc(9, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	c := f.restart()
	if err := c.Recover(context.Background(), credName); err != nil {
		t.Fatal(err)
	}
	if got := f.store.value(credName); !bytes.Equal(got, fresh) {
		t.Errorf("the re-import was replaced by the journal's document: %q", got)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal is still in the store")
	}
	if got := resolveWith(t, c); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q, want the re-import", got)
	}
}

// A renewed journal whose document is not of the secret's kind is not
// promoted. The same bytes stored as the secret are refused, so a journal
// must not be a way around that.
func TestAJournalDocumentThatDoesNotParseIsNotPromoted(t *testing.T) {
	f := newCacheFixture(t, 2*time.Hour)
	planted := []byte("not-a-document")
	plantJournal(t, f.store, journal{V: journalVersion, Phase: phaseRenewed,
		Owner: journalOwner{PID: 999999}, StartedAt: f.clock.Now().UnixMilli(),
		DocHash: docHash(f.initial), Doc: planted})
	c := f.restart()
	if err := c.Recover(context.Background(), credName); err != nil {
		t.Fatal(err)
	}
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Errorf("the secret is %q after recovery, want it untouched", got)
	}
	if _, ok := f.storedJournal(t); ok {
		t.Error("the journal is still in the store")
	}
	if got := resolveWith(t, c); bytes.Equal(got, planted) {
		t.Error("the journal's document was served")
	}
}

// Two caches that both find no journal and both come due: the journal
// create admits one, and only that one calls the provider.
func TestOnlyOneOfTwoCachesCreatesTheJournal(t *testing.T) {
	f := newCacheFixture(t, 15*time.Minute)
	other := NewCache(f.broker, f.store, map[string]secret.Renewer{credName: f.renewer})
	other.now = f.clock.Now
	f.resolve(t)
	resolveWith(t, other)
	f.clock.Advance(6 * time.Minute)

	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started, release := f.renewer.started, f.renewer.release
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan []byte, 1)
	go func() { done <- f.resolve(t) }()
	<-started

	served := make(chan []byte, 1)
	go func() { served <- resolveWith(t, other) }()
	select {
	case got := <-served:
		if !bytes.Equal(got, f.initial) {
			t.Errorf("the second cache served %q, want the stored document", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second cache created a journal over the first one's, and waits on the provider")
	}
	if got := other.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("the second cache's Failing = %v, want [%s]", got, credName)
	}
	once.Do(func() { close(release) })
	<-done
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls from two caches, want one", n)
	}
	f.renewer.checkNoReuse(t)
}

// A journal that starts in the future, after a clock step back, is not
// young, so it does not hold the secret past its expiry.
func TestAJournalFromTheFutureIsStale(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	plantJournal(t, f.store, journal{V: journalVersion, Phase: phaseRenewing,
		Owner:     journalOwner{PID: 4242, Nonce: "live"},
		StartedAt: f.clock.Now().Add(30 * 24 * time.Hour).UnixMilli(), DocHash: docHash(f.initial)})
	f.cache.alive = func(journalOwner) bool { return true }
	if got := f.resolve(t); bytes.Equal(got, f.initial) {
		t.Error("a journal dated a month ahead held off the renewal")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("the provider got %d calls, want one", n)
	}
}

// A journal's owner is a process of this user with the recorded start
// time. A process of another user, or a pid reused by a later process, is
// not it.
func TestAJournalOwnerIsThisUsersProcessWithItsStartTime(t *testing.T) {
	self := journalOwner{PID: os.Getpid(), Start: selfStart()}
	if self.Start == "" {
		t.Fatal("no start time for this process")
	}
	if !ownerAlive(self) {
		t.Error("this process does not count as alive")
	}
	if ownerAlive(journalOwner{PID: os.Getpid(), Start: "Thu Jan 1 00:00:00 1970"}) {
		t.Error("a pid with another start time counts as the owner")
	}
	if ownerAlive(journalOwner{PID: 1 << 30}) {
		t.Error("a pid nothing runs as counts as alive")
	}
	if os.Getuid() != 0 && processAlive(1) {
		t.Error("pid 1, a process of another user, counts as alive")
	}
}

// Two brokers can run under different locales, and each reads the other's
// journal. The owner's start time reads the same under both.
func TestAJournalOwnerIsTheSameUnderAnotherLocale(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LC_TIME", "en_US.UTF-8")
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	start, err := processStart(child.Process.Pid)
	if err != nil {
		t.Skipf("no start time on this platform: %v", err)
	}
	owner := journalOwner{PID: child.Process.Pid, Start: start}
	t.Setenv("LC_ALL", "el_GR.UTF-8")
	t.Setenv("LC_TIME", "el_GR.UTF-8")
	if !ownerAlive(owner) {
		t.Error("under another locale, the journal's owner counts as dead")
	}
}

// A journal that cannot be read, because the store does not answer, is
// reported as recovering until a read succeeds.
func TestAJournalThatCannotBeReadIsReported(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	f.cache.storeTimeout = 20 * time.Millisecond
	hang := make(chan struct{})
	f.store.setHang(hang)
	_, _ = f.cache.Resolve(context.Background(), credName, testWorkload)
	if got := f.cache.Recovering(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Recovering = %v while the journal cannot be read, want [%s]", got, credName)
	}
	close(hang)
	f.clock.Advance(retryAfter)
	f.resolve(t)
	if got := f.cache.Recovering(); len(got) != 0 {
		t.Errorf("Recovering = %v after the read succeeded, want none", got)
	}
}

// A caller that goes away while the journal is read says nothing about the
// store, so the secret is not reported as recovering.
func TestACanceledCallerDoesNotReportTheJournalUnreadable(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	hang := make(chan struct{})
	f.store.setHang(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.cache.Resolve(ctx, credName, testWorkload); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Resolve = %v, want the caller's own deadline", err)
	}
	if got := f.cache.Recovering(); len(got) != 0 {
		t.Errorf("Recovering = %v after a caller went away, want none", got)
	}
	close(hang)
	f.resolve(t)
	if got := f.cache.Recovering(); len(got) != 0 {
		t.Errorf("Recovering = %v after a read that succeeded, want none", got)
	}
}
