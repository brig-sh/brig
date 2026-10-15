package renew

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

var errProvider = errors.New("the provider could not be reached")

// fakeClock is a clock the tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeRenewer is a Renewer and Expirer over documents of the form
// "<generation>@<expiry in unix seconds>".
//
// It also plays the provider's rotation: a document it has renewed once is
// dead, and renewing it again counts as a reuse.
type fakeRenewer struct {
	clock *fakeClock
	// life is how long a renewed document lasts.
	life time.Duration

	mu      sync.Mutex
	calls   int
	gen     int
	fail    error
	renewed map[string]bool
	reused  int
	// started, when set, is closed by the first Renew to begin.
	started chan struct{}
	// release, when set, holds every Renew until it is closed.
	release chan struct{}
	// ctxErr is what the renewal context said when Renew returned.
	ctxErr error
}

func newFakeRenewer(clock *fakeClock) *fakeRenewer {
	return &fakeRenewer{clock: clock, life: 8 * time.Hour, renewed: map[string]bool{}}
}

func fakeDoc(gen int, expires time.Time) []byte {
	return []byte(fmt.Sprintf("%d@%d", gen, expires.Unix()))
}

func (f *fakeRenewer) Renew(ctx context.Context, current []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	release := f.release
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErr = ctx.Err()
	if f.renewed[string(current)] {
		f.reused++
		return nil, fmt.Errorf("reused: %w", secret.ErrSpent)
	}
	if f.fail != nil {
		if errors.Is(f.fail, secret.ErrSpent) {
			f.renewed[string(current)] = true
		}
		return nil, f.fail
	}
	f.renewed[string(current)] = true
	f.gen++
	return fakeDoc(f.gen, f.clock.Now().Add(f.life)), nil
}

func (f *fakeRenewer) Expiry(doc []byte) (time.Time, error) {
	_, at, ok := strings.Cut(string(doc), "@")
	if !ok {
		return time.Time{}, errors.New("not a fake document")
	}
	sec, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

func (f *fakeRenewer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRenewer) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// checkNoReuse fails the test if any document was renewed twice. The
// provider rotates the refresh token single-use, so a second renewal of one
// document is a request with a dead token.
func (f *fakeRenewer) checkNoReuse(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reused != 0 {
		t.Errorf("a superseded document was renewed again %d time(s)", f.reused)
	}
}

// countingBroker counts the reads that reach the store.
type countingBroker struct {
	secret.Broker
	mu    sync.Mutex
	reads int
}

func (b *countingBroker) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	b.mu.Lock()
	b.reads++
	b.mu.Unlock()
	return b.Broker.Resolve(ctx, name, w)
}

func (b *countingBroker) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

const credName = "claude-credentials"

var (
	testWorkload = secret.Workload{Sandbox: "brig-7f3a", Profile: "claude-code", Session: "refactor"}
	importedFrom = secret.Provenance{V: secret.ProvenanceVersion, From: "keychain:Claude Code-credentials", ExpiresAt: 1}
)

// cacheFixture is a store holding one imported document, a renewer for it
// and a cache over both.
type cacheFixture struct {
	clock   *fakeClock
	store   *memStore
	broker  *countingBroker
	renewer *fakeRenewer
	cache   *Cache
	initial []byte
}

func newCacheFixture(t *testing.T, expiresIn time.Duration) *cacheFixture {
	t.Helper()
	clock := newFakeClock()
	store := newMemStore()
	initial := fakeDoc(0, clock.Now().Add(expiresIn))
	if err := store.Write(credName, initial, importedFrom, false); err != nil {
		t.Fatal(err)
	}
	renewer := newFakeRenewer(clock)
	broker := &countingBroker{Broker: secret.StoreBroker(store)}
	cache := NewCache(broker, store, map[string]secret.Renewer{credName: renewer})
	cache.now = clock.Now
	return &cacheFixture{clock, store, broker, renewer, cache, initial}
}

func (f *cacheFixture) resolve(t *testing.T) []byte {
	t.Helper()
	doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return doc
}

