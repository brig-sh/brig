package brigfile

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
)

// SourceKind is the form an alias was declared in.
type SourceKind string

// The three forms. Only a profile is ever SourceBuiltin.
const (
	SourceBuiltin SourceKind = "builtin"
	SourceFile    SourceKind = "file"
	SourceInline  SourceKind = "inline"
)

// Source is where a resolved document came from, apart from the alias that
// names it: two aliases can name one file, and one alias can move to another
// file between two versions of a repository.
type Source struct {
	Kind SourceKind
	// Ref is the profile's name for SourceBuiltin and the path relative to
	// the project root for SourceFile. It is empty for SourceInline.
	Ref string
}

// String is the source as a person reads it: builtin:claude-code,
// file:.brig/x.yaml or inline.
func (s Source) String() string {
	if s.Kind == SourceInline {
		return string(s.Kind)
	}
	return string(s.Kind) + ":" + s.Ref
}

// Repository reports whether the repository wrote the document, as opposed
// to brig shipping it.
func (s Source) Repository() bool { return s.Kind != SourceBuiltin }

// ResolvedProfile is one profile alias, resolved.
type ResolvedProfile struct {
	Alias  string
	Source Source
	// Profile is the parsed profile. For a repository profile, Profile.Policy
	// is empty and Policies holds what it listed: those names are aliases in
	// this Brigfile, and a run that resolved Profile.Policy against the
	// reader's installed policies would attach a different policy that
	// happens to share a name. A built-in profile keeps its policy: list for
	// the run to resolve as it does today.
	Profile profile.Profile
	// Policies are the policy aliases a repository profile listed under
	// policy:, in its order.
	Policies []string
	// Doc is the document the profile was parsed from: the file's bytes, the
	// embedded spec, or the inline mapping as the decoder handed it over, in
	// JSON. An inline document's YAML is in Loaded.Raw.
	Doc []byte
}

// ResolvedPolicy is one policy alias, resolved.
type ResolvedPolicy struct {
	Alias  string
	Source Source
	Policy policy.Policy
	// Doc is the document the policy was parsed from, as for ResolvedProfile.
	Doc []byte
}

// ResolvedTarget is one target with its aliases resolved.
type ResolvedTarget struct {
	Name    string
	Profile ResolvedProfile
	// Policies are the target's own policies, then those a repository
	// profile lists, each alias once, in that order.
	Policies []ResolvedPolicy
	// Overrides is what the target changes. Nothing has applied it: the
	// profile above is as declared, and applying it is the run's business.
	Overrides Overrides
	// Args are the target's default guest arguments.
	Args []string
}

// resolve parses every profile and policy the Brigfile names, and checks what
// needs a parsed document to check.
func (l *Loaded) resolve(root *os.Root) error {
	for _, alias := range sortedKeys(l.Project.Policies) {
		a := l.Project.Policies[alias]
		src, doc := Source{Kind: SourceInline}, []byte(a.Inline)
		if a.File != "" {
			src = Source{Kind: SourceFile, Ref: a.File}
			blob, err := l.read(root, a.File)
			if err != nil {
				return fmt.Errorf("policies.%s.file: %w", alias, err)
			}
			doc = blob
		}
		pol, err := policy.Parse(doc)
		if err != nil {
			return fmt.Errorf("policies.%s: %s: %w", alias, src, err)
		}
		l.Policies[alias] = ResolvedPolicy{Alias: alias, Source: src, Policy: pol, Doc: doc}
	}
	for _, alias := range sortedKeys(l.Project.Profiles) {
		a := l.Project.Profiles[alias]
		var rp ResolvedProfile
		var err error
		switch {
		case a.From != "":
			rp, err = builtin(alias, strings.TrimPrefix(a.From, builtinScheme))
		case a.File != "":
			blob, rerr := l.read(root, a.File)
			if rerr != nil {
				return fmt.Errorf("profiles.%s.file: %w", alias, rerr)
			}
			rp, err = repository(alias, Source{Kind: SourceFile, Ref: a.File}, blob)
		default:
			rp, err = repository(alias, Source{Kind: SourceInline}, a.Inline)
		}
		if err != nil {
			return err
		}
		for _, name := range rp.Policies {
			if _, ok := l.Project.Policies[name]; !ok {
				return fmt.Errorf("profiles.%s: %s: policy: %q is not an alias under policies", alias, rp.Source, name)
			}
		}
		l.Profiles[alias] = rp
	}
	for _, name := range sortedKeys(l.Project.Targets) {
		t := l.Project.Targets[name]
		p := l.Profiles[t.Profile]
		if len(t.Policies) == 0 {
			continue
		}
		// The rule `brig policy attach` holds a profile to. A policy on a
		// shell or gui profile is never enforced, so a target that gives one
		// policies is refused rather than run unprotected.
		if err := policy.CheckCoverage(p.Profile); err != nil {
			return fmt.Errorf("targets.%s: %w, so the target cannot list policies", name, err)
		}
	}
	return nil
}

