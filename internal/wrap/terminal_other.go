//go:build !darwin && !linux

package wrap

// isatty on a platform brig does not ship for.
//
// Answering no is the safe half of every decision this feeds: a confirmation
// that cannot be asked is refused rather than assumed, and an exec that cannot
// tell gets no pseudo-terminal rather than one nothing is driving. brig builds
// for darwin and linux, so this exists to keep `go build ./...` honest
// elsewhere rather than to serve anyone.
func isatty(uintptr) bool { return false }