// reimport stands in for `brig secret import` replacing the stored document.
func (f *cacheFixture) reimport(t *testing.T, doc []byte) {
	t.Helper()
	if err := f.store.Write(credName, doc, importedFrom, true); err != nil {
		t.Fatal(err)
	}
}

func TestWriteBackKeepsProvenance(t *testing.T) {
	s := newMemStore()
	if err := s.Write(credName, []byte("old"), importedFrom, false); err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC)
	if err := writeBack(s, credName, []byte("new"), expires); err != nil {
		t.Fatal(err)
	}
	if got := s.value(credName); !bytes.Equal(got, []byte("new")) {
		t.Errorf("the store holds %q, want the renewed value", got)
	}
	want := secret.Provenance{V: secret.ProvenanceVersion, From: importedFrom.From, ExpiresAt: expires.UnixMilli()}
	if got := s.provenance(credName); got != want {
		t.Errorf("provenance after write-back = %+v, want %+v", got, want)
	}
}

func TestWriteBackLeavesAHandCreatedSecretAbsent(t *testing.T) {
	s := newMemStore()
	if err := s.Create(credName, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := writeBack(s, credName, []byte("new"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := s.provenance(credName); !got.IsZero() {
		t.Errorf("a hand-created secret gained provenance %+v", got)
	}
}

// A write-back for a secret deleted under the cache must not bring it back.
func TestWriteBackNeverCreates(t *testing.T) {
	s := newMemStore()
	if err := writeBack(s, credName, []byte("new"), time.Time{}); !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("writeBack of an absent secret = %v, want ErrNotFound", err)
	}
	if _, err := s.Read(credName); !errors.Is(err, secret.ErrNotFound) {
		t.Error("writeBack created a secret")
	}
}

func TestCacheHoldsAFreshDocument(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	for range 3 {
		if got := f.resolve(t); !bytes.Equal(got, f.initial) {
			t.Fatalf("Resolve = %q, want the stored document", got)
		}
	}
	if f.renewer.callCount() != 0 {
		t.Errorf("a document an hour from expiry was renewed %d time(s)", f.renewer.callCount())
	}
	if n := f.broker.readCount(); n != 1 {
		t.Errorf("the store was read %d times inside recheck, want once", n)
	}
	f.clock.Advance(recheck)
	f.resolve(t)
	if n := f.broker.readCount(); n != 2 {
		t.Errorf("the store was read %d times after recheck, want twice", n)
	}
}

func TestCacheRenewsAheadOfExpiry(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	first := f.resolve(t)
	second := f.resolve(t)
	if n := f.renewer.callCount(); n != 1 {
		t.Fatalf("renewed %d times, want once", n)
	}
	if bytes.Equal(first, f.initial) || !bytes.Equal(first, second) {
		t.Errorf("Resolve served %q then %q, want the renewed document twice", first, second)
	}
	if got := f.store.value(credName); !bytes.Equal(got, first) {
		t.Errorf("the store holds %q, want the renewed document", got)
	}
	prov := f.store.provenance(credName)
	if prov.From != importedFrom.From {
		t.Errorf("write-back changed From to %q", prov.From)
	}
	if want := f.clock.Now().Add(8 * time.Hour).Unix(); prov.ExpiresAt/1000 != want {
		t.Errorf("write-back set ExpiresAt %d, want %d s", prov.ExpiresAt, want)
	}
	f.renewer.checkNoReuse(t)
}

func TestConcurrentCallersShareOneRenewal(t *testing.T) {
	f := newCacheFixture(t, -time.Minute)
	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started := f.renewer.started

	const callers = 8
	docs := make([][]byte, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs[i], errs[i] = f.cache.Resolve(context.Background(), credName, testWorkload)
		}()
	}
	<-started
	close(f.renewer.release)
	wg.Wait()

	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("%d concurrent callers renewed %d times, want once", callers, n)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !bytes.Equal(docs[i], docs[0]) || bytes.Equal(docs[i], f.initial) {
			t.Errorf("caller %d got %q, want the one renewed document", i, docs[i])
		}
	}
	f.renewer.checkNoReuse(t)
}

