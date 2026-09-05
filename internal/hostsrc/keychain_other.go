//go:build !darwin

package hostsrc

// readKeychain reports absence rather than failing: there is no keychain on
// this platform, and a chain that names one falls through to whatever comes
// next exactly as if this host had never run the agent. That fallthrough --
// not a Linux keychain backend, which is #8 -- is the only thing this file
// exists for.
func readKeychain(service string) ([]byte, error) {
	return nil, errNoSuchItem
}
