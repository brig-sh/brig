package wrap

import "github.com/brig-sh/brig/internal/secret"

// ptr is how a test says required: false. Package-level because several test
// files need it, and each declaring its own would not compile.
// copy to different files merge cleanly in git and then fail to compile, and
// no file-ownership table can see that -- it is a collision in the package
// namespace, not in a file.
func ptr(b bool) *bool { return &b }

// listingStore is a SecretReader that can also list, which is what the expiry
// warning type-asserts for. creds.SecretReader is deliberately read-only, so a
// backend that cannot list simply produces no warning.
type listingStore []secret.Secret

func (l listingStore) Read(name string) ([]byte, error) {
	for _, s := range l {
		if s.Name == name {
			return []byte("value"), nil
		}
	}
	return nil, secret.ErrNotFound
}

func (l listingStore) List() ([]secret.Secret, error) { return []secret.Secret(l), nil }
