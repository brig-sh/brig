package wrap

import "github.com/brig-sh/brig/internal/creds"

// deliverSecretFiles writes the profile's files: bindings into the guest.
//
// A no-op until the delivery mechanism lands (PR 6): the seam exists here so
// that EnsureRunning has exactly one owner, and so the two call sites -- the
// already-running path and the fresh boot -- are written once rather than
// added later by a task that also has a guest to reason about.
//
// It clears the resolved values on the way out, and does so already: the
// discipline belongs in place before there is anything to protect. Defence in
// depth rather than a control -- a Go process memory dump is already game over
// -- but retaining a credential for the process lifetime is the wrong default
// to write down.
func (c *Config) deliverSecretFiles() error {
	c.secrets = creds.Resolution{}
	return nil
}
