package creds

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// Missing is one secret a run could not resolve, and why.
//
// Two independent axes decide what happens about it. Required decides whether
// the run stops; Importable decides which command the message names. Nothing
// crosses over, which is why they are separate fields rather than one verdict.
type Missing struct {
	Name       string
	Required   bool
	Importable bool
	// Reason is what the store actually said: secret.ErrNotFound,
	// secret.ErrUnsupported, or its own failure. "You could import it" is bad
	// advice when importing would fail for the same reason the run could not
	// read the store, so the message reads this rather than assuming absence.
	Reason error
}

// MissingSecretsError is every REQUIRED secret a run could not resolve,
// collected into one error.
//
// Collected rather than short-circuited, so a fresh host is fixed in one pass
// instead of one failed run per secret. Optional misses are not here: they are
// warnings on the Resolution, because the run proceeds.
type MissingSecretsError struct {
	// Sandbox is the sandbox whose environment could not be built, named as
	// the user knows it -- the same name `brig ps` and `brig rm` take.
	Sandbox string
	// Profile is the canonical profile name, which is what
	// `brig secret import <profile>` takes. Canonical rather than as typed:
	// `claude` is an alias and a user's own file can shadow a built-in, so the
	// message has to name the profile actually loaded.
	Profile string
	Missing []Missing
}

func (e *MissingSecretsError) Error() string {
	// Singular gets one line, because one missing secret is the common case
	// and a bulleted list of one reads like a form.
	if len(e.Missing) == 1 {
		m := e.Missing[0]
		return fmt.Sprintf("missing secret %q needed by the %s sandbox -- %s",
			m.Name, e.Sandbox, e.fix(m))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "missing %d secrets needed by the %s sandbox:", len(e.Missing), e.Sandbox)
	for _, m := range e.Missing {
		fmt.Fprintf(&b, "\n  %s: %s", m.Name, e.fix(m))
	}
	return b.String()
}

// Unwrap exposes the reasons, so errors.Is reaches a backend's sentinel
// whether the store failed to open or failed on one read. Without it a caller
// could tell a locked keychain from an absent secret in one case and not the
// other, for no reason a caller could see.
func (e *MissingSecretsError) Unwrap() []error {
	reasons := make([]error, 0, len(e.Missing))
	for _, m := range e.Missing {
		if m.Reason != nil {
			reasons = append(reasons, m.Reason)
		}
	}
	return reasons
}

// fix names the command that supplies one secret. A mix lists each name
// against its own, one per line: the two commands are not interchangeable and
// a single trailing hint would send half the names the wrong way.
//
// Importable is read but not yet acted on: `brig secret import` does not exist
// until PR 5, and a message naming a verb that answers "unknown secret
// subcommand" is worse than one naming a verb that works. PR 5 adds the arm in
// the same change that adds the dispatch.
func (e *MissingSecretsError) fix(m Missing) string {
	// A reason other than absence is not something creating a secret fixes:
	// the store refused to answer, and "create it first" would send the user
	// to a command that hits the same wall. Say what happened instead.
	if m.Reason != nil && !errors.Is(m.Reason, secret.ErrNotFound) {
		return fmt.Sprintf("could not be read: %v", m.Reason)
	}
	return fmt.Sprintf("create it first with: brig secret create %s", m.Name)
}

// Resolution is what a run got out of the store, plus what it wants to say
// about what it did not get.
//
// Warnings rather than an error because these are the optional misses: the run
// proceeds, and the agent asks for a login. Held on the value instead of
// printed here so that resolution stays testable without a writer.
type Resolution struct {
	Values   map[string]string
	Warnings []string
}

