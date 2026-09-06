// Package exitcode maps a finished run's error to the stable exit status brig
// promises a script.
//
// The set is the exit-code table in README.md beside the command reference: 1 a
// general failure, 2 a usage error, 3 no such profile or sandbox, 4 a runtime
// that is missing or broken, 5 a boot verification refused, 6 a credential that
// could not be resolved. A caller that only checked "zero or not" keeps working;
// one that wants to branch on the reason now can.
//
// cmd/brig/exit.go is the other producer of these numbers -- it classifies the
// same internal error types for the CLI. It is package main and cannot be
// imported, and an open PR is editing it, so it is left where it is; once the
// branches touching it land it is to be moved onto this package so there is one
// classifier rather than two that can drift. Do not do that move here.
package exitcode

import (
	"errors"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// The exit statuses, matching README.md's table. A code no path returns is
// worse than no code at all, so the set is only the failure shapes brig's paths
// -- and brigd's -- actually produce.
const (
	OK          = 0
	Failure     = 1
	Usage       = 2
	NotFound    = 3
	Runtime     = 4
	Verify      = 5
	Credentials = 6
)

// UsageError marks a request brigd refused as malformed: an unknown op, an
// unknown protocol version, a value in the wrong place. It carries the message
// unchanged and only adds the class Of matches, so it maps to Usage.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// NotFoundError marks a name that resolves to nothing: a profile that does not
// exist, or a session the daemon has no record of and the runtime does not have
// running. It maps to NotFound.
type NotFoundError struct{ Err error }

func (e *NotFoundError) Error() string { return e.Err.Error() }
func (e *NotFoundError) Unwrap() error { return e.Err }

// Of reads the exit status for a finished operation out of its error. It reads
// the cause rather than the message, so a wrapped error keeps its class the
// whole way up, and the order is most specific first -- the same order and the
// same classes cmd/brig/exit.go reads.
func Of(err error) int {
	if err == nil {
		return OK
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		return Usage
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return NotFound
	}
	// Missing and broken are one class to a script: both mean "fix the runtime
	// before this can run", and neither is something the request itself did
	// wrong.
	if errors.Is(err, runtime.ErrNoRuntime) || errors.Is(err, runtime.ErrBadRuntime) {
		return Runtime
	}
	var vr *wrap.VerifyRefusedError
	if errors.As(err, &vr) {
		return Verify
	}
	// Both credential-failure shapes -- a required secret the store did not have
	// and a store that could not be opened -- are one class here.
	var ce creds.CredentialError
	if errors.As(err, &ce) {
		return Credentials
	}
	return Failure
}