func TestCacheKeepsTheRenewedDocumentWhenWriteBackFails(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.failNext(1)

	renewed := f.resolve(t)
	if bytes.Equal(renewed, f.initial) {
		t.Fatal("Resolve served the old document after a renewal")
	}
	if got := f.cache.Pending(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Pending = %v after a failed write-back, want [%s]", got, credName)
	}
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Fatalf("the store changed after a failed write: %q", got)
	}

	f.clock.Advance(retryAfter)
	if got := f.resolve(t); !bytes.Equal(got, renewed) {
		t.Errorf("the next Resolve served %q, want the renewed document", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, renewed) {
		t.Errorf("the store holds %q after the retry, want the renewed document", got)
	}
	if got := f.cache.Pending(); len(got) != 0 {
		t.Errorf("Pending = %v after a successful retry, want none", got)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("renewed %d times, want once", n)
	}
}

// Each retry of a write-back lists the store, which is a keychain dump on
// macOS. A store that keeps refusing must not see one per request.
func TestWriteBackRetriesAreSpaced(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.failNext(100)
	renewed := f.resolve(t)
	writes, lists := f.store.counts()
	for range 5 {
		if got := f.resolve(t); !bytes.Equal(got, renewed) {
			t.Fatalf("Resolve = %q, want the renewed document", got)
		}
	}
	if w, l := f.store.counts(); w != writes || l != lists {
		t.Errorf("five requests inside retryAfter made %d writes and %d lists, want none",
			w-writes, l-lists)
	}
	f.clock.Advance(retryAfter)
	f.resolve(t)
	if w, _ := f.store.counts(); w != writes+1 {
		t.Errorf("a request after retryAfter made %d writes, want 1", w-writes)
	}
}

func TestCacheServesTheCurrentDocumentWhileRenewalFails(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.renewer.setFail(errProvider)

	for range 2 {
		if got := f.resolve(t); !bytes.Equal(got, f.initial) {
			t.Fatalf("Resolve = %q, want the current document", got)
		}
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("renewed %d times inside retryAfter, want once", n)
	}
	if got := f.cache.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Failing = %v, want [%s]", got, credName)
	}
	f.clock.Advance(retryAfter)
	f.resolve(t)
	if n := f.renewer.callCount(); n != 2 {
		t.Errorf("renewed %d times after retryAfter, want twice", n)
	}
	f.renewer.setFail(nil)
	f.clock.Advance(retryAfter)
	f.resolve(t)
	if got := f.cache.Failing(); len(got) != 0 {
		t.Errorf("Failing = %v after a successful renewal, want none", got)
	}
	f.renewer.checkNoReuse(t)
}

// A renewal that failed after it reached the provider may have rotated the
// refresh token. Sending that document again every retryAfter would be a
// stream of requests with a dead token.
func TestCacheDoesNotRenewASpentDocument(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.renewer.setFail(fmt.Errorf("refused after rotating: %w", secret.ErrSpent))

	if got := f.resolve(t); !bytes.Equal(got, f.initial) {
		t.Fatal("Resolve did not serve the current document, which has not expired")
	}
	for range 3 {
		f.clock.Advance(retryAfter)
		f.resolve(t)
	}
	stale := f.initial
	if _, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale); err == nil {
		t.Error("ForceRenew renewed a spent document")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("a spent document was renewed %d times, want once", n)
	}
	if got := f.cache.Failing(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Failing = %v, want [%s]", got, credName)
	}

	// A fresh import is the way out.
	fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	f.clock.Advance(recheck)
	if got := f.resolve(t); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q after a re-import, want the imported document", got)
	}
	if got := f.cache.Failing(); len(got) != 0 {
		t.Errorf("Failing = %v after a re-import, want none", got)
	}
	f.renewer.checkNoReuse(t)
}

func TestCacheRefusesAnExpiredDocumentItCouldNotRenew(t *testing.T) {
	f := newCacheFixture(t, -time.Minute)
	f.renewer.setFail(errProvider)
	doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
	if !errors.Is(err, errProvider) || doc != nil {
		t.Fatalf("Resolve = %q, %v; want no document and the provider's error", doc, err)
	}
	if got := f.store.value(credName); !bytes.Equal(got, f.initial) {
		t.Errorf("a failed renewal changed the store to %q", got)
	}
	if got := f.cache.Pending(); len(got) != 0 {
		t.Errorf("Pending = %v after a failed renewal", got)
	}
}