// builtin resolves builtin:<name> against the profiles brig ships, never
// against the reader's own profile directory: the same Brigfile has to mean
// the same profile on every machine.
func builtin(alias, name string) (ResolvedProfile, error) {
	p, blob, ok := profile.BuiltIn(name)
	if !ok {
		if canonical, isAlias := profile.Alias(name); isAlias {
			return ResolvedProfile{}, fmt.Errorf("profiles.%s.from: brig ships no profile %q; write builtin:%s",
				alias, name, canonical)
		}
		return ResolvedProfile{}, fmt.Errorf("profiles.%s.from: brig ships no profile %q", alias, name)
	}
	return ResolvedProfile{Alias: alias, Source: Source{Kind: SourceBuiltin, Ref: name}, Profile: p, Doc: blob}, nil
}

// repository resolves a profile the repository wrote, from a file or inline.
func repository(alias string, src Source, doc []byte) (ResolvedProfile, error) {
	where := "profiles." + alias + "." + string(src.Kind)
	if src.Kind == SourceFile {
		where = fmt.Sprintf("profiles.%s.file: %s", alias, src.Ref)
	}
	p, err := parseRepositoryProfile(doc)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("%s: %w", where, err)
	}
	own := p.Policy
	p.Policy = nil
	return ResolvedProfile{Alias: alias, Source: src, Profile: p, Policies: own, Doc: doc}, nil
}

// parseRepositoryProfile parses a profile a repository wrote, and refuses
// what a repository may not set.
//
// profile.Parse matches a key to a field without regard to case and reads
// only the first document, which is right for a file the reader keeps in
// their own profile directory. A repository profile is held to the rule a
// Brigfile is: one document, exact keys. The host fields are checked on the
// decoded struct before profile.Parse validates it, so a refusal says which
// field it was and not whatever Validate trips on first.
func parseRepositoryProfile(doc []byte) (profile.Profile, error) {
	var probe profile.Profile
	if err := v1alpha1.UnmarshalStrict(doc, &probe); err != nil {
		return profile.Profile{}, err
	}
	if err := refuseHostFields(probe); err != nil {
		return profile.Profile{}, err
	}
	p, err := profile.Parse(doc)
	if err != nil {
		return profile.Profile{}, err
	}
	// Checked again on what profile.Parse decoded, which is what a caller
	// gets, in case its decode and the strict one ever disagree.
	if err := refuseHostFields(p); err != nil {
		return profile.Profile{}, err
	}
	return p, nil
}

