package secret

import (
	"context"
	"errors"
	"time"
)

// Workload names the sandbox a secret is resolved for.
//
// It is carried for the audit line. Nothing in this package decides access
// from it.
type Workload struct {
	// Sandbox is the runtime's instance name, the one `brig info` prints.
	Sandbox string
	// Profile is the canonical profile name.
	Profile string
	// Session is the named session, or empty for the default one.
	Session string
}

// Broker resolves a named secret for a workload.
//
// Store answers by name alone. A broker also learns who is asking, so a later
// backend can present the workload to a provider that checks it. Brig's own
// store ignores it.
type Broker interface {
	// Resolve returns the value of name, or ErrNotFound.
	Resolve(ctx context.Context, name string, w Workload) ([]byte, error)
}

// StoreBroker returns the Broker over one of brig's own stores, so the
// keychain and the Secret Service serve the broker without either backend
// changing. A read is bound to the context of Resolve where the backend
// supports that.
func StoreBroker(s Store) Broker { return storeBroker{s} }

type storeBroker struct{ s Store }

func (b storeBroker) Resolve(ctx context.Context, name string, _ Workload) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return WithContext(ctx, b.s).Read(name)
}

// ContextStore is a Store whose calls can be bound to a context.
//
// Optional, like Annotator. A backend that waits on another process, or on a
// keyring prompt, implements it so that a caller with a deadline is not held
// past it.
type ContextStore interface {
	Store
	// WithContext returns the store with every call bound to ctx.
	WithContext(ctx context.Context) Store
}

// WithContext returns s with its calls bound to ctx where the backend
// supports that, and s itself otherwise.
func WithContext(ctx context.Context, s Store) Store {
	if c, ok := s.(ContextStore); ok {
		return c.WithContext(ctx)
	}
	return s
}

// WriteWithProvenance stores a value with its provenance where the backend
// can carry one, and without where it cannot.
//
// The Annotator arm is one call for both create and update, so a write never
// takes the delete-then-create shape. That shape lets a concurrent run fail
// with "missing secret" in the window between the two.
func WriteWithProvenance(s Store, name string, value []byte, p Provenance, update bool) error {
	if a, ok := s.(Annotator); ok {
		return a.Write(name, value, p, update)
	}
	// A backend with no room for provenance still takes the value. Absent
	// provenance is the zero value a hand-created secret carries too.
	if update {
		return s.Update(name, value)
	}
	return s.Create(name, value)
}

// Renewer is a credential type whose value can be renewed from the host.
//
// Optional, following the Annotator precedent: a static credential has no
// Renewer. Renew never writes the store. The caller does, so one place
// decides what happens when that write fails.
type Renewer interface {
	// Renew takes the current document and returns the one that replaces
	// it. After a successful Renew the provider may already refuse current.
	// A failure after the request reached the provider wraps ErrSpent.
	Renew(ctx context.Context, current []byte) ([]byte, error)
}

// Expirer is a Renewer that reads the expiry out of its own document, so the
// caller can renew ahead of it.
type Expirer interface {
	// Expiry returns when doc stops being valid, or the zero time when doc
	// carries no expiry.
	Expiry(doc []byte) (time.Time, error)
}

// ErrSpent marks a renewal that failed after its request reached the
// provider. The provider may have rotated the credential before the failure,
// so the document the renewal started from must not be renewed again.
var ErrSpent = errors.New("the renewal reached the provider, which may have rotated the credential")
