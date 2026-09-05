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
	// Hint is the declaration's own hint: -- "run `claude` on the host once to
	// log in". Carried on the miss rather than looked up again where the
	// message is built, because only the profile knows what makes its
	// credential appear and the message is built from Missing alone.
	Hint string
}

// CredentialError is the class of run-stopping credential failures: a required
// secret the store did not have (MissingSecretsError) and a store that could
// not be opened at all (storeError). The two read differently to a person but
// are one thing to a script -- a credential this run needed could not be
// resolved -- so a caller mapping a failure to an exit code matches on this
// rather than on either concrete type. The marker is unexported, so nothing
// outside this package can join the class.
type CredentialError interface {
	error
	credentialFailure()
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

func (e *MissingSecretsError) credentialFailure() {}

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
// The import arm names the PROFILE, because that is what the verb takes, and
// the profile is the canonical one rather than the word the user typed -- see
// MissingSecretsError.Profile.
func (e *MissingSecretsError) fix(m Missing) string {
	// A reason other than absence is not something creating a secret fixes:
	// the store refused to answer, and "create it first" would send the user
	// to a command that hits the same wall. Say what happened instead.
	if m.Reason != nil && !errors.Is(m.Reason, secret.ErrNotFound) {
		return fmt.Sprintf("could not be read: %v", m.Reason)
	}
	if m.Importable {
		return fmt.Sprintf("import it from your host with: brig secret import %s", e.Profile)
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
// the bindings do with it -- the requirement list describes the workload, not
// one binding.
//
// Computed before resolving rather than resolving lazily: laziness would
// report the first miss and hide the rest, while a needed set still yields one
// collected error naming every genuine miss.
//
// A files: binding has no earlier env source to fall back to, so it always
// needs its secret -- which is why a profile delivering a credential as a file
// opens the store on every run, and why the unavailable-store outcomes below
// must tell a platform invariant from something the user can act on.
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
	// A file binding has no earlier env source to fall back to, so it
	// unconditionally needs its secret -- which is what makes the
	// unavailable-store outcomes below load-bearing rather than theoretical:
	// a profile delivering a credential as a file opens the store on every
	// run, on every platform, including the one that has none.
	for _, b := range p.Files {
		r, err := profile.ParseRef(b.Ref)
		if err != nil {
			continue // Validate refuses these
		}
		if r.Namespace == profile.NamespaceSecrets {
			wanted[r.Name] = true
		}
	}
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
	var missing, optional []Missing
	for _, d := range needed {
		value, err := store.Read(d.Name)
		switch {
		case err == nil:
			res.Values[d.Name] = string(value)
			continue
		case errors.Is(err, secret.ErrNotFound):
			err = secret.ErrNotFound
		}
		m := newMissing(d, err)
		if m.Required {
			missing = append(missing, m)
			continue
		}
		optional = append(optional, m)
	}
	res.Warnings = warnOptional(p, optional)
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
	var required, optional []Missing
	var res Resolution
	for _, d := range needed {
		m := newMissing(d, err)
		if m.Required {
			required = append(required, m)
			continue
		}
		optional = append(optional, m)
	}
	// Through the same builder as an ordinary miss, so a store that could not
	// be opened says what a store that answered ErrNotFound says: ErrUnsupported
	// silent, everything else a state the user can change.
	res.Warnings = warnOptional(p, optional)
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

func (e *storeError) credentialFailure() {}

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

// newMissing is one unresolved secret, with everything a message about it
// needs read off the declaration in one place.
func newMissing(d profile.SecretDecl, reason error) Missing {
	return Missing{
		Name:       d.Name,
		Required:   d.IsRequired(),
		Importable: d.Importable(),
		Reason:     reason,
		Hint:       d.HintText(),
	}
}

// warnOptional is what a run says about the secrets it could not resolve and
// did not stop for. The three reasons need three sentences: telling someone to
// import a credential when the store could not be opened sends them at a wall
// they have already hit.
//
// The importable misses are collected into ONE block rather than one each,
// because the command is the same command: import takes the PROFILE, so a
// block per secret would print the identical "brig secret import claude-code"
// line once per name with nothing but the name to tell them apart.
func warnOptional(p profile.Profile, misses []Missing) []string {
	var out []string
	var importable []Missing
	for _, m := range misses {
		switch {
		case errors.Is(m.Reason, secret.ErrUnsupported):
			// A platform invariant: there is nothing the user can do about it
			// on this run, and saying so every time is noise rather than
			// information. Silent. Everything else -- a locked keychain, a
			// denied access dialog -- is a state the user can change.
		case errors.Is(m.Reason, secret.ErrNotFound) && m.Importable:
			importable = append(importable, m)
		case errors.Is(m.Reason, secret.ErrNotFound):
			// Names the secret, not the profile: a profile may declare
			// several, and two lines differing in nothing leave the reader
			// unable to tell which one to supply.
			out = append(out, fmt.Sprintf("no value for the secret %q, and %s will run "+
				"without it.\nTo supply one: brig secret create %s", m.Name, p.Name, m.Name))
		default:
			out = append(out, fmt.Sprintf("could not read brig's secret store: %v. "+
				"%s will run without %s", m.Reason, p.Name, m.Name))
		}
	}
	if len(importable) > 0 {
		out = append(out, importBlock(p, importable))
	}
	return out
}

// importBlock is the one block the importable misses share.
//
// It does not assert what the sandbox will do about it: a profile with two
// optional secrets has no business claiming a missing gh-token means the agent
// will ask you to log in. It names them, says the one command that fills them,
// and carries each declaration's own hint: -- attributed to its secret where
// there is more than one, because an unattributed list of hints under a list of
// names is a puzzle rather than advice.
func importBlock(p profile.Profile, misses []Missing) string {
	names := make([]string, 0, len(misses))
	for _, m := range misses {
		names = append(names, fmt.Sprintf("%q", m.Name))
	}
	var msg string
	if len(misses) == 1 {
		msg = fmt.Sprintf("no value for the secret %s, and %s will run without it.\n"+
			"To carry it in from your host: brig secret import %s", names[0], p.Name, p.Name)
	} else {
		msg = fmt.Sprintf("no value for the secrets %s, and %s will run without them.\n"+
			"To carry them in from your host: brig secret import %s",
			list(names), p.Name, p.Name)
	}
	for _, m := range misses {
		switch {
		case m.Hint == "":
		case len(misses) == 1:
			msg += "\n" + m.Hint
		default:
			msg += fmt.Sprintf("\n%s: %s", m.Name, m.Hint)
		}
	}
	return msg
}

// list joins names the way a sentence does, because this one is read as a
// sentence rather than scanned as a column.
func list(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
