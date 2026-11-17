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
// hull says through it that a setting of the other runtime is set and not
// read, which is the same kind of surprise: a run that is not what the
// reader asked for, and no line saying so.
//
// Optional on the same terms as NetworkPruner. A runtime with nothing to fall
// back to needs no stub to say so.
type FallbackReporter interface {
	// Fallback is the one line to print, or "" when the binary is the first
	// choice or a setting named it, and every setting is read.
	Fallback() string
}
