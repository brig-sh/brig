package profile

import "fmt"

// normaliseForward folds the deprecated Forward list into Env, so an in-memory
// profile carries exactly one mechanism for getting a variable into the guest.
//
// Same treatment, and for the same reason, as normaliseKind gives shell: and
// gui:. `forward: [X]` means "carry X in from brig's own environment when it is
// set", which is exactly `env: [{name: X, ref: env.X}]` -- so the translation is
// mechanical and nothing below the parser has to know which spelling the file
// used.
//
// Translated entries are appended after any explicit Env, so a report reads in
// the order the file declares. A variable in both is an error rather than a
// precedence rule: either answer would surprise whoever wrote both. A repeat
// within Forward alone is not that case -- it is harmless today, and a file
// with no env: block at all would otherwise fail with an error about a
// collision that does not exist, so repeats are deduped silently instead.
func (p *Profile) normaliseForward() error {
	if len(p.Forward) == 0 {
		return nil
	}
	explicit := make(map[string]bool, len(p.Env))
	for _, b := range p.Env {
		explicit[b.Name] = true
	}
	seen := make(map[string]bool, len(p.Forward))
	for _, name := range p.Forward {
		if seen[name] {
			continue
		}
		seen[name] = true
		if explicit[name] {
			return fmt.Errorf("%s is in forward and also bound in env; drop it from "+
				"forward, which env replaces", name)
		}
		p.Env = append(p.Env, EnvBinding{Name: name, Ref: NamespaceEnv + "." + name})
	}
	p.Forward = nil
	return nil
}