func TestCacheStopsServingADeletedSecret(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	f.resolve(t)
	if err := f.store.Delete(credName); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(recheck)
	if doc, err := f.cache.Resolve(context.Background(), credName, testWorkload); !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("Resolve after a delete = %q, %v; want ErrNotFound", doc, err)
	}
}

// The store is read before a renewal, so a deleted secret is never renewed.
func TestCacheNoticesADeleteBeforeRenewing(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	f.resolve(t)
	if err := f.store.Delete(credName); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(55 * time.Minute)
	if _, err := f.cache.Resolve(context.Background(), credName, testWorkload); !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("Resolve after a delete = %v, want ErrNotFound", err)
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Errorf("a deleted secret was renewed %d time(s)", n)
	}
	if _, err := f.store.Read(credName); !errors.Is(err, secret.ErrNotFound) {
		t.Error("the deleted secret came back")
	}
}

func TestCacheServesAReimport(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	f.resolve(t)
	fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	f.clock.Advance(recheck)
	if got := f.resolve(t); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q after a re-import, want the imported document", got)
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Errorf("renewed %d time(s), want none", n)
	}
}

// A write-back pending from before a re-import holds an older pair. Writing
// it would replace the user's fresh import with it.
func TestReimportIsNeverOverwrittenByAStaleWriteBack(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.store.failNext(1)
	renewed := f.resolve(t)
	if !slices.Equal(f.cache.Pending(), []string{credName}) {
		t.Fatal("the failed write-back is not pending")
	}

	fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	f.clock.Advance(retryAfter)
	if got := f.resolve(t); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q, want the re-imported document", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, fresh) || bytes.Equal(got, renewed) {
		t.Errorf("the store holds %q, want the re-import untouched", got)
	}
	if got := f.cache.Pending(); len(got) != 0 {
		t.Errorf("Pending = %v after adopting a re-import, want none", got)
	}
}

// A store call that never returns would hold the secret's turn, and every
// caller behind it.
func TestAHungStoreDoesNotHoldTheTurn(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	f.cache.storeTimeout = 20 * time.Millisecond
	hang := make(chan struct{})
	f.store.setHang(hang)

	done := make(chan error, 1)
	go func() {
		_, err := f.cache.Resolve(context.Background(), credName, testWorkload)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Resolve on a hung store = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve is still waiting on a hung store")
	}
	close(hang)
	if got := f.resolve(t); !bytes.Equal(got, f.initial) {
		t.Errorf("Resolve after the store recovered = %q, want the stored document", got)
	}
}

func TestForceRenewRenewsOnce(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	stale := f.resolve(t)

	var wg sync.WaitGroup
	docs := make([][]byte, 4)
	for i := range docs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale)
			if err != nil {
				t.Errorf("ForceRenew: %v", err)
			}
			docs[i] = doc
		}()
	}
	wg.Wait()
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("four callers refused with one document renewed %d times, want once", n)
	}
	for i := range docs {
		if bytes.Equal(docs[i], stale) || !bytes.Equal(docs[i], docs[0]) {
			t.Errorf("caller %d got %q, want the renewed document", i, docs[i])
		}
	}
	if got := f.store.value(credName); !bytes.Equal(got, docs[0]) {
		t.Errorf("the store holds %q, want the renewed document", got)
	}
	f.renewer.checkNoReuse(t)
}

func TestForceRenewRefusesAStaticSecret(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	if _, err := f.cache.ForceRenew(context.Background(), "gh-token", testWorkload, nil); err == nil {
		t.Error("ForceRenew renewed a secret with no lifecycle")
	}
}

// The provider rotates the refresh token on every renewal. A renewal whose
// caller gave up must still complete, or the only valid pair is lost.
func TestRenewalOutlivesItsCaller(t *testing.T) {
	f := newCacheFixture(t, -time.Minute)
	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started := f.renewer.started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.cache.Resolve(ctx, credName, testWorkload)
		done <- err
	}()
	<-started
	cancel()
	close(f.renewer.release)
	<-done

	if f.renewer.ctxErr != nil {
		t.Errorf("the renewal saw its context end: %v", f.renewer.ctxErr)
	}
	renewed := f.resolve(t)
	if bytes.Equal(renewed, f.initial) {
		t.Error("the renewed document was dropped with its caller")
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("renewed %d times, want once", n)
	}
	if got := f.store.value(credName); !bytes.Equal(got, renewed) {
		t.Error("the renewed document was not written back")
	}
}

