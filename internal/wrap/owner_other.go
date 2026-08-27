//go:build !darwin && !linux

package wrap

import "os"

// dirWritableByUs has no answer without POSIX ownership either, so it says
// yes: nothing is trusted, and the whole path is walked one component at a
// time. Slower and stricter, which is the right way round for a guess.
var dirWritableByUs = func(string, os.FileInfo) bool { return true }
