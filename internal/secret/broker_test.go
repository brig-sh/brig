package secret

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// mapStore is the smallest Store, for the seams that only read. It records
// the context it was bound to.
type mapStore struct {
	values map[string][]byte
	ctx    context.Context
}

func (m mapStore) Kind() string                { return "map" }
func (m mapStore) Create(string, []byte) error { return errors.New("read-only") }
func (m mapStore) Update(string, []byte) error { return errors.New("read-only") }
func (m mapStore) Delete(string) error         { return errors.New("read-only") }
func (m mapStore) List() ([]Secret, error)     { return nil, nil }
func (m mapStore) WithContext(ctx context.Context) Store {
	m.ctx = ctx
	return m
}

func (m mapStore) Read(name string) ([]byte, error) {
	if m.ctx != nil {
		if err := m.ctx.Err(); err != nil {
			return nil, err
		}
	}
	v, ok := m.values[name]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

func TestStoreBrokerReadsTheStore(t *testing.T) {
	b := StoreBroker(mapStore{values: map[string][]byte{"gh-token": []byte("v1")}})
	w := Workload{Sandbox: "brig-7f3a", Profile: "claude-code"}
	got, err := b.Resolve(context.Background(), "gh-token", w)
	if err != nil || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("Resolve = %q, %v; want the stored value", got, err)
	}
	if _, err := b.Resolve(context.Background(), "absent", w); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve of an absent secret = %v, want ErrNotFound", err)
	}
}

// The broker's deadline reaches the backend. A store call that outlives it
// would hold every request for the same secret behind it.
func TestStoreBrokerBindsTheReadToItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := StoreBroker(mapStore{values: map[string][]byte{"gh-token": []byte("v1")}})
	if _, err := b.Resolve(ctx, "gh-token", Workload{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Resolve on a canceled context = %v, want context.Canceled", err)
	}
}
