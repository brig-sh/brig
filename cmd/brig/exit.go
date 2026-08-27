package main

import (
	"errors"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// Exit codes brig promises to a script. They are documented in the README
// beside the command reference, and the mapping below is the only thing that
// produces them, so the table and the behaviour cannot drift.
//
// 1 stays the general failure and 2 stays the usage error, so a caller written
// against either keeps working. The rest name the failure shapes brig's own
// paths already produce -- a profile that does not exist, a runtime that is
// missing or broken, a boot refused over verification, a credential that could
// not be resolved -- and nothing else, because a code no path returns is worse
// than no code at all.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitNotFound    = 3
	exitRuntime     = 4
	exitVerify      = 5
	exitCredentials = 6
)

// exitCode reads the exit status for a finished run out of its error. It reads
// the cause rather than the message, so a wrapped error keeps its class the
// whole way up the stack, and the order is most specific first.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	var nf *notFoundError
	if errors.As(err, &nf) {
		return exitNotFound
	}
	// Missing and broken are one class to a script: both mean "fix the runtime
	// before this can run", and neither is something the command itself did
	// wrong.
	if errors.Is(err, runtime.ErrNoRuntime) || errors.Is(err, runtime.ErrBadRuntime) {
		return exitRuntime
	}
	var vr *wrap.VerifyRefusedError
	if errors.As(err, &vr) {
		return exitVerify
	}
	// Both credential-failure shapes -- a required secret the store did not have
	// and a store that could not be opened -- are one class here.
	var ce creds.CredentialError
	if errors.As(err, &ce) {
		return exitCredentials
	}
	return exitFailure
}