// refuseHostFields refuses each profile field that lets a repository name a
// host path, a host executable or a host lookup. Everything a repository
// profile may set describes the guest. These describe the reader's machine,
// and a repository that set them would choose what brig runs, copies or
// reads there.
//
// A repository profile may name a credential and bind it: secrets: [{name:
// x}] and a ref: secrets.x. Naming is something a later step can show the
// reader before a run. Where the value comes from on the host is the
// reader's to say, with their own profile or `brig secret create`.
func refuseHostFields(p profile.Profile) error {
	if p.RuntimeBin != "" {
		return fmt.Errorf("runtimeBin names a program on your host for brig to run; " +
			"a repository profile may not set it")
	}
	if p.HostConfigDir != "" {
		return fmt.Errorf("hostConfigDir names a directory on your host that brig copies into " +
			"the guest; a repository profile may not set it")
	}
	if len(p.ProjectPaths) > 0 {
		return fmt.Errorf("projectPaths names directories on your host that brig copies into " +
			"the guest; a repository profile may not set it")
	}
	// Not a host field, but refused for what it would do on the host: see
	// imageRef. profile.Parse only asks that an image is set.
	if !imageRef(p.Image) {
		return fmt.Errorf("image: %q is not an image reference", p.Image)
	}
	for i, s := range p.Secrets {
		for _, f := range []struct {
			field string
			set   bool
		}{
			{"sources", len(s.Sources) > 0},
			{"from", s.From != ""}, {"service", s.Service != ""}, {"path", s.Path != ""},
			{"var", s.Var != ""}, {"hint", s.Hint != ""},
			{"field", s.Field != ""}, {"expiryField", s.ExpiryField != ""},
		} {
			if f.set {
				return fmt.Errorf("secrets[%d].%s: says how brig secret import reads the secret %q on "+
					"your host; a repository profile may name a secret, not where it comes from",
					i, f.field, s.Name)
			}
		}
	}
	for i, b := range p.Env {
		for _, ref := range b.RefList() {
			r, err := profile.ParseRef(ref)
			if err == nil && r.Namespace == profile.NamespaceEnv {
				return fmt.Errorf("env[%d]: %s reads brig's own environment on your host into %s in "+
					"the guest; a repository profile may bind a secret it names, with secrets.<name>",
					i, ref, b.Name)
			}
		}
	}
	for i, v := range p.Volumes {
		if v.Kind == profile.VolumeHostMount {
			return fmt.Errorf("volumes[%d]: the hostmount %s shares a directory on your host with "+
				"the guest and keeps it across runs; a repository profile may declare tmpfs volumes only",
				i, v.Path)
		}
	}
	return nil
}

// Target resolves one target by name. The result shares nothing with l or
// with an earlier result: each document is parsed again from the bytes Load
// read, so a caller that changes it changes only its own copy.
func (l *Loaded) Target(name string) (ResolvedTarget, error) {
	t, ok := l.Project.Targets[name]
	if !ok {
		return ResolvedTarget{}, fmt.Errorf("the Brigfile has no target %q", name)
	}
	p, err := l.fresh(l.Profiles[t.Profile])
	if err != nil {
		return ResolvedTarget{}, err
	}
	out := ResolvedTarget{
		Name:      name,
		Profile:   p,
		Overrides: t.Overrides.clone(),
		Args:      slices.Clone(t.Args),
	}
	seen := map[string]bool{}
	for _, alias := range append(slices.Clone(t.Policies), p.Policies...) {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		rp := l.Policies[alias]
		pol, err := policy.Parse(rp.Doc)
		if err != nil {
			return ResolvedTarget{}, fmt.Errorf("policies.%s: %w", alias, err)
		}
		rp.Policy = pol
		out.Policies = append(out.Policies, rp)
	}
	return out, nil
}

// fresh parses a resolved profile again from its document.
func (l *Loaded) fresh(rp ResolvedProfile) (ResolvedProfile, error) {
	if rp.Source.Kind == SourceBuiltin {
		return builtin(rp.Alias, rp.Source.Ref)
	}
	return repository(rp.Alias, rp.Source, rp.Doc)
}

func (o *Overrides) clone() Overrides {
	if o == nil {
		return Overrides{}
	}
	var out Overrides
	if o.CPUs != nil {
		v := *o.CPUs
		out.CPUs = &v
	}
	if o.Mem != nil {
		v := *o.Mem
		out.Mem = &v
	}
	if o.Image != nil {
		v := *o.Image
		out.Image = &v
	}
	return out
}
