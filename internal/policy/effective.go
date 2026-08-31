package policy

import "github.com/brig-sh/brig/internal/profile"

// EffectivePolicies is the union of every policy that applies to a run of
// p: its own inline policy: list, whatever attach has bound to p in dir's
// attachments.yaml, and -- when session is non-empty -- whatever attach
// has bound to that session name. Deduplicated by name, inline first, then
// the profile's own attachments, then the session's, each in the order it
// was declared or attached; a name already seen from an earlier source is
// not repeated.
//
// This does not check that any of the names it returns still exist as a
// policy, or that p can enforce them -- see CheckCoverage for that. It
// only answers "what is bound here," the question attach, detach and
// check all need the same answer to.
func EffectivePolicies(p profile.Profile, session string, dir string) ([]string, error) {
	a, err := LoadAttachments(dir)
	if err != nil {
		return nil, err
	}

	var names []string
	seen := map[string]bool{}
	add := func(list []string) {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	add(p.Policy)
	add(a.Profiles[p.Name])
	if session != "" {
		add(a.Sessions[p.Name][session])
	}
	return names, nil
}
