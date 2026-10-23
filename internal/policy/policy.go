// Package policy reads policy documents, binds them to runs, and folds the
// policies bound to one run into what that run gets.
//
// The document itself is api/policy/v1alpha1. This package decodes both of
// its spellings into that one type: the kind: Policy envelope, and the
// retired flat form, which has name, desc and egress at the top level. A
// caller above this package sees one shape.
//
// A policy holds egress rules, brokered routes, or both. This package keeps
// the presets that routes name, and expands them. Nothing serves a route
// yet, so internal/wrap refuses a run bound to one.
package policy

import "github.com/brig-sh/brig/api/policy/v1alpha1"

// APIVersion is the only apiVersion a policy document may declare. Parse
// refuses anything else, rather than guessing what an unrecognised version
// means.
const APIVersion = v1alpha1.APIVersion

// Policy is one policy document, as data.
type Policy = v1alpha1.Policy

// Egress is a policy's outbound rule set.
type Egress = v1alpha1.Egress

// Rule is one egress destination.
type Rule = v1alpha1.Rule

// CheckName reports whether name is safe to use as a policy name -- a bare
// word in a shell command and in a file path -- rather than the reason it is
// not. Exported so a command that builds a path from a name before a
// document exists to validate (create, for one) can reject a bad one before
// touching the filesystem.
//
// The rule lives in v1alpha1.CheckName, which validates metadata.name.
func CheckName(name string) error {
	return v1alpha1.CheckName(name)
}
