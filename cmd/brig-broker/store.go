package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/brig-sh/brig/internal/secret"
)

// lazyStore is brig's secret store on a host where it did not open when the
// broker started: a Linux host with no Secret Service on its session bus, or
// with no session bus at all. Each call tries to open it again, and once one
// does, every call goes to it. Until then each call fails with the reason,
// and a route that reads its credential answers 502, so the broker still
// starts, registers sandboxes and mints their tokens.
type lazyStore struct {
	*opener
	ctx context.Context
}

// opener holds the store once it opened.
type opener struct {
	open func() (secret.Store, error)

	mu sync.Mutex
	s  secret.Store
}

var (
	_ secret.ContextStore = lazyStore{}
	_ secret.Annotator    = lazyStore{}
)

func newLazyStore(open func() (secret.Store, error)) lazyStore {
	return lazyStore{opener: &opener{open: open}}
}

// store returns the opened store, bound to the context of l when it has one.
func (l lazyStore) store() (secret.Store, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == nil {
		s, err := l.open()
		if err != nil {
			return nil, fmt.Errorf("cannot open brig's secret store: %w", err)
		}
		l.s = s
	}
	if l.ctx != nil {
		return secret.WithContext(l.ctx, l.s), nil
	}
	return l.s, nil
}

func (l lazyStore) Kind() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == nil {
		return "unavailable"
	}
	return l.s.Kind()
}

func (l lazyStore) Create(name string, value []byte) error {
	s, err := l.store()
	if err != nil {
		return err
	}
	return s.Create(name, value)
}

func (l lazyStore) Read(name string) ([]byte, error) {
	s, err := l.store()
	if err != nil {
		return nil, err
	}
	return s.Read(name)
}

func (l lazyStore) Update(name string, value []byte) error {
	s, err := l.store()
	if err != nil {
		return err
	}
	return s.Update(name, value)
}

func (l lazyStore) Delete(name string) error {
	s, err := l.store()
	if err != nil {
		return err
	}
	return s.Delete(name)
}

func (l lazyStore) List() ([]secret.Secret, error) {
	s, err := l.store()
	if err != nil {
		return nil, err
	}
	return s.List()
}

// Write keeps the provenance where the store, once open, carries one.
func (l lazyStore) Write(name string, value []byte, p secret.Provenance, update bool) error {
	s, err := l.store()
	if err != nil {
		return err
	}
	return secret.WriteWithProvenance(s, name, value, p, update)
}

func (l lazyStore) WithContext(ctx context.Context) secret.Store {
	l.ctx = ctx
	return l
}
