package creds

import (
	"errors"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// errKeyringLocked stands in for a backend that failed for a reason other than
// the secret being absent.
var errKeyringLocked = errors.New("keyring is locked")

// fakeStore is the SecretReader seam under test. The real one is the macOS
// keychain, which no unit test can assert against.
type fakeStore map[string]string

func (f fakeStore) Read(name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, secret.ErrNotFound
	}
	return []byte(v), nil
}

// panicStore fails loudly if it is read at all, for the cases whose whole point
// is that the store is never opened.
type panicStore struct{}

func (panicStore) Read(string) ([]byte, error) {
	panic("the store was read for a profile that declares no secrets")
}

type brokenStore struct{}

func (brokenStore) Read(string) ([]byte, error) { return nil, errKeyringLocked }

// profileWith parses a profile from the smallest valid preamble plus whatever
// the case is actually about.
func profileWith(t *testing.T, body string) profile.Profile {
	t.Helper()
	p, err := profile.Parse([]byte(
		"name: x\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n" + body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
