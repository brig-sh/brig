package profile

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/secret"
)

// validateBindings checks the requirement list and the bindings against each
// other. The deprecated forward: list needs no arm of its own here: Parse
// folds it into Env before Validate ever runs, so by the time this sees a
// profile there is exactly one mechanism to check.
func (p Profile) validateBindings() error {
	declared := make(map[string]bool, len(p.Secrets))
	for _, name := range p.Secrets {
		if err := secret.ValidName(name); err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
		if declared[name] {
			return fmt.Errorf("secrets lists %q twice", name)
		}
		declared[name] = true
	}

	bound := make(map[string]bool, len(p.Env))
	for _, b := range p.Env {
		if b.Name == "" {
			return fmt.Errorf("an env entry has no name")
		}
		if strings.ContainsAny(b.Name, "= \t\n") {
			return fmt.Errorf("env name %q may not hold =, a space or a newline", b.Name)
		}
		if bound[b.Name] {
			return fmt.Errorf("env binds %s twice", b.Name)
		}
		bound[b.Name] = true

		// An explicitly empty value: is indistinguishable from an absent one in
		// a plain string, so it counts as absent. Binding it empty would shadow
		// a value baked into the image anyway, which is the rule forwarding has
		// always applied.
		hasValue, hasRef := b.Value != "", b.Ref != ""
		switch {
		case hasValue && hasRef:
			return fmt.Errorf("env %s has both value and ref; a binding has one source", b.Name)
		case !hasValue && !hasRef:
			return fmt.Errorf("env %s has neither value nor ref; use `value:` for a "+
				"literal or `ref: %s.<name>` for a secret", b.Name, NamespaceSecrets)
		}

		if p.Denied(b.Name) {
			return fmt.Errorf("%s is bound in env and listed in deny", b.Name)
		}

		if hasRef {
			r, _, err := b.Resolved()
			if err != nil {
				return fmt.Errorf("env %s: %w", b.Name, err)
			}
			// Checked here rather than at resolution: the requirement list is
			// what makes the missing-secret error complete, so a ref that
			// bypassed it would be a secret nothing ever checks for.
			if r.Namespace == NamespaceSecrets && !declared[r.Name] {
				return fmt.Errorf("env %s refs %s, which is not in secrets: -- add %q "+
					"to secrets: so brig checks for it before creating the sandbox",
					b.Name, r, r.Name)
			}
		}
	}

	return nil
}