func TestCacheReadsAStaticSecretThrough(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	if err := f.store.Create("gh-token", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	f.resolveStatic(t, "v1")
	if err := f.store.Update("gh-token", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	f.resolveStatic(t, "v2")
}

func (f *cacheFixture) resolveStatic(t *testing.T, want string) {
	t.Helper()
	got, err := f.cache.Resolve(context.Background(), "gh-token", testWorkload)
	if err != nil || string(got) != want {
		t.Errorf("Resolve(gh-token) = %q, %v; want %q", got, err, want)
	}
}

// A caller whose request ends during the renewal must not turn the
// write-back's store read into "nobody else wrote". The user re-imported
// meanwhile, and that import is the newer document.
func TestACanceledCallerNeverOverwritesAReimport(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.renewer.started = make(chan struct{})
	f.renewer.release = make(chan struct{})
	started := f.renewer.started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.cache.Resolve(ctx, credName, testWorkload)
		done <- err
	}()
	<-started
	fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	cancel()
	close(f.renewer.release)
	<-done

	if got := f.store.value(credName); !bytes.Equal(got, fresh) {
		t.Errorf("the store holds %q after a re-import during the renewal, want the re-import %q", got, fresh)
	}
	if got := f.resolve(t); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q, want the re-imported document", got)
	}
	if got := f.cache.Pending(); len(got) != 0 {
		t.Errorf("Pending = %v after adopting a re-import, want none", got)
	}
}

// failingBroker fails the reads it is armed for, like a keychain that is
// briefly unavailable.
type failingBroker struct {
	secret.Broker
	mu    sync.Mutex
	fails int
}

func (b *failingBroker) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	b.mu.Lock()
	fail := b.fails > 0
	if fail {
		b.fails--
	}
	b.mu.Unlock()
	if fail {
		return nil, errors.New("the store could not be read")
	}
	return b.Broker.Resolve(ctx, name, w)
}

func (b *failingBroker) failNext(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = n
}

