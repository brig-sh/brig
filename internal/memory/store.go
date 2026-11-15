// Package memory is the host-side store for agent memory, and the MCP handler
// that serves one store to the agent in the sandbox.
//
// A store is a directory of Markdown files that outlives any sandbox. The
// guest chooses memory names, so every path derived from a name is resolved
// through an [os.Root] opened on the store directory and never by joining
// strings: a name cannot climb out, and a symlink planted in the store cannot
// lead the host to a file the guest was never given. This is the first
// promise (the guest reaches only the host directories brig names for it)
// applied to a directory the guest writes into through the broker.
//
// The package knows no tokens, credentials or sandboxes. Who may reach which
// store is decided by the caller, so this package cannot widen it.
package memory

import (
	"context"
	"errors"

	"github.com/brig-sh/brig/internal/memory/memkey"
)

// Entry is one line of a store's index.
type Entry struct{ Name, Description string }

// Memory is one stored note.
type Memory struct{ Name, Description, Body string }

// Store is what the MCP handler needs from a memory store.
type Store interface {
	// Index lists every memory, sorted by name.
	Index(ctx context.Context) ([]Entry, error)
	// Get returns [ErrNotFound] when there is no such memory.
	Get(ctx context.Context, name string) (Memory, error)
	// Put creates a memory or replaces the one with the same name.
	Put(ctx context.Context, m Memory) error
	// Delete returns [ErrNotFound] when there is no such memory.
	Delete(ctx context.Context, name string) error
	// Search matches the query, ignoring case, against name, description
	// and body, and returns the matches sorted by name.
	Search(ctx context.Context, query string) ([]Entry, error)
}

var (
	ErrNotFound    = errors.New("memory not found")
	ErrInvalidName = memkey.ErrInvalidName
	ErrTooLarge    = errors.New("memory too large")
	ErrStoreFull   = errors.New("memory store is full")
)

// The limits keep a guest from filling the host disk, and keep one memory
// small enough to put in a model's context.
const (
	MaxBody        = 64 << 10
	MaxDescription = 256
	MaxName        = memkey.MaxName
	MaxMemories    = 500
)
