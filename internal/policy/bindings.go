package policy

import (
	"fmt"
	"sort"

	"github.com/brig-sh/brig/internal/profile"
)

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
		// A profile's own policy: list is not deduplicated at the source
		// (internal/profile.Validate only checks each name is well-formed),
		// so the same name listed twice must not become two identical
		// entries here.
		seen := map[string]bool{}
		for _, name := range p.Policy {
			if seen[name] {
				continue
			}
			seen[name] = true
			bound[name] = append(bound[name], p.Name+" (inline)")
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
		for _, policyName := range a.Profiles[profileName] {
			bound[policyName] = append(bound[policyName], profileName)
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
			for _, policyName := range a.Sessions[profileName][session] {
				bound[policyName] = append(bound[policyName], fmt.Sprintf("%s -n %s", profileName, session))
			}
		}
	}
	return bound, nil
}