// A store read that fails before a pending write-back says nothing about a
// re-import. The write waits for a read that succeeds.
func TestAFailedReadHoldsThePendingWriteBack(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	broker := &failingBroker{Broker: secret.StoreBroker(f.store)}
	f.cache.broker = broker
	f.store.failNext(1)
	renewed := f.resolve(t)
	if !slices.Equal(f.cache.Pending(), []string{credName}) {
		t.Fatal("the failed write-back is not pending")
	}

	fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.reimport(t, fresh)
	f.clock.Advance(retryAfter)
	broker.failNext(1)
	if got := f.resolve(t); !bytes.Equal(got, renewed) {
		t.Errorf("Resolve = %q, want the renewed document while the store cannot be read", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, fresh) {
		t.Fatalf("the store holds %q after a failed read, want the re-import %q", got, fresh)
	}
	if got := f.cache.Pending(); !slices.Equal(got, []string{credName}) {
		t.Errorf("Pending = %v after a failed read, want [%s]", got, credName)
	}

	f.clock.Advance(retryAfter)
	if got := f.resolve(t); !bytes.Equal(got, fresh) {
		t.Errorf("Resolve = %q once the store reads again, want the re-imported document", got)
	}
	if got := f.store.value(credName); !bytes.Equal(got, fresh) {
		t.Errorf("the store holds %q, want the re-import untouched", got)
	}
}

// A provider that issues a token shorter-lived than renewAhead must not see
// one refresh per request. The cache renews it halfway through its life.
func TestAShortLifetimeIsRenewedHalfwayThrough(t *testing.T) {
	f := newCacheFixture(t, 5*time.Minute)
	f.renewer.life = 5 * time.Minute
	first := f.resolve(t)
	for range 4 {
		if got := f.resolve(t); !bytes.Equal(got, first) {
			t.Fatalf("Resolve = %q, want the renewed document %q", got, first)
		}
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Fatalf("five requests at one instant renewed %d times, want once", n)
	}
	f.clock.Advance(2 * time.Minute)
	f.resolve(t)
	if n := f.renewer.callCount(); n != 1 {
		t.Errorf("renewed %d times before half the life, want once", n)
	}
	f.clock.Advance(time.Minute)
	if got := f.resolve(t); bytes.Equal(got, first) {
		t.Error("a document past half its life was not renewed")
	}
	if n := f.renewer.callCount(); n != 2 {
		t.Errorf("renewed %d times past half the life, want twice", n)
	}
	f.renewer.checkNoReuse(t)
}

// A secret deleted, or re-imported in a form the renewer cannot read, while
// a renewal runs is found by the read before the write-back. Resolve returns
// that error, and never an empty document with no error.
func TestAChangeDuringARenewalIsAnError(t *testing.T) {
	for name, change := range map[string]func(*cacheFixture) error{
		"delete": func(f *cacheFixture) error { return f.store.Delete(credName) },
		"unreadable re-import": func(f *cacheFixture) error {
			return f.store.Write(credName, []byte("not a document"), importedFrom, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCacheFixture(t, 5*time.Minute)
			started, release := make(chan struct{}), make(chan struct{})
			f.renewer.started, f.renewer.release = started, release
			type result struct {
				doc []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
				done <- result{doc, err}
			}()
			<-started
			if err := change(f); err != nil {
				t.Fatal(err)
			}
			close(release)
			r := <-done
			if r.err == nil {
				t.Fatalf("Resolve = %q with no error after the change during the renewal", r.doc)
			}
			if r.doc != nil {
				t.Errorf("Resolve returned %q beside its error", r.doc)
			}
		})
	}
}

// A store read that fails before a renewal cannot tell a re-import from
// nothing changed. A renewal then would spend the refresh token on a pair
// that the next good read drops for the re-import, so none is made until a
// read succeeds.
func TestAFailedReadHoldsTheRenewal(t *testing.T) {
	for name, c := range map[string]struct {
		expiresIn, wait time.Duration
		fails           int
	}{
		// The document is due and the recheck is due too. The recheck's
		// read fails, and so does the read made for the renewal.
		"at the recheck": {time.Hour, 55 * time.Minute, 2},
		// The document is due before the recheck, so the read is the one
		// made for the renewal.
		"before the renewal": {14 * time.Minute, 4*time.Minute + 30*time.Second, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCacheFixture(t, c.expiresIn)
			broker := &failingBroker{Broker: secret.StoreBroker(f.store)}
			f.cache.broker = broker
			f.resolve(t)
			fresh := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
			f.reimport(t, fresh)
			f.clock.Advance(c.wait)
			broker.failNext(c.fails)
			if got := f.resolve(t); !bytes.Equal(got, f.initial) {
				t.Errorf("Resolve = %q while the store cannot be read, want the current document", got)
			}
			if n := f.renewer.callCount(); n != 0 {
				t.Fatalf("renewed %d time(s) after a failed store read", n)
			}
			if got := f.cache.Failing(); !slices.Equal(got, []string{credName}) {
				t.Errorf("Failing = %v, want [%s]", got, credName)
			}
			f.clock.Advance(retryAfter)
			if got := f.resolve(t); !bytes.Equal(got, fresh) {
				t.Errorf("Resolve = %q once the store reads again, want the re-import", got)
			}
			if n := f.renewer.callCount(); n != 0 {
				t.Errorf("renewed %d time(s), want none: the re-import needs no renewal", n)
			}
		})
	}
}

// ForceRenew reads the store first too, and refuses to renew when that read
// fails.
func TestForceRenewHoldsTheRenewalAfterAFailedRead(t *testing.T) {
	f := newCacheFixture(t, time.Hour)
	broker := &failingBroker{Broker: secret.StoreBroker(f.store)}
	f.cache.broker = broker
	stale := f.resolve(t)
	f.reimport(t, fakeDoc(100, f.clock.Now().Add(8*time.Hour)))
	broker.failNext(1)
	if doc, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale); err == nil {
		t.Errorf("ForceRenew = %q with no error after a failed store read", doc)
	}
	if n := f.renewer.callCount(); n != 0 {
		t.Errorf("ForceRenew renewed %d time(s) after a failed store read", n)
	}
	if got := f.store.value(credName); bytes.Equal(got, stale) {
		t.Error("the re-import was overwritten")
	}
}

