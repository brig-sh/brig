// Package secret is the store brig owns.
//
// internal/creds resolves credentials that already exist somewhere else --
// brig's own environment, or the agent's own keychain item -- and forwards
// them into a guest. This package is the other half: named values brig itself
// creates, updates and deletes, so a secret that is not already exported has
// somewhere to live.
//
// The backend is behind Store because the system keyring is only the first
// one. Nothing above this interface knows which is in use.
package secret

import (
	"errors"
	"fmt"
	"time"
)

// The errors callers distinguish. The backends translate their own failures
// into these, so a message a user sees is brig's rather than the backend's.
var (
	ErrNotFound    = errors.New("no such secret")
	ErrExists      = errors.New("secret already exists")
	ErrUnsupported = errors.New("no secret store on this platform")
)

// Secret is one entry, without its value. Listing never reads values: the
// names are not sensitive and the values would each be a separate decrypt.
type Secret struct {
	Name string
	// Modified is best-effort, and a backend that cannot supply it returns
	// the zero time -- the keychain carries a modification date, and another
	// backend may not. Callers render a zero value as absent rather than
	// inventing a date: a made-up timestamp is worse than a visibly missing
	// one, and is the "tidy-up" this comment exists to prevent.
	Modified time.Time
	// Provenance is what brig knows about where this secret came from, read
	// from the backend's own metadata without decrypting the value. The zero
	// value means absent -- a hand-created secret, or a backend that cannot
	// carry it -- and callers render that as absent rather than guessing. Same
	// contract as Modified above.
	Provenance Provenance
}

// Store is the backend seam. A Vault, 1Password or cloud implementation is an
// addition here rather than a change above it.
type Store interface {
	// Kind names the backend, for reporting.
	Kind() string
	// Create stores a new secret and returns ErrExists if the name is taken.
	Create(name string, value []byte) error
	// Read returns the value, or ErrNotFound.
	Read(name string) ([]byte, error)
	// Update replaces an existing value, or returns ErrNotFound. It never
	// creates: an update naming a secret that is not there is a typo, and
	// creating it silently would hide that.
	Update(name string, value []byte) error
	// Delete removes a secret, or returns ErrNotFound.
	Delete(name string) error
	// List returns every secret in brig's namespace, sorted by name. Values
	// are never read: the names are not sensitive, and reading each value
	// would be a separate decrypt of something the caller did not ask for.
	List() ([]Secret, error)
}

// Annotator is a backend that can attach provenance to a value it stores.
//
// Optional rather than part of Store, and that is the point: a backend that
// cannot carry metadata implements Store alone and callers fall back to
// Create/Update, where provenance is simply absent. Same contract as
// Secret.Modified -- the zero value means absent and is rendered as such --
// so this follows a precedent rather than inventing one. A future backend
// (libsecret items carry arbitrary string attributes; see #8) can implement
// it without every other backend changing.
type Annotator interface {
	// Write stores a value with its provenance, creating or updating.
	Write(name string, value []byte, p Provenance, update bool) error
}

// Sizer is a backend with a value-size ceiling worth checking before writing.
//
// The keychain has one, and it truncates silently on a four-byte boundary --
// so a truncated value still base64-decodes and still resolves. Checking
// before the write is what stops a re-import destroying a good value and
// leaving a resolvable bad one behind, which is the failure verify cannot roll
// back on an update.
type Sizer interface {
	MaxValue(name string, update bool) int
}

// Open returns the store for this host. The platform files supply open(),
// which is the whole of the backend auto-detection: a host has one system
// keyring, and brig uses it.
func Open() (Store, error) { return open() }

// maxName is generous for a label and short enough that the error arrives
// before the keychain's own limits do.
const maxName = 128

// ValidName reports whether a name is one brig will store.
//
// The grammar is deliberately narrow. A name is three things at once: the
// keychain account, a word in brig's error messages, and -- once profiles can
// reference secrets -- the tail of `ref: secrets.<name>`. A dot would make
// that reference ambiguous, and a space or a slash would make the keychain
// account awkward to address by hand.
func ValidName(name string) error {
	if name == "" {
		return errors.New("a secret needs a name, for example `brig secret create gh-token`")
	}
	if len(name) > maxName {
		return fmt.Errorf("secret names are at most %d characters, and %q is %d",
			maxName, name, len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '_', c == '-':
			// A leading digit reads as a number rather than a name, and a
			// leading dash reads as a flag wherever the name is typed.
			if i == 0 {
				return fmt.Errorf("a secret name starts with a letter, and %q starts with %q",
					name, string(c))
			}
		default:
			return fmt.Errorf("a secret name holds letters, digits, - and _, and %q holds %q",
				name, string(c))
		}
	}
	return nil
}
