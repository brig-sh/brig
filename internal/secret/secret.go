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
	"github.com/brig-sh/brig/internal/secret/internal/keyring"
)

// The API is defined in internal/secret/internal/keyring, which only code under
// this directory can import, and re-exported here so callers do not change.
type (
	// Secret is one entry, without its value.
	Secret = keyring.Secret
	// Store is the backend seam.
	Store = keyring.Store
	// Annotator is a backend that can attach provenance to a value.
	Annotator = keyring.Annotator
	// ContextStore is a Store whose calls can be bound to a context.
	ContextStore = keyring.ContextStore
	// Provenance is what brig knows about where a stored secret came from.
	Provenance = keyring.Provenance
)

// The errors callers distinguish.
var (
	ErrNotFound    = keyring.ErrNotFound
	ErrExists      = keyring.ErrExists
	ErrUnsupported = keyring.ErrUnsupported
	ErrDamaged     = keyring.ErrDamaged
)

// ProvenanceVersion is the schema version of the provenance comment document.
const ProvenanceVersion = keyring.ProvenanceVersion

var (
	// DecodeProvenance reads a comment attribute back into a Provenance.
	DecodeProvenance = keyring.DecodeProvenance
	// SafeFrom makes a source locator safe to store as provenance.
	SafeFrom = keyring.SafeFrom
	// ValidName reports whether a name is one brig will store.
	ValidName = keyring.ValidName
)

// JournalSuffix ends the name of a renewal journal. See keyring.JournalSuffix.
const JournalSuffix = keyring.JournalSuffix

// JournalName is the store name of the renewal journal of name.
func JournalName(name string) string { return keyring.JournalName(name) }

// service is brig's own namespace for the secrets it stores.
const service = "sh.brig.secret"

// Open returns the store for this host: the system keyring, which is the whole
// of the backend auto-detection.
func Open() (Store, error) { return keyring.Open(keyring.Config{Service: service}) }