// cuttingBroker cuts its next read short the way a caller does: it cancels
// the caller's context, when it was given the cancel, and waits until that
// context is done, then fails with the context's error.
type cuttingBroker struct {
	secret.Broker
	mu     sync.Mutex
	armed  bool
	cancel context.CancelFunc
}

func (b *cuttingBroker) Resolve(ctx context.Context, name string, w secret.Workload) ([]byte, error) {
	b.mu.Lock()
	armed, cancel := b.armed, b.cancel
	b.armed, b.cancel = false, nil
	b.mu.Unlock()
	if !armed {
		return b.Broker.Resolve(ctx, name, w)
	}
	if cancel != nil {
		cancel()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *cuttingBroker) cutNext(cancel context.CancelFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed, b.cancel = true, cancel
}

// cutCaller makes one Resolve whose context ends during the read before a
// renewal: canceled, or past a deadline far shorter than storeTimeout.
func cutCaller(t *testing.T, f *cacheFixture, b *cuttingBroker, deadline bool) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if deadline {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	}
	defer cancel()
	if deadline {
		b.cutNext(nil)
	} else {
		b.cutNext(cancel)
	}
	_, err := f.cache.Resolve(ctx, credName, testWorkload)
	return err
}

// A caller that goes away during the read before a renewal says nothing
// about the store. It gets its own error, and the next caller renews.
func TestACanceledCallerDoesNotHoldTheRenewal(t *testing.T) {
	for name, deadline := range map[string]bool{"canceled": false, "past its deadline": true} {
		t.Run(name, func(t *testing.T) {
			f := newCacheFixture(t, 14*time.Minute)
			broker := &cuttingBroker{Broker: secret.StoreBroker(f.store)}
			f.cache.broker = broker
			f.resolve(t)
			f.clock.Advance(4*time.Minute + 30*time.Second)
			err := cutCaller(t, f, broker, deadline)
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Resolve = %v, want the caller's own context error", err)
			}
			if got := f.cache.Failing(); len(got) != 0 {
				t.Errorf("Failing = %v after a caller went away, want none", got)
			}
			if got := f.resolve(t); bytes.Equal(got, f.initial) {
				t.Error("the next caller was served the old document, want a renewed one")
			}
			if n := f.renewer.callCount(); n != 1 {
				t.Errorf("renewed %d time(s), want 1: the store was healthy throughout", n)
			}
		})
	}
}

// ForceRenew's own read succeeds, so neither a caller that went away nor an
// earlier failed read holds its renewal, and it does not report a store read
// failure.
func TestForceRenewIsNotHeldAfterItsOwnReadSucceeds(t *testing.T) {
	for name, hold := range map[string]func(*testing.T, *cacheFixture){
		"after a canceled caller": func(t *testing.T, f *cacheFixture) {
			broker := &cuttingBroker{Broker: secret.StoreBroker(f.store)}
			f.cache.broker = broker
			_ = cutCaller(t, f, broker, false)
		},
		"after a failed read": func(t *testing.T, f *cacheFixture) {
			broker := &failingBroker{Broker: secret.StoreBroker(f.store)}
			f.cache.broker = broker
			broker.failNext(1)
			f.resolve(t)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCacheFixture(t, 14*time.Minute)
			stale := f.resolve(t)
			f.clock.Advance(4*time.Minute + 30*time.Second)
			hold(t, f)
			doc, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale)
			if err != nil {
				t.Fatalf("ForceRenew = %v after its own read succeeded", err)
			}
			if bytes.Equal(doc, stale) {
				t.Error("ForceRenew returned the refused document")
			}
			if n := f.renewer.callCount(); n != 1 {
				t.Errorf("renewed %d time(s), want 1", n)
			}
		})
	}
}
