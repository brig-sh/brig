package policy

import (
	"fmt"
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
func Bindings(dir string) (map[string][]string, error) {
	bound := map[string][]string{}

	// profile.All() already returns its slice sorted by name.
	for _, p := range profile.All() {
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
