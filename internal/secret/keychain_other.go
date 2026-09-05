//go:build !darwin && !linux

package secret

import "fmt"

// open reports the platform rather than failing later and vaguely. The Store
// interface is what makes a second backend an addition here; darwin has the
// keychain and linux the Secret Service, so this file is now every other
// platform, where saying so plainly is the whole implementation.
func open() (Store, error) {
	return nil, fmt.Errorf("%w: brig secret needs the macOS keychain or a Linux Secret Service keyring so far", ErrUnsupported)
}
