package creds

import (
	"fmt"

	"github.com/brig-sh/brig/internal/profile"
)

// Bind turns a profile's env bindings into the variables the guest is handed.
//
// `secrets` is what ResolveSecrets already read, so binding does no I/O and a
// missing secret has already failed the run by the time this is reached -- which
// is what keeps the requirement error firing once rather than once per binding.
//
// An unset or empty value is skipped rather than bound empty, whatever its
// source: an empty value would shadow one baked into the image, which is the
// rule forwarding has always followed, and a secret that is genuinely empty is
// indistinguishable from that.
//
// A binding whose ref does not parse is skipped with a warning rather than
// failing the run: Validate rejects those at parse time, so reaching one here
// means a profile built in Go rather than read from a file, and refusing to
// create the sandbox over it would be the worse trade.
func Bind(
	p profile.Profile,
	bindings []profile.EnvBinding,
	secrets map[string]string,
	lookup func(string) (string, bool),
	opt Options,
) Set {
	var s Set
	for _, b := range bindings {
		var value, annotation string
		var r profile.Ref
		fromEnv, fromSecret := false, false
		// A secret the store returned empty and one that never reached this
		// map both leave value == "", and they are different mistakes with
		// different fixes, so the lookup keeps the two apart.
		secretPresent := false

		if b.Ref == "" {
			value = b.Value
		} else {
			var err error
			r, _, err = b.Resolved()
			if err != nil {
				s.Warnings = append(s.Warnings,
					fmt.Sprintf("not forwarding %s: %v", b.Name, err))
				continue
			}
			switch r.Namespace {
			case profile.NamespaceSecrets:
				value, secretPresent = secrets[r.Name]
				annotation = "(secret)"
				fromSecret = true
			case profile.NamespaceEnv:
				v, ok := lookup(r.Name)
				if !ok {
					continue
				}
				value, fromEnv = v, true
			}
		}

		if value == "" {
			switch {
			case fromSecret && secretPresent:
				// brig itself refuses to write a secret empty
				// (cmd/brig/secret.go), so one reaching here means another
				// keychain tool emptied it. Silence would leave the guest
				// failing to authenticate with no explanation.
				s.Warnings = append(s.Warnings, fmt.Sprintf(
					"not forwarding %s: the secret %s is empty, not absent. Set it "+
						"again with `brig secret update %s`, or remove it with "+
						"`brig secret delete %s` if it should not exist",
					b.Name, r.Name, r.Name, r.Name))
			case fromSecret:
				// Nothing resolved this one. A profile read from a file cannot
				// get here -- Validate refuses a secrets. ref the list does not
				// declare, and ResolveSecrets fails the run before Bind when the
				// store does not have a declared name -- so this is a Profile
				// built in Go whose secrets list does not cover its own
				// bindings. Same class as an unparsable ref: warn and carry on
				// rather than refuse to create the sandbox.
				s.Warnings = append(s.Warnings, fmt.Sprintf(
					"not forwarding %s: the secret %s was never resolved, because it is "+
						"not on this profile's secrets list", b.Name, r.Name))
			}
			continue
		}
		if warning, ok := admit(p, b.Name, value, fromEnv, opt); !ok {
			s.Warnings = append(s.Warnings, warning)
			continue
		}
		// A secret travels through AddSecret rather than Add so that
		// BRIG_ENV_ARGV can never put it in argv: the host durably logs every
		// exec's argv, and that turns an opt-in debugging escape hatch into a
		// keychain leak the moment this feature lands.
		if fromSecret {
			s.AddSecret(b.Name, value, b.Name+annotation)
			continue
		}
		s.Add(b.Name, value, b.Name+annotation)
	}
	return s
}
