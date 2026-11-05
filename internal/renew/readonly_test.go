package renew

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// readOnlyStore is a Store that holds the documents it was given and refuses
// every write with secret.ErrReadOnly, as a mounted Kubernetes Secret does.
// It is neither an Annotator nor a ContextStore, so nothing in the cache can
// reach a write path around Create, Update and Delete.
type readOnlyStore struct {
	mu     sync.Mutex
	values map[string][]byte
	writes int
}

var _ secret.Store = (*readOnlyStore)(nil)

func newReadOnlyStore(values map[string][]byte) *readOnlyStore {
	return &readOnlyStore{values: values}
}

func (s *readOnlyStore) Kind() string { return "files" }

func (s *readOnlyStore) Read(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *readOnlyStore) refuse() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	return secret.ErrReadOnly
}

func (s *readOnlyStore) Create(string, []byte) error { return s.refuse() }
func (s *readOnlyStore) Update(string, []byte) error { return s.refuse() }
func (s *readOnlyStore) Delete(string) error         { return s.refuse() }

func (s *readOnlyStore) List() ([]secret.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []secret.Secret
	for name := range s.values {
		out = append(out, secret.Secret{Name: name})
	}
	return out, nil
}

// rotate stands in for the kubelet replacing a mounted file.
func (s *readOnlyStore) rotate(name string, doc []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = doc
}

func (s *readOnlyStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// readOnlyFixture is a cache over a readOnlyStore with one document due soon.
type readOnlyFixture struct {
	clock   *fakeClock
	store   *readOnlyStore
	renewer *fakeRenewer
	cache   *Cache
	initial []byte

	mu   sync.Mutex
	logs []string
}

func newReadOnlyFixture(t *testing.T, expiresIn time.Duration) *readOnlyFixture {
	t.Helper()
	clock := newFakeClock()
	initial := fakeDoc(0, clock.Now().Add(expiresIn))
	store := newReadOnlyStore(map[string][]byte{credName: initial})
	renewer := newFakeRenewer(clock)
	cache := NewCache(secret.StoreBroker(store), store, map[string]secret.Renewer{credName: renewer})
	cache.now = clock.Now
	f := &readOnlyFixture{clock: clock, store: store, renewer: renewer, cache: cache, initial: initial}
	cache.Logf = func(format string, args ...any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
	}
	return f
}

func (f *readOnlyFixture) resolve(t *testing.T) []byte {
	t.Helper()
	doc, err := f.cache.Resolve(context.Background(), credName, testWorkload)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return doc
}

func (f *readOnlyFixture) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

func TestTheReadOnlyDoubleIsNeitherAnAnnotatorNorAContextStore(t *testing.T) {
	var s secret.Store = newReadOnlyStore(nil)
	if _, ok := s.(secret.Annotator); ok {
		t.Fatal("the double is an Annotator, so a write could bypass Create and Update")
	}
	if _, ok := s.(secret.ContextStore); ok {
		t.Fatal("the double is a ContextStore, so WithContext could hide it")
	}
}

func TestARenewalOnAReadOnlyStoreIsServedFromMemory(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	got := f.resolve(t)
	if string(got) == string(f.initial) {
		t.Fatalf("Resolve served the stored document %q, which is due for renewal", got)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Fatalf("%d renewals, want 1", n)
	}
	stored, err := f.store.Read(credName)
	if err != nil || string(stored) != string(f.initial) {
		t.Fatalf("the store holds %q, %v; want the original", stored, err)
	}
	if p := f.cache.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %q, want none: no write is waiting", p)
	}
	if fl := f.cache.Failing(); len(fl) != 0 {
		t.Fatalf("Failing = %q, want none", fl)
	}
	if again := f.resolve(t); string(again) != string(got) {
		t.Fatalf("the second Resolve served %q, want the held %q", again, got)
	}
	if n := f.renewer.callCount(); n != 1 {
		t.Fatalf("the held document was renewed again: %d renewals", n)
	}
	f.renewer.checkNoReuse(t)
}

func TestAReadOnlyStoreIsLoggedOncePerSecret(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	f.resolve(t)
	// Move past the held document's own renewal point, then renew again.
	f.clock.Advance(7*time.Hour + 55*time.Minute)
	f.resolve(t)
	if n := f.renewer.callCount(); n != 2 {
		t.Fatalf("%d renewals, want 2: a held document is renewed when due", n)
	}
	if logs := f.logged(); len(logs) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(logs), logs)
	}
	f.renewer.checkNoReuse(t)
}

func TestAReadOnlyRenewalIsNotBlockedByTheLastOne(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	first := f.resolve(t)
	stale := first
	got, err := f.cache.ForceRenew(context.Background(), credName, testWorkload, stale)
	if err != nil {
		t.Fatalf("ForceRenew on a read-only store: %v", err)
	}
	if string(got) == string(first) {
		t.Fatalf("ForceRenew returned the document it was told was refused: %q", got)
	}
	if p := f.cache.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %q, want none", p)
	}
	f.renewer.checkNoReuse(t)
}

func TestARotatedFileReplacesTheHeldDocument(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	held := f.resolve(t)
	rotated := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.store.rotate(credName, rotated)
	f.clock.Advance(recheck + time.Second)
	got := f.resolve(t)
	if string(got) != string(rotated) {
		t.Fatalf("served %q after the file rotated to %q (held %q)", got, rotated, held)
	}
	if p := f.cache.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %q, want none", p)
	}
}

func TestAProviderFailureOnAReadOnlyStoreRecordsNoJournalStep(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	f.renewer.setFail(errProvider)
	got := f.resolve(t)
	if string(got) != string(f.initial) {
		t.Fatalf("served %q while the renewal failed, want the stored %q", got, f.initial)
	}
	if p := f.cache.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %q after a failed renewal, want none: there is no journal to clear", p)
	}
	if fl := f.cache.Failing(); len(fl) != 1 || fl[0] != credName {
		t.Fatalf("Failing = %q, want [%s]", fl, credName)
	}
	// After the backoff the renewal is tried again and succeeds.
	f.renewer.setFail(nil)
	f.clock.Advance(retryAfter + time.Second)
	if got := f.resolve(t); string(got) == string(f.initial) {
		t.Fatalf("served the stored document after the backoff: %q", got)
	}
}

func TestAFailedReadOnlyRenewalLeavesNoJournalAfterARotation(t *testing.T) {
	f := newReadOnlyFixture(t, 5*time.Minute)
	f.renewer.setFail(errProvider)
	f.resolve(t)
	// The first attempt probes the store and learns it is read-only.
	probed := f.store.writeCount()
	// The kubelet replaces the file, which drops the held state. A journal
	// step left behind by the failure would now read as pending and be
	// flushed into the read-only store.
	rotated := fakeDoc(100, f.clock.Now().Add(8*time.Hour))
	f.store.rotate(credName, rotated)
	f.clock.Advance(recheck + time.Second)
	f.renewer.setFail(nil)
	if got := f.resolve(t); string(got) != string(rotated) {
		t.Fatalf("served %q after the rotation, want %q", got, rotated)
	}
	if p := f.cache.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %q after a failure and a rotation, want none", p)
	}
	if n := f.store.writeCount(); n != probed {
		t.Fatalf("%d writes reached the read-only store after the rotation, want %d", n, probed)
	}
	f.clock.Advance(7*time.Hour + 55*time.Minute)
	f.resolve(t)
	if n := f.renewer.callCount(); n < 1 {
		t.Fatalf("the credential was never renewed after the rotation")
	}
}
