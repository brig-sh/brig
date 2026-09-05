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

// agentExit carries the agent's own exit status up to main, so brig returns
// exactly what the agent returned under --json. It is not a failure of brig's:
// the Run object has already been printed and the agent's output has already
// gone to the inherited streams, so main neither prints nor decorates it -- it
// reads the code off here and exits with it. Error is empty for that reason;
// nothing prints it.
type agentExit struct{ code int }

func (e *agentExit) Error() string { return "" }

// exitCode reads the exit status for a finished run out of its error. It reads
// the cause rather than the message, so a wrapped error keeps its class the
// whole way up the stack, and the order is most specific first.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	// The agent's own status, when the agent ran under --json. Read first: it is
	// the one error whose code is not a class of brig's but a number brig is
	// passing through unchanged.
	var ae *agentExit
	if errors.As(err, &ae) {
		return ae.code
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
