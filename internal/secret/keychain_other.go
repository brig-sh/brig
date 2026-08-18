//go:build !darwin

package secret

import "fmt"

// open reports the platform rather than failing later and vaguely. The Store
// interface is what makes a second backend an addition here; until one
// exists, saying so plainly is the whole implementation.
func open() (Store, error) {
	return nil, fmt.Errorf("%w: brig secret needs the macOS keychain so far", ErrUnsupported)
}
