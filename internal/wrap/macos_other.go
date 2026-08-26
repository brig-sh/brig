//go:build !darwin

package wrap

// macOSVersion reports nothing off macOS: the hvi floor is a macOS fact, and
// the container runtime brig drives elsewhere ignores the hypervisor field
// entirely, so there is no version here to gate a run on.
func macOSVersion() string { return "" }
