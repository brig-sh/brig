package policy

import (
	"fmt"
	"sort"

	"github.com/brig-sh/brig/internal/profile"
)

// Resolve is what a boot needs: every policy bound to this run, loaded and
// folded into one rule set, with the names it came from.
//
// A bound name that does not load is an error rather than a policy skipped.
// The names are what someone attached; a run that carried on without one would
// be a sandbox reported as covered and enforcing less than was asked for,
// which is the failure this whole path exists to prevent. `brig policy check`
// reports the same condition before a boot rather than during one.
func Resolve(p profile.Profile, session, dir string) (Egress, []string, error) {
	names, err := EffectivePolicies(p, session, dir)
	if err != nil {
		return Egress{}, nil, err
	}
	if len(names) == 0 {
		return Egress{}, nil, nil
	}
	// LoadAll returns what it could read alongside its error: a file that does
	// not parse, or two files claiming one name, is reported without stopping
	// the rest from loading. So the map is read either way, and only a name
	// this run is actually bound to can fail it. Treating the error as fatal
	// let one unrelated document in the directory -- an editor backup renamed
	// to .yaml, a half-written file -- refuse every policy-bound boot, while
	// `brig policy ls` and `brig policy check` kept working and gave no hint
	// where it came from.
	entries, loadErr := LoadAll(dir)
	var (
		policies []Policy
		missing  []string
	)
	for _, name := range names {
		entry, ok := entries[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		policies = append(policies, entry.Policy)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		// The load error, when there was one, is the likeliest reason a bound
		// name did not turn up -- that document is probably the one that failed
		// to parse -- so it travels with the refusal instead of being dropped.
		because := ""
		if loadErr != nil {
			because = ": " + loadErr.Error()
		}
		return Egress{}, nil, fmt.Errorf("policy %v is attached to %s but no document of that "+
			"name loads from %s; `brig policy check %s` says which, and `brig policy ls` what "+
			"is there%s", missing, p.Name, dir, p.Name, because)
	}
	return Merge(policies), names, nil
}

// Merge folds every policy that applies to one run into a single rule set.
//
// A run can carry several: a profile's inline policy: list, whatever attach
// bound to the profile, and whatever attach bound to the session. They were
// written separately and nothing coordinates them, so this has to say what
// several policies at once mean.
//
// The rules are unioned. Attaching two policies is a request for both, and a
// rule in either is a rule of the run's -- which is what a reader expects from
// "this sandbox has the base policy and the extra one". Note what that costs:
// a name allowed by the second is reachable even though the first alone would
// have denied it. Attaching is granting.
//
// The default is the strictest asked for. One deny among a set of allows makes
// the whole run deny-by-default, because the alternative is that adding a
// restrictive policy to a permissive one leaves the permissive one in charge,
// which is the wrong direction to be wrong in.
//
// Deny still wins over Allow at the gateway, so a deny rule in any policy in
// the set holds against an allow rule in any other. That ordering is the
// enforcer's; this only decides what reaches it.
func Merge(ps []Policy) Egress {
	if len(ps) == 0 {
		return Egress{}
	}
	merged := Egress{Default: "allow"}
	for _, p := range ps {
		if p.Egress.Default == "deny" {
			merged.Default = "deny"
		}
		merged.Allow = append(merged.Allow, p.Egress.Allow...)
		merged.Deny = append(merged.Deny, p.Egress.Deny...)
	}
	return dedupe(merged)
}

// dedupe drops a rule that two policies both carry. Two identical --egress-allow
// flags mean nothing more than one, and a reader comparing what brig reports
// against what they wrote should not have to account for the repetition.
func dedupe(e Egress) Egress {
	e.Allow = unique(e.Allow)
	e.Deny = unique(e.Deny)
	return e
}

func unique(rules []Rule) []Rule {
	if len(rules) == 0 {
		return nil
	}
	seen := make(map[Rule]bool, len(rules))
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
