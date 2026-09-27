package runtime

// FallbackReporter is a runtime that can say it settled for a binary nobody
// asked for.
//
// The nerdctl adapter takes docker from PATH when nerdctl is not there. It
// used to do that without a word, so someone who installed brig for a microVM
// boundary got whatever docker's setup gives them and no brig output said so
// (#30). The CLI type-asserts for this after it detects a runtime and prints
// the note at default verbosity.
//
// Optional on the same terms as TelemetryReporter. A runtime with nothing to
// fall back to needs no stub to say so.
type FallbackReporter interface {
	// Fallback is the one line to print, or "" when the binary is the first
	// choice or a setting named it.
	Fallback() string
}
