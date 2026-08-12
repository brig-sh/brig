//go:build !darwin

package wrap

// Only the macOS runner opens a window of its own.
func focusWindow() {}
