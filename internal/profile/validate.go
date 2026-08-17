package profile

import (
	"fmt"
	"strings"

	"github.com/brig-sh/brig/internal/secret"
)

// validateBindings checks the requirement list and the two binding channels
// against each other. The deprecated forward: list needs no arm of its own:
// Parse folds it into Env before Validate runs, so by the time this sees a
// profile there is exactly one mechanism per channel to check.
func (p Profile) validateBindings() error {
	declared, err := p.validateSecrets()
	if err != nil {
		return err
	}
	// validateFiles arrives with the files: schema in PR 6.
	return p.validateEnv(declared)
}

// validateSecrets checks the requirement list and returns the declared names.
func (p Profile) validateSecrets() (map[string]bool, error) {
	declared := make(map[string]bool, len(p.Secrets))
	for _, d := range p.Secrets {
		if err := secret.ValidName(d.Name); err != nil {
			return nil, fmt.Errorf("secrets: %w", err)
		}
		if declared[d.Name] {
			return nil, fmt.Errorf("secrets lists %q twice", d.Name)
		}
		declared[d.Name] = true

		// Both spellings at once has no meaning to pick between, and guessing
		// one would silently ignore the other.
		if d.From != "" && len(d.Sources) > 0 {
			return nil, fmt.Errorf("secret %q has both from: and sources:; the singular "+
				"from: IS a one-element sources:, so use one", d.Name)
		}
		// An empty list is not "no sources" -- that spelling already exists,
		// by omitting the key. Written out, it is somebody who meant to fill
		// it in.
		if d.Sources != nil && len(d.Sources) == 0 {
			return nil, fmt.Errorf("secret %q has an empty sources:; omit it for a "+
				"hand-created secret", d.Name)
		}
		for _, s := range d.SourceList() {
			if err := validateSource(d.Name, s); err != nil {
				return nil, err
			}
		}
	}
	return declared, nil
}

// validateSource checks that a source carries exactly the locator its from:
// requires. A `from: keychain` with a `path:` is a mistake rather than
// something to ignore: ignoring it reads as a working portable chain and
// resolves nothing.
func validateSource(name string, s Source) error {
	want, extra := "", []string{}
	switch s.From {
	case SourceKeychain:
		want, extra = "service", []string{"path", "var"}
	case SourceFile:
		want, extra = "path", []string{"service", "var"}
	case SourceEnv:
		want, extra = "var", []string{"service", "path"}
	case "":
		return fmt.Errorf("a source of secret %q has no from: (%s, %s or %s)",
			name, SourceKeychain, SourceFile, SourceEnv)
	default:
		return fmt.Errorf("secret %q names source %q, which is not %s, %s or %s",
			name, s.From, SourceKeychain, SourceFile, SourceEnv)
	}
	// The misplaced locator is reported before the missing one, because a
	// source carrying the wrong key is missing the right one by construction
	// and "no service:" would name the symptom while `path:` sits there
	// naming the cause.
	for _, other := range extra {
		switch other {
		case "service":
			if s.Service != "" {
				return fmt.Errorf("secret %q has a %s source carrying service:, which only "+
					"%s takes", name, s.From, SourceKeychain)
			}
		case "path":
			if s.Path != "" {
				return fmt.Errorf("secret %q has a %s source carrying path:, which only "+
					"%s takes", name, s.From, SourceFile)
			}
		case "var":
			if s.Var != "" {
				return fmt.Errorf("secret %q has a %s source carrying var:, which only "+
					"%s takes", name, s.From, SourceEnv)
			}
		}
	}
	if s.locatorValue() == "" {
		return fmt.Errorf("secret %q has a %s source with no %s:", name, s.From, want)
	}
	return nil
}

// validateEnv checks the environment channel, including chains.
func (p Profile) validateEnv(declared map[string]bool) error {
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
		hasValue := b.Value != ""
		hasRef, hasRefs := b.Ref != "", len(b.Refs) > 0
		switch {
		case hasRef && hasRefs:
			return fmt.Errorf("env %s has both ref: and refs:; ref: IS a refs: of length "+
				"one, so use one", b.Name)
		case hasValue && (hasRef || hasRefs):
			return fmt.Errorf("env %s has both value and ref; a binding has one source", b.Name)
		case !hasValue && !hasRef && !hasRefs:
			return fmt.Errorf("env %s has neither value nor ref; use `value:` for a "+
				"literal or `ref: %s.<name>` for a secret", b.Name, NamespaceSecrets)
		}

		if p.Denied(b.Name) {
			return fmt.Errorf("%s is bound in env and listed in deny", b.Name)
		}

		for _, raw := range b.RefList() {
			r, err := ParseRef(raw)
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
