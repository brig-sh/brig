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
	if err := p.validateVolumes(); err != nil {
		return err
	}
	if err := p.validateFiles(declared); err != nil {
		return err
	}
	return p.validateEnv(declared)
}

// validateVolumes checks the mount list.
//
// The rules here are all about a configuration that reads as protection and
// is not one. That is the failure mode this list has: every entry looks
// plausible, and the ones that do nothing are indistinguishable from the ones
// that work unless brig says so at parse time.
func (p Profile) validateVolumes() error {
	if len(p.Volumes) > 0 && len(p.StatePaths) > 0 {
		return fmt.Errorf("volumes: and statePaths: are both set; volumes: is statePaths: " +
			"made load-bearing, so drop statePaths: -- two lists that can disagree about " +
			"what persists is worse than either")
	}
	seen := make(map[string]bool, len(p.Volumes))
	var tmpfs []Volume
	for _, v := range p.Volumes {
		switch v.Kind {
		case VolumeTmpfs, VolumeHostMount:
		case VolumeNamed:
			// Parsed and refused, rather than rejected as an unknown kind: a
			// profile written against the shared-volume case should fail with
			// the reason it fails, and land unchanged when it is implemented.
			return fmt.Errorf("volume %q is kind: %s, which is not yet supported -- "+
				"a named volume several sandboxes share has no implementation behind it "+
				"yet, so declaring one would persist nothing", v.Path, VolumeNamed)
		case "":
			return fmt.Errorf("a volume has no kind: (%s or %s)", VolumeTmpfs, VolumeHostMount)
		default:
			return fmt.Errorf("volume %q names kind %q, which is not %s or %s",
				v.Path, v.Kind, VolumeTmpfs, VolumeHostMount)
		}
		if err := guestPath(fmt.Sprintf("volume %q", v.Path), v.Path); err != nil {
			return err
		}
		if seen[v.Path] {
			return fmt.Errorf("volumes mounts %q twice; the second would cover the first",
				v.Path)
		}
		seen[v.Path] = true
		// source: is the reserved kind's field. On the two that ship it is
		// either a restatement of the implicit source or a redirection with
		// nothing behind it, and silently ignoring the latter is how a
		// profile ends up persisting somewhere its author did not name.
		if v.Source != "" {
			return fmt.Errorf("volume %q carries source:, which only kind: %s takes -- a %s "+
				"is bound from the same path in the workspace", v.Path, VolumeNamed, VolumeHostMount)
		}
		if v.File && v.Kind != VolumeHostMount {
			return fmt.Errorf("volume %q is kind: %s and sets file:, which only %s takes",
				v.Path, v.Kind, VolumeHostMount)
		}
		if v.Kind == VolumeTmpfs {
			tmpfs = append(tmpfs, v)
		}
	}
	for _, v := range p.Volumes {
		if v.Kind != VolumeHostMount {
			continue
		}
		if !nestedUnder(v.Path, tmpfs) {
			return fmt.Errorf("volume %q is a %s under no %s, so it would mount the "+
				"workspace onto itself -- the workspace is already guestHome. Either drop "+
				"it, or add the %s it is meant to punch a hole in",
				v.Path, VolumeHostMount, VolumeTmpfs, VolumeTmpfs)
		}
	}
	return nil
}

func nestedUnder(target string, list []Volume) bool {
	for _, v := range list {
		if Under(target, v.Path) {
			return true
		}
	}
	return false
}

// validateFiles checks the file channel.
//
// The target rule is the one that matters and it is fail-closed: a files:
// binding writes a credential, and the only reason it is safe to write one
// into the guest at all is that the directory it lands in cannot reach host
// disk. A binding whose target is not covered by a tmpfs -- or that is covered
// and then carved back out by a hostmount -- writes a live token into the
// workspace, which is the leak this whole design exists to close. Refused at
// parse time, where it is one message, rather than at delivery, where it is a
// file already on the host.
func (p Profile) validateFiles(declared map[string]bool) error {
	bound := make(map[string]bool, len(p.Files))
	for _, b := range p.Files {
		what := fmt.Sprintf("file %q", b.Path)
		if err := guestPath(what, b.Path); err != nil {
			return err
		}
		if bound[b.Path] {
			return fmt.Errorf("files writes %q twice; a file has one source", b.Path)
		}
		bound[b.Path] = true
		if _, err := b.FileMode(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if b.Ref == "" {
			return fmt.Errorf("%s has no ref:; use `ref: %s.<name>`", what, NamespaceSecrets)
		}
		r, err := ParseRef(b.Ref)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if r.Namespace != NamespaceSecrets {
			return fmt.Errorf("%s refs %s; a file is filled from brig's own store, so its "+
				"ref is %s.<name>", what, r, NamespaceSecrets)
		}
		if !declared[r.Name] {
			return fmt.Errorf("%s refs %s, which is not in secrets: -- add %q to secrets: "+
				"so brig checks for it before creating the sandbox", what, r, r.Name)
		}
		if !p.EphemeralPath(b.Path) {
			return fmt.Errorf("%s writes a credential to a path that reaches host disk -- "+
				"add `volumes: [{kind: %s, path: <a directory above it>}]` so the bytes stay "+
				"in memory, and make sure no %s under it carves the target back out",
				what, VolumeTmpfs, VolumeHostMount)
		}
	}
	return nil
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
