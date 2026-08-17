package creds

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// MissingSecretsError is every secret a run requires that the store does not
// have, collected into one error.
//
// This is the error a user sees when a run cannot resolve a ref, so it is
// written to answer the three questions they would otherwise have to ask: what
// is missing, what was it needed for, and what do I type. Collected rather than
// short-circuited, so a fresh host is fixed in one pass instead of one failed
// run per secret.
type MissingSecretsError struct {
	// Sandbox is the sandbox whose environment could not be built, named as the
	// user knows it -- the same name `brig ps` and `brig rm` take.
	Sandbox string
	// Names are the missing secrets, in the order the profile declares them.
	Names []string
}

func (e *MissingSecretsError) Error() string {
	// Singular gets one line, because one missing secret is the common case and
	// a bulleted list of one reads like a form. Plural gets the commands on
	// their own lines, where they can be copied one at a time.
	if len(e.Names) == 1 {
		return fmt.Sprintf("missing secret %q needed by the %s sandbox -- "+
			"create it first with: brig secret create %s",
			e.Names[0], e.Sandbox, e.Names[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "missing %d secrets needed by the %s sandbox -- "+
		"create them first:", len(e.Names), e.Sandbox)
	for _, name := range e.Names {
		fmt.Fprintf(&b, "\n  brig secret create %s", name)
	}
	return b.String()
}

// ResolveSecrets reads every secret the profile requires, once, up front
// before the sandbox's environment is built.
//
// Up front rather than at each binding, because the requirement list is what
// makes the error complete: resolving lazily would report the first miss and
// hide the rest. A profile requiring nothing never touches the store at all, so
// a run with no secrets raises no keychain prompt.
//
// Absence is a MissingSecretsError, which the caller turns into a failed run: a
// sandbox's environment built without a credential it was told it needs is
// exactly what the requirement list exists to prevent. Any other backend failure
// is returned as it came -- a locked keyring is not a secret you forgot to
// create, and telling someone to create it would send them the wrong way.
func ResolveSecrets(p profile.Profile, sandbox string, store SecretReader) (map[string]string, error) {
	if len(p.Secrets) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(p.Secrets))
	var missing []string
	for _, name := range p.Secrets {
		value, err := store.Read(name)
		switch {
		case err == nil:
			resolved[name] = string(value)
		case errors.Is(err, secret.ErrNotFound):
			missing = append(missing, name)
		default:
			return nil, fmt.Errorf("reading secret %q: %w", name, err)
		}
	}
	if len(missing) > 0 {
		return nil, &MissingSecretsError{Sandbox: sandbox, Names: missing}
	}
	return resolved, nil
}
