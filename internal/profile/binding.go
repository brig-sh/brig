package profile

import (
	"fmt"
	"strings"
)

// The namespaces a ref may name.
//
// Two of them, deliberately. secrets. reads the store brig owns; env. reads
// brig's own environment, which is what gives the ambient-shell case a
// spelling in the new syntax -- and so what lets forward: be deprecated rather
// than kept alongside it forever. Two mechanisms for "get a variable into the
// guest" is the thing worth avoiding.
//
// A third source would be an addition here rather than a change to the schema.
const (
	NamespaceSecrets = "secrets"
	NamespaceEnv     = "env"
)

// Ref is a parsed `ref:` -- where a binding's value comes from.
type Ref struct {
	Namespace string
	Name      string
}

// String renders a ref back into the form a profile writes it in.
func (r Ref) String() string { return r.Namespace + "." + r.Name }

// ParseRef parses `<namespace>.<name>`.
//
// Split on the first dot only: a secret name holds no dot (see
// secret.ValidName, whose grammar narrowed for exactly this reason), but an
// environment variable name is the caller's and may hold anything, so splitting
// on the last dot would quietly truncate it.
func ParseRef(s string) (Ref, error) {
	namespace, name, ok := strings.Cut(s, ".")
	if !ok {
		return Ref{}, fmt.Errorf("ref %q needs a namespace: %s.<name> reads the store "+
			"brig owns, %s.<name> reads brig's own environment",
			s, NamespaceSecrets, NamespaceEnv)
	}
	if namespace == "" || name == "" {
		return Ref{}, fmt.Errorf("ref %q is not %s.<name> or %s.<name>",
			s, NamespaceSecrets, NamespaceEnv)
	}
	switch namespace {
	case NamespaceSecrets, NamespaceEnv:
		return Ref{Namespace: namespace, Name: name}, nil
	default:
		return Ref{}, fmt.Errorf("ref %q names no namespace brig has: %q is not %s or %s",
			s, namespace, NamespaceSecrets, NamespaceEnv)
	}
}

// EnvBinding is one environment variable the guest sees, and where its value
// comes from.
//
// Exactly one of Value or Ref is set. Both is a contradiction with no safe
// precedence rule -- either answer surprises whoever wrote both -- and neither
// is a binding that binds nothing. Validate enforces it.
type EnvBinding struct {
	// Name is the variable as the guest sees it.
	Name string `json:"name"`
	// Value is a literal, for the non-secret configuration a profile wants to
	// set. It is not a credential: a credential has a ref.
	Value string `json:"value,omitempty"`
	// Ref is `secrets.<name>` or `env.<name>`. See ParseRef.
	Ref string `json:"ref,omitempty"`
	// OptIn holds a binding back until the user asks for it by name, in
	// BRIG_FORWARD_OPTIN or in BRIG_FORWARD_ENV. It is for a value that is
	// genuinely useful to some flows and too powerful to hand a sandbox by
	// default -- a refresh token, which is durable account access rather than
	// the short-lived credential beside it. deny: is the wrong tool for that:
	// it is the billing guard, one switch covers all of it, and a variable on
	// it may not be bound at all.
	OptIn bool `json:"optIn,omitempty"`
	// Refs is an ordered chain: the first element that resolves non-empty
	// wins. It is how one variable gets a shell override and a store fallback,
	// which BRIG_FORWARD_ENV cannot give it once a profile binds the name.
	// Exactly one of Value, Ref and Refs is set.
	//
	// Scoped to env: deliberately. A file has one source, so files: takes ref:
	// and never refs:.
	Refs []string `json:"refs,omitempty"`
}

// RefList is the binding's sources in order, with the singular ref: expanded
// into a chain of one, so every caller walks one shape.
func (b EnvBinding) RefList() []string {
	if len(b.Refs) > 0 {
		return b.Refs
	}
	if b.Ref == "" {
		return nil
	}
	return []string{b.Ref}
}

// Resolved reports where the binding's value comes from: the parsed ref, and
// whether there is one at all. A binding with a literal returns
// (Ref{}, false, nil).
func (b EnvBinding) Resolved() (Ref, bool, error) {
	if b.Ref == "" {
		return Ref{}, false, nil
	}
	r, err := ParseRef(b.Ref)
	if err != nil {
		return Ref{}, false, err
	}
	return r, true, nil
}
