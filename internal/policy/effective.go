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
	return NamesFor(p.Policy, p.Name, session, dir)
}

// NamesFor is EffectivePolicies with its three sources named apart: own, a
// profile's inline list; what attach bound in dir under the profile name
// attachKey; and, when session is non-empty, what attach bound to that
// session of attachKey.
//
// It is apart from EffectivePolicies because the key is not always the
// name of the profile that runs. A repository's Brigfile can name its
// profile anything, claude-code included, and attachments are the reader's:
// a run of a Brigfile target asks for the attachments of a profile brig
// ships only, under that profile's own name, and never under a name the
// repository chose.
func NamesFor(own []string, attachKey, session, dir string) ([]string, error) {
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
	add(own)
	add(a.Profiles[attachKey])
	if session != "" {
		add(a.Sessions[attachKey][session])
	}
	return names, nil
}
