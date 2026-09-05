package profile

import (
	"bytes"
	"encoding/json"
)

// The sources `brig secret import` knows how to read.
//
// Each names a mechanism, not a platform. `keychain` is the macOS keychain and
// stays macOS-specific: when a Linux backend arrives (#8) it appends
// `secret-service` to a chain rather than `keychain` quietly meaning two
// different things per platform. Naming a source for what it is beats a
// generic word whose meaning changes underfoot.
const (
	SourceKeychain = "keychain"
	SourceFile     = "file"
	SourceEnv      = "env"
)

// Source is one place a secret's value can be imported from.
//
// A secret carries a list of these and the first that exists wins, which is
// what makes a profile portable without a per-platform predicate: the same
// entry names the macOS keychain and the Linux file, brig never has to know
// which OS maps to which, and same-platform variation (a macOS user whose
// agent wrote a file) is covered for free.
type Source struct {
	From string `json:"from"`
	// Service is the macOS keychain generic-password service name.
	Service string `json:"service,omitempty"`
	// Path is a host file. A leading ~ is expanded when it is read, not here:
	// a profile is a shareable artifact and must not carry one host's home
	// directory baked into it.
	Path string `json:"path,omitempty"`
	// Var is an environment variable. `var:`, not `name:` -- at the secret
	// level `name:` is already the secret's own, so the shorthand form would
	// be ambiguous.
	Var string `json:"var,omitempty"`
	// Hint is what to tell the user when this source held nothing, for
	// example "run `claude` on the host once to log in".
	Hint string `json:"hint,omitempty"`
}

// Locator identifies the thing a source reads, ignoring everything that only
// describes it.
//
// Two uses, and they have to agree. `brig secret import` dedupes on it so that
// two secrets sharing one keychain item raise one approval dialog rather than
// two; and the stored provenance records it verbatim, so `brig secret ls` can
// say which source supplied a value. A hint is not part of it: the same item
// described two ways is still one read.
func (s Source) Locator() string { return s.From + ":" + s.locatorValue() }

func (s Source) locatorValue() string {
	switch s.From {
	case SourceKeychain:
		return s.Service
	case SourceFile:
		return s.Path
	case SourceEnv:
		return s.Var
	}
	return ""
}

// SecretDecl is one entry of `secrets:` -- a name the profile needs out of
// brig's own store, whether a run without it should stop, and where
// `brig secret import` may find it.
//
// Two axes decide what a run does about an unresolved secret, and nothing
// crosses over: `required:` decides whether the run stops, `sources:` decides
// which command the message names.
type SecretDecl struct {
	Name string `json:"name"`
	// Required decides whether an unresolved secret stops the run. A pointer
	// because absent and false are different answers: absent means required,
	// which is what keeps a bare string meaning what it always meant, and a
	// plain bool could not tell the two apart on export.
	Required *bool `json:"required,omitempty"`
	// Field and ExpiryField say how to read this secret's value and its expiry
	// out of whatever a source yields. They sit on the secret rather than on
	// the source because they describe the secret's own shape -- which is
	// single-valued only because every source for one secret must yield the
	// same document shape. A credential whose two locations differ in shape
	// needs two secrets; that is a documented constraint, not a checkable one.
	//
	// An absent Field means the value is stored verbatim, which is what a
	// file-shaped secret wants: the host keychain blob IS the format the
	// agent's credentials file takes, so nothing is extracted and no field
	// brig does not understand is lost.
	Field       string `json:"field,omitempty"`
	ExpiryField string `json:"expiryField,omitempty"`
	// Sources are tried in order, first hit wins.
	Sources []Source `json:"sources,omitempty"`
	// The singular shorthand: `from:` plus its locator is a one-element
	// sources:, the same way `ref:` is a `refs:` of length one. Validate
	// refuses both spellings at once.
	From    string `json:"from,omitempty"`
	Service string `json:"service,omitempty"`
	Path    string `json:"path,omitempty"`
	Var     string `json:"var,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// IsRequired reports whether an unresolved value stops the run. Absent means
// required, so a profile that says nothing keeps today's behaviour.
func (d SecretDecl) IsRequired() bool { return d.Required == nil || *d.Required }

// SourceList is the chain with the singular shorthand expanded, so every
// caller reads one shape.
func (d SecretDecl) SourceList() []Source {
	if len(d.Sources) > 0 {
		return d.Sources
	}
	if d.From == "" {
		return nil
	}
	return []Source{{From: d.From, Service: d.Service, Path: d.Path, Var: d.Var, Hint: d.Hint}}
}

// HintText is what to tell a user whose sources held nothing -- "run `claude`
// on the host once to log in". Only the profile knows what makes its
// credential appear, which is what the field is for.
//
// The declaration's own hint: comes first and a source's second, because the
// singular shorthand copies the declaration's into the one source it expands
// to -- so reading only one of the two would leave the shipped spellings
// answering differently depending on which form they were written in.
func (d SecretDecl) HintText() string {
	if d.Hint != "" {
		return d.Hint
	}
	for _, s := range d.SourceList() {
		if s.Hint != "" {
			return s.Hint
		}
	}
	return ""
}

// Importable reports whether `brig secret import` can fill this one. A secret
// with no sources is hand-created by definition, and its error names
// `brig secret create` instead -- which keeps the hint local to the failing
// secret rather than a search across every profile for one that covers it.
func (d SecretDecl) Importable() bool { return len(d.SourceList()) > 0 }

// UnmarshalJSON accepts either a bare name or the object form.
//
// The strict decoder inside is not optional. sigs.k8s.io/yaml's UnmarshalStrict
// sets DisallowUnknownFields on the outer decode, and a type with its own
// UnmarshalJSON takes its bytes raw -- so without this, `requred: false`
// decodes into nothing and the secret is silently required. That is precisely
// the failure strict decoding exists to prevent, and it would be invisible.
func (d *SecretDecl) UnmarshalJSON(b []byte) error {
	if trimmed := bytes.TrimSpace(b); len(trimmed) > 0 && trimmed[0] == '"' {
		var name string
		if err := json.Unmarshal(trimmed, &name); err != nil {
			return err
		}
		*d = SecretDecl{Name: name}
		return nil
	}
	// plain has no methods, so decoding into it does not recurse.
	type plain SecretDecl
	var v plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*d = SecretDecl(v)
	return nil
}

// SecretNames is the requirement list as bare names, for reporting and for the
// callers that only care that a name was declared.
func SecretNames(list []SecretDecl) []string {
	names := make([]string, 0, len(list))
	for _, d := range list {
		names = append(names, d.Name)
	}
	return names
}

// Secret finds one declaration by name.
func (p Profile) Secret(name string) (SecretDecl, bool) {
	for _, d := range p.Secrets {
		if d.Name == name {
			return d, true
		}
	}
	return SecretDecl{}, false
}
