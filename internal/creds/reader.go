package creds

// SecretReader is the part of secret.Store that resolving a profile needs.
//
// Narrowed to one method on purpose: resolution reads, and a seam that could
// also create or delete would let a future caller mutate the store from the run
// path. secret.Store satisfies this without being named here, which is also
// what keeps the tests off the keychain.
type SecretReader interface {
	Read(name string) ([]byte, error)
}
