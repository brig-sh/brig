package renew

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/brig-sh/brig/internal/secret"
)

// errWriteFailed stands in for a keychain that refuses a write, a locked one
// for example.
var errWriteFailed = errors.New("the store refused the write")

// memStore is an in-memory Store, Annotator and ContextStore. The broker
// tests run on it and never on the login keychain.
type memStore struct {
	mu     sync.Mutex
	values map[string][]byte
	prov   map[string]secret.Provenance
	// failWrites is how many of the next writes fail.
	failWrites int
	writes     int
	lists      int
	// hang, while open, holds every call until it is closed or the call's
	// context ends, like a security(1) that never returns.
	hang chan struct{}
}

var (
	_ secret.ContextStore = (*memStore)(nil)
	_ secret.Annotator    = (*memStore)(nil)
	_ secret.Annotator    = memView{}
)

func newMemStore() *memStore {
	return &memStore{values: map[string][]byte{}, prov: map[string]secret.Provenance{}}
}

// memView is the store bound to a context.
type memView struct {
	m   *memStore
	ctx context.Context
}

func (m *memStore) WithContext(ctx context.Context) secret.Store { return memView{m, ctx} }

func (m *memStore) wait(ctx context.Context) error {
	m.mu.Lock()
	hang := m.hang
	m.mu.Unlock()
	if hang == nil {
		return nil
	}
	select {
	case <-hang:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v memView) Kind() string { return v.m.Kind() }
func (v memView) Create(name string, value []byte) error {
	return v.Write(name, value, secret.Provenance{}, false)
}
func (v memView) Update(name string, value []byte) error {
	return v.Write(name, value, secret.Provenance{}, true)
}

func (v memView) Read(name string) ([]byte, error) {
	if err := v.m.wait(v.ctx); err != nil {
		return nil, err
	}
	return v.m.read(name)
}

func (v memView) Delete(name string) error {
	if err := v.m.wait(v.ctx); err != nil {
		return err
	}
	return v.m.delete(name)
}

func (v memView) List() ([]secret.Secret, error) {
	if err := v.m.wait(v.ctx); err != nil {
		return nil, err
	}
	return v.m.list()
}

func (v memView) Write(name string, value []byte, p secret.Provenance, update bool) error {
	if err := v.m.wait(v.ctx); err != nil {
		return err
	}
	return v.m.write(name, value, p, update)
}

func (m *memStore) Kind() string { return "memory" }
func (m *memStore) Create(name string, value []byte) error {
	return m.WithContext(context.Background()).Create(name, value)
}
func (m *memStore) Read(name string) ([]byte, error) {
	return m.WithContext(context.Background()).Read(name)
}
func (m *memStore) Update(name string, value []byte) error {
	return m.WithContext(context.Background()).Update(name, value)
}
func (m *memStore) Delete(name string) error       { return m.WithContext(context.Background()).Delete(name) }
func (m *memStore) List() ([]secret.Secret, error) { return m.WithContext(context.Background()).List() }
func (m *memStore) Write(name string, value []byte, p secret.Provenance, update bool) error {
	return m.WithContext(context.Background()).(memView).Write(name, value, p, update)
}

func (m *memStore) read(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return slices.Clone(v), nil
}

func (m *memStore) delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[name]; !ok {
		return secret.ErrNotFound
	}
	delete(m.values, name)
	delete(m.prov, name)
	return nil
}

func (m *memStore) list() ([]secret.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lists++
	var out []secret.Secret
	for name := range m.values {
		out = append(out, secret.Secret{Name: name, Provenance: m.prov[name]})
	}
	slices.SortFunc(out, func(a, b secret.Secret) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memStore) write(name string, value []byte, p secret.Provenance, update bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.failWrites > 0 {
		m.failWrites--
		return errWriteFailed
	}
	_, exists := m.values[name]
	switch {
	case update && !exists:
		return secret.ErrNotFound
	case !update && exists:
		return secret.ErrExists
	}
	m.values[name] = slices.Clone(value)
	m.prov[name] = p
	return nil
}

func (m *memStore) failNext(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWrites = n
}

func (m *memStore) setHang(hang chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hang = hang
}

func (m *memStore) counts() (writes, lists int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes, m.lists
}

func (m *memStore) value(name string) []byte {
	v, _ := m.read(name)
	return v
}

func (m *memStore) provenance(name string) secret.Provenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prov[name]
}