// Needed computes the secrets this run actually has to resolve.
//
// Walk the bindings first: a chain whose earlier env. element resolves does
// not contribute its secrets. element, so a run the environment already
// satisfies never touches the store. Every required secret is needed whatever
// the bindings do with it -- the requirement list is a statement about the
// workload, not about one binding.
//
// Computed BEFORE resolving rather than resolving lazily, which is the
// property up-front resolution exists for: laziness would report the first
// miss and hide the rest, while a needed set still yields one collected error
// naming every genuine miss.
//
// Note what this does not save. A files: binding has no earlier env source to
// fall back to, so it unconditionally needs its secret -- which means a
// profile delivering a credential as a file opens the store on every run, on
// every platform. That is why the unavailable-store outcomes below have to
// tell a platform invariant from an actionable failure.
func Needed(p profile.Profile, lookup func(string) (string, bool)) []profile.SecretDecl {
	wanted := map[string]bool{}
	for _, b := range p.Env {
		for _, raw := range b.RefList() {
			r, err := profile.ParseRef(raw)
			if err != nil {
				continue // Validate refuses these; a Go-built profile warns at Bind
			}
			if r.Namespace == profile.NamespaceEnv {
				if v, ok := lookup(r.Name); ok && v != "" {
					break // this binding is answered; later elements are not reached
				}
				continue
			}
			wanted[r.Name] = true
		}
	}
	// PR 6 adds the files: walk here. A file binding has no earlier env source
	// to fall back to, so it unconditionally needs its secret -- which is what
	// makes the unavailable-store outcomes below load-bearing rather than
	// theoretical. Until then only env bindings and required secrets reach the
	// store.
	var out []profile.SecretDecl
	for _, d := range p.Secrets {
		if d.IsRequired() || wanted[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// ResolveSecrets reads what this run needs out of the store, once, up front
// before the sandbox's environment is built.
//
// The store is opened only when something needs it, which is what keeps a run
// with no secrets -- and a run the environment already answers -- free of any
// keychain prompt. open is passed rather than an opened store for exactly that
// reason: the decision to open is part of resolution.
func ResolveSecrets(
	p profile.Profile,
	sandbox string,
	open func() (SecretReader, error),
	lookup func(string) (string, bool),
) (Resolution, error) {
	needed := Needed(p, lookup)
	if len(needed) == 0 {
		return Resolution{}, nil
	}

	store, err := open()
	if err != nil {
		return unavailable(p, sandbox, needed, err)
	}

	res := Resolution{Values: make(map[string]string, len(needed))}
	var missing []Missing
	for _, d := range needed {
		value, err := store.Read(d.Name)
		switch {
		case err == nil:
			res.Values[d.Name] = string(value)
			continue
		case errors.Is(err, secret.ErrNotFound):
			err = secret.ErrNotFound
		}
		m := Missing{Name: d.Name, Required: d.IsRequired(), Importable: d.Importable(), Reason: err}
		if m.Required {
			missing = append(missing, m)
			continue
		}
		if w := warn(p, m); w != "" {
			res.Warnings = append(res.Warnings, w)
		}
	}
	// The required failures win the report: a run that stopped must say why it
	// stopped, not bury it under advice about something that did not stop it.
	if len(missing) > 0 {
		return Resolution{}, &MissingSecretsError{Sandbox: sandbox, Profile: p.Name, Missing: missing}
	}
	return res, nil
}

// unavailable decides what a store that could not be opened means, which is
// three different things.
func unavailable(p profile.Profile, sandbox string, needed []profile.SecretDecl, err error) (Resolution, error) {
	var required []Missing
	var res Resolution
	for _, d := range needed {
		m := Missing{Name: d.Name, Required: d.IsRequired(), Importable: d.Importable(), Reason: err}
		if m.Required {
			required = append(required, m)
			continue
		}
		// ErrUnsupported is a platform invariant: there is nothing the user can
		// do about it on this run, and saying so every time is noise rather
		// than information. Silent. Everything else -- a locked keychain, a
		// denied access dialog -- is a state the user can change.
		if errors.Is(err, secret.ErrUnsupported) {
			continue
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"could not read brig's secret store: %v. %s will run without %s",
			err, p.Name, m.Name))
	}
	if len(required) > 0 {
		// storeError unwraps to the backend's own error, so a caller can still
		// tell ErrUnsupported from a lock.
		return Resolution{}, &storeError{
			sandbox: sandbox, missing: required, cause: err, advice: storeAdvice(p, required, err)}
	}
	return res, nil
}

// storeError is a MissingSecretsError whose cause is the store rather than the
// secret: the names are missing because nothing could be read, so the message
// leads with that and errors.Is still reaches the backend's sentinel.
type storeError struct {
	sandbox string
	missing []Missing
	cause   error
	// advice is the one place the platform-aware hint lives. Optional plus
	// ErrUnsupported is silent, so a required secret on a storeless platform
	// is the only run that has anything to say about it.
	advice string
}

func (e *storeError) Error() string {
	names := make([]string, 0, len(e.missing))
	for _, m := range e.missing {
		names = append(names, m.Name)
	}
	msg := fmt.Sprintf("the %s sandbox needs %s from brig's secret store, which could not be "+
		"read: %v", e.sandbox, strings.Join(names, ", "), e.cause)
	if e.advice != "" {
		msg += ". " + e.advice
	}
	return msg
}

func (e *storeError) Unwrap() error { return e.cause }

// storeAdvice says what to do when the platform has no store at all, computed
// from the profile's own bindings rather than stated generically.
//
// "Export the value instead" is a no-op for a bare `ref: secrets.<name>`
// binding: envOverride drops names the profile binds, so a user follows that
// instruction exactly and the run fails identically. Only a chain whose first
// element is an env. ref reads the shell at all, so only that spelling is
// told to export; the rest are told how to get the spelling that would work.
// A secret with no environment binding has no environment spelling, and this
// must not invent one.
func storeAdvice(p profile.Profile, missing []Missing, cause error) string {
	if !errors.Is(cause, secret.ErrUnsupported) {
		return ""
	}
	var exportable, storeOnly, suggestions []string
	for _, m := range missing {
		variable, first := envSpelling(p, m.Name)
		switch {
		case variable == "":
			// Bound through no env binding at all -- a files: binding, or a
			// requirement nothing binds. Nothing to export.
		case first:
			exportable = append(exportable, variable)
		default:
			storeOnly = append(storeOnly, m.Name)
			suggestions = append(suggestions, fmt.Sprintf("`refs: [%s.%s, %s.%s]`",
				profile.NamespaceEnv, variable, profile.NamespaceSecrets, m.Name))
		}
	}
	var out []string
	if len(exportable) > 0 {
		out = append(out, fmt.Sprintf("Export %s before running brig, which this profile "+
			"reads first", strings.Join(exportable, ", ")))
	}
	if len(storeOnly) > 0 {
		out = append(out, fmt.Sprintf("This profile reads %s only from the store, so run it "+
			"on macOS, or add %s to its env binding",
			strings.Join(storeOnly, ", "), strings.Join(suggestions, ", ")))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ". ") + "."
}

// envSpelling finds the variable a secret reaches the guest as, and whether
// the chain reads the shell before the store -- which is what decides whether
// exporting it does anything.
func envSpelling(p profile.Profile, name string) (variable string, readsTheShellFirst bool) {
	for _, b := range p.Env {
		list := b.RefList()
		for _, raw := range list {
			r, err := profile.ParseRef(raw)
			if err != nil || r.Namespace != profile.NamespaceSecrets || r.Name != name {
				continue
			}
			if first, err := profile.ParseRef(list[0]); err == nil &&
				first.Namespace == profile.NamespaceEnv {
				return first.Name, true
			}
			return b.Name, false
		}
	}
	return "", false
}

// warn is what an optional miss says. The three reasons need three sentences:
// telling someone to import a credential when the store could not be opened
// sends them at a wall they have already hit.
func warn(p profile.Profile, m Missing) string {
	switch {
	case errors.Is(m.Reason, secret.ErrUnsupported):
		return ""
	case errors.Is(m.Reason, secret.ErrNotFound):
		// Names the SECRET, not the profile. An earlier draft formatted p.Name
		// twice and m.Name never, which on the shipped claude-code -- two
		// optional importable secrets, both always needed -- printed the same
		// two lines twice with nothing to tell them apart, and told a user who
		// had imported one of them that they had no credential at all.
		//
		// The importable arm, and the per-secret hint that goes with it,
		// arrives in PR 5 with the verb.
		return fmt.Sprintf("no value for the secret %q, and %s will run without it.\n"+
			"To supply one: brig secret create %s", m.Name, p.Name, m.Name)
	default:
		return fmt.Sprintf("could not read brig's secret store: %v. %s will run without %s",
			m.Reason, p.Name, m.Name)
	}
}
