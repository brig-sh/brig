package creds

import "fmt"

// StoreUnavailableError reports that brig's own secret store could not be
// opened, independent of any one run's requirements.
//
// It joins CredentialError -- the unexported marker below is what admits it to
// the class -- so a bare probe of the store, which is all brig doctor's secrets
// check does, exits on the same credentials code (6) as a run that needed a
// secret and could not read it. A script then branches on "a credential brig
// handles could not be resolved" whether it learned that from a run or from
// doctor.
//
// It carries no sandbox or secret list because a probe named none; the cause is
// unwrapped, so errors.Is still reaches the backend's own sentinel -- a keychain
// lock, secret.ErrUnsupported -- the way it does through the run-time storeError.
type StoreUnavailableError struct{ Cause error }

func (e *StoreUnavailableError) Error() string {
	return fmt.Sprintf("brig's secret store could not be opened: %v", e.Cause)
}

func (e *StoreUnavailableError) Unwrap() error { return e.Cause }

func (e *StoreUnavailableError) credentialFailure() {}
