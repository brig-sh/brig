package policy

import (
	"fmt"
	"slices"
	"sort"

	"github.com/brig-sh/brig/internal/profile"
)

// InlineSuffix marks a Bindings entry that comes from a profile's own
// inline policy: list, rather than an attach. A caller that needs to tell
// the two apart -- removePolicy deciding whether "detach it" is even
// possible advice -- checks for this suffix rather than hardcoding its own
// copy of it, so the two stay in sync by construction, not by convention.
const InlineSuffix = " (inline)"

// Bindings maps a policy name to every place that binds it: an inline
// policy: entry in a profile's own file, a profile-level attach, or a
// session-level attach -- in that order, and each already sorted by name,
// so a caller printing this does not change shape from one run to the next
// just because a map iterated differently.
//
// Lives beside Attachments, rather than in the cmd package that prints it,
// because it is Attachments' own shape (Profiles keyed by profile,
// Sessions keyed by profile then session) that this inverts; a caller
// should not have to know that shape to ask "what binds this policy."
//
// The profiles to read inline policy: lists from are passed in rather than
// fetched from the registry, the same way CheckCoverage and
// EffectivePolicies take the profile they judge. A caller that had not
// loaded the registry yet would otherwise get an answer with every inline
// binding silently missing -- and removePolicy would then delete a policy
// a profile declares inline without the refusal that exists to stop it.
// Passing them makes that impossible to get wrong by accident.
//
// Sorted here rather than trusted from the caller, on a copy so the
// caller's slice is left alone: the order profiles are walked in is the
// order their names appear in each entry, which is what keeps the printed
// line identical from one run to the next.
func Bindings(dir string, profiles []profile.Profile) (map[string][]string, error) {
	bound := map[string][]string{}

	byName := slices.Clone(profiles)
	sort.Slice(byName, func(i, j int) bool { return byName[i].Name < byName[j].Name })
	for _, p := range byName {
		seen := map[string]bool{}
		for _, name := range p.Policy {
			recordBinding(bound, seen, name, p.Name+InlineSuffix)
		}
	}

	a, err := LoadAttachments(dir)
	if err != nil {
		return nil, err
	}
	profileNames := make([]string, 0, len(a.Profiles))
	for name := range a.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		seen := map[string]bool{}
		for _, policyName := range a.Profiles[profileName] {
			recordBinding(bound, seen, policyName, profileName)
		}
	}

	sessionProfiles := make([]string, 0, len(a.Sessions))
	for name := range a.Sessions {
		sessionProfiles = append(sessionProfiles, name)
	}
	sort.Strings(sessionProfiles)
	for _, profileName := range sessionProfiles {
		sessions := make([]string, 0, len(a.Sessions[profileName]))
		for session := range a.Sessions[profileName] {
			sessions = append(sessions, session)
		}
		sort.Strings(sessions)
		for _, session := range sessions {
			seen := map[string]bool{}
			for _, policyName := range a.Sessions[profileName][session] {
				recordBinding(bound, seen, policyName, fmt.Sprintf("%s -n %s", profileName, session))
			}
		}
	}
	return bound, nil
}

// Orphaned reports every name that bound (as Bindings returns it) binds
// something to, but that entries (as LoadAll returns it, for the same dir)
// does not declare -- sorted by name.
//
// This happens on purpose: --force on rm, or on an edit's rename, lets a
// bound name stop declaring a policy at all rather than refuse, and both
// say so when they do it. Nothing reports it afterward, though, so a
// listing is the one place the leftover would ever surface again.
//
// Takes bound and entries rather than a dir, so a caller that already has
// both -- listPolicies always does, to print the ordinary listing -- does
// not pay for a second Bindings and LoadAll pass just to ask this.
func Orphaned(bound map[string][]string, entries map[string]Entry) []string {
	var orphaned []string
	for name := range bound {
		if _, ok := entries[name]; !ok {
			orphaned = append(orphaned, name)
		}
	}
	sort.Strings(orphaned)
	return orphaned
}

// recordBinding appends binder to bound[policyName], once. seen is fresh
// per source list -- a profile's own policy: list, one profile's
// attachments, or one session's -- and guards against a duplicate name
// within that one list. AttachToProfile and AttachToSession dedupe on
// write and internal/profile.Validate does not check policy: for
// duplicates, so this is the one place both are caught.
func recordBinding(bound map[string][]string, seen map[string]bool, policyName, binder string) {
	if seen[policyName] {
		return
	}
	seen[policyName] = true
	bound[policyName] = append(bound[policyName], binder)
}
