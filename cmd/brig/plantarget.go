package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/telemetry"
	"github.com/brig-sh/brig/internal/wrap"
)

// `brig plan --target <name>`: the plan of a run of one Brigfile target.
//
// A Brigfile is the repository's, and the agent in a sandbox can edit the
// repository. The plan is where what it asks for becomes visible to the
// person who would run it, so it is built from the same Config a run would
// boot and says where each part came from.
//
// The glue lives here and not in internal/wrap or internal/brigfile. brigfile
// may not link wrap (see its arch_test.go), and wrap takes plain values: an
// identity, a set of policies and what the plan shows. Everything that knows
// what a Brigfile is stays on this side of that line.

// checkTargetLine refuses a bare word on a line of plan, stop or rm that
// names a target. The target says which session and the Brigfile's directory
// is the project, so a profile or a project on the same line is a second
// answer to each.
//
// It walks the line the way split does, so a flag's value is never read as
// the word.
func checkTargetLine(verb string, args []string) error {
	target := false
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
			continue
		}
		name, _, inline := strings.Cut(a, "=")
		if name == "--target" {
			target = true
		}
		if mine, takesValue := oursFor(verb, a, posRun); mine && takesValue && !inline {
			i++
		}
	}
	if !target || len(words) == 0 {
		return nil
	}
	if verb == "plan" {
		return usagef("unexpected argument %q; `brig plan --target` plans a target of the Brigfile, "+
			"whose directory is the project, so it takes no profile and no project. "+
			"Drop --target to plan a profile", words[0])
	}
	return usagef("unexpected argument %q; `brig %s --target` %s a session of a target of the "+
		"Brigfile, so it takes no profile. Drop --target to %s a profile's session",
		words[0], verb, lifecycleVerbs[verb], verb)
}

// lifecycleVerbs is what stop and rm do to a session, as a message says it.
var lifecycleVerbs = map[string]string{"stop": "stops", "rm": "removes"}

// refuseTarget refuses --target and --session on a verb that reaches a
// session by its ref alone. Only the flags before the first bare word are
// read: after the ref of sh they are the guest command's.
func refuseTarget(verb string, args []string) error {
	var advice string
	switch verb {
	case "info":
		advice = "`brig plan --target <name>` shows what a run of a target gets"
	case "sh", "exec", "network ls", "network publish", "network unpublish":
		advice = "`brig stop --target` and `brig rm --target` reach a session of a target"
	default:
		return nil
	}
	for _, a := range args {
		if a == "--" || !strings.HasPrefix(a, "-") {
			return nil
		}
		name, _, _ := strings.Cut(a, "=")
		if name == "--target" || name == "--session" {
			return usagef("`brig %s` takes a ref and does not read a Brigfile target. %s", verb, advice)
		}
	}
	return nil
}

// targetRun is one target of the Brigfile resolved into the Config a run of
// it boots, with the snapshot it was resolved from. plan and run build it the
// same way, so the plan a reader approves is the run that boots.
type targetRun struct {
	// loaded is the one read of the Brigfile and the files it names. Nothing
	// after loadTarget reads the repository again: the approval covers these
	// bytes, and a second read would boot what the guest wrote since.
	loaded   *brigfile.Loaded
	resolved brigfile.ResolvedTarget
	target   *wrap.Target
	cfg      *wrap.Config
	ref      projectRef
}

// loadTarget resolves the target the line names into the Config a run of it
// boots. It looks for the runtime, lists the store and asks the runtime what
// `brig plan <profile>` asks it, and writes nothing. l is the Brigfile when
// the caller has read it already, so that one read is all there is.
func loadTarget(verb string, opts options, l *brigfile.Loaded) (*targetRun, error) {
	switch {
	case opts.target == "":
		return nil, usagef("--target needs a target name")
	case opts.load.Workspace != "":
		return nil, usagef("--home and --target ask for different things: a target's guest home is " +
			"brig's own, named after the project and the target")
	case opts.load.NoProject:
		return nil, usagef("--no-project and --target ask for different things: a target's project is " +
			"the directory that holds its Brigfile")
	}
	if l == nil {
		var err error
		if l, err = loadBrigfile(opts.file); err != nil {
			return nil, err
		}
	}
	rt, err := resolveTarget(l, opts.target)
	if err != nil {
		return nil, err
	}
	p, base := applyOverrides(rt)
	set, err := targetPolicies(rt)
	if err != nil {
		return nil, err
	}
	// A policy on a shell or a gui profile is never enforced. The loader
	// checks a target's own policies; this holds the whole set to the same
	// rule, a repository profile's own list and a built-in's attachments
	// included.
	if len(set.Docs) > 0 {
		if err := policy.CheckCoverage(p); err != nil {
			return nil, fmt.Errorf("target %s: %w, so it cannot carry policies", rt.Name, err)
		}
	}
	// Resolved once, here. Leaving the names on the profile would have Load
	// or a later reader resolve them again, against your policy directory.
	p.Policy = nil

	builtIn := rt.Profile.Source.Kind == brigfile.SourceBuiltin
	// The Brigfile says whether the profile is one brig ships, not the
	// registry: a repository profile may call itself claude-code.
	telemetry.SetAgent(p.Name, builtIn)
	if p.Unpublished && opts.load.Image == "" {
		return nil, fmt.Errorf("we do not publish an image for %q, so there is nothing to boot. Set "+
			"overrides.image on target %s, or name one with --image", p.Name, rt.Name)
	}

	// Only a profile brig ships has settings of its own. A repository's
	// profile is read with the global ones alone, so the name it chose
	// reaches none of yours.
	envAgent := ""
	if builtIn {
		envAgent = rt.Profile.Source.Ref
	}
	id := wrap.ProjectIdentity(l.Root, rt.Name, opts.session, envAgent)
	target := &wrap.Target{
		Name:          rt.Name,
		Session:       opts.session,
		Args:          rt.Args,
		Brigfile:      l.Path,
		Root:          l.Root,
		ProfileAlias:  rt.Profile.Alias,
		ProfileSource: rt.Profile.Source.String(),
		BuiltIn:       builtIn,
		Base:          base,
		RmCommand:     projectRef{target: rt.Name, session: opts.session, file: opts.file}.command("rm"),
	}
	if file := ignoredUserFile(rt.Profile); file != "" {
		name := rt.Profile.Source.Ref
		target.Notes = append(target.Notes, fmt.Sprintf("%s overrides the built-in %s for `brig run %s`.\n"+
			"This Brigfile says %s, so brig ignores that file for this target.", file, name, name,
			rt.Profile.Source))
	}

	// A built-in binds your credentials for the image brig ships with it. A
	// target that keeps the built-in and names another image hands the same
	// bindings to an image the repository chose, including a variable read
	// from your environment, which a profile the repository wrote may not
	// ask for at all. The override is allowed, so the plan says it outright.
	if rt.Profile.Source.Kind == brigfile.SourceBuiltin && rt.Overrides.Image != nil {
		note := fmt.Sprintf("The image %s is this repository's choice, not the one brig ships for %s.",
			*rt.Overrides.Image, rt.Profile.Source.Ref)
		if refs := bindingRefs(rt.Profile.Profile); len(refs) > 0 {
			note += fmt.Sprintf("\nIt receives what %s binds: %s.", rt.Profile.Source.Ref, strings.Join(refs, ", "))
		}
		target.Notes = append(target.Notes, note)
	}

	runner, err := detectRuntimeFor(runtime.Preference{Bin: p.RuntimeBin})
	if err != nil {
		// As `brig plan <profile>`: a runtime that is not on PATH is a row
		// marked unavailable, and any other failure is the reader's to fix.
		// A run needs the runtime, so it stops here either way.
		if verb != "plan" || !errors.Is(err, runtime.ErrNoRuntime) {
			return nil, err
		}
		runner = nil
	}
	sayFallback(runner)
	o := opts.load
	o.Name = ""
	o.Project = l.Root
	o.Identity = &id
	o.Policies = &set
	o.Target = target
	o.Verbosity = verbosity
	cfg, err := wrap.Load(p, o, runner)
	if err != nil {
		return nil, err
	}
	cfg.OpenStore = func() (creds.SecretReader, error) { return openStore() }
	return &targetRun{
		loaded:   l,
		resolved: rt,
		target:   target,
		cfg:      cfg,
		ref:      projectRef{root: l.Root, target: rt.Name, session: opts.session, file: opts.file},
	}, nil
}

// planTarget prints the plan of the target the line names. It writes
// nothing.
func planTarget(opts options, wantJSON bool) error {
	tr, err := loadTarget("plan", opts, nil)
	if err != nil {
		return err
	}
	cfg := tr.cfg
	// A sandbox keeps what it was booted with. When the Brigfile has changed
	// what this target boots since, the plan says that this is not what the
	// running sandbox has.
	if changed, _ := cfg.ConfigChanged(); changed {
		tr.target.Notes = append(tr.target.Notes, "This session's sandbox was booted with a different "+
			"configuration, so it does not run what this plan shows.\n`"+tr.ref.command("rm")+
			"` removes it and its guest home.")
	}
	d := cfg.Plan()
	if wantJSON {
		return writeJSONDocument(cfg.Out, "Plan", d)
	}
	cfg.PrintPlan(d)
	return nil
}

// applyOverrides returns the target's profile with its overrides applied, and
// where each value came from before any flag or setting. Applied to the
// profile, so Load's own order, flag then setting then this, is the order a
// target's values follow too.
func applyOverrides(rt brigfile.ResolvedTarget) (profile.Profile, wrap.Origins) {
	p := rt.Profile.Profile
	from := fmt.Sprintf("profile %s (%s)", rt.Profile.Alias, rt.Profile.Source)
	byTarget := "target " + rt.Name
	base := wrap.Origins{Image: from, Mem: from, CPUs: from, Network: from}
	if o := rt.Overrides; o.Image != nil {
		p.Image, base.Image = *o.Image, byTarget
		// An image of the target's own is published by whoever named it.
		p.Unpublished = false
	}
	if o := rt.Overrides; o.Mem != nil {
		p.Mem, base.Mem = *o.Mem, byTarget
	}
	if o := rt.Overrides; o.CPUs != nil {
		p.CPUs, base.CPUs = *o.CPUs, byTarget
	}
	return p, base
}

// bindingRefs is every source a profile's env and file bindings read, each
// once, in the profile's order: secrets.<name> and env.<NAME>.
func bindingRefs(p profile.Profile) []string {
	var out []string
	add := func(ref string) {
		if ref != "" && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	for _, b := range p.Env {
		for _, ref := range b.RefList() {
			add(ref)
		}
	}
	for _, f := range p.Files {
		add(f.Ref)
	}
	return out
}

// targetPolicies is every policy a target's run is bound to, each with where
// it came from: the target's policies from the Brigfile, in its order, and,
// for a profile brig ships, that profile's own policy: list and what you
// attached to it, read from your policy directory.
//
// Attachments are looked up under the built-in's own name only. A repository
// profile gets none, whatever it calls itself, and attachments to a session
// do not apply: a target has no session name of yours.
//
// Two policies here can share a metadata.name, one from the Brigfile and one
// of yours. Both apply, and the plan names where each came from.
func targetPolicies(rt brigfile.ResolvedTarget) (wrap.PolicySet, error) {
	var set wrap.PolicySet
	for _, rp := range rt.Policies {
		set.Docs = append(set.Docs, rp.Policy)
		set.Origins = append(set.Origins, fmt.Sprintf("brigfile %s (%s)", rp.Alias, rp.Source))
	}
	if rt.Profile.Source.Kind != brigfile.SourceBuiltin {
		return set, nil
	}
	name, own := rt.Profile.Source.Ref, rt.Profile.Profile.Policy
	dir := policy.Dir()
	names, err := policy.NamesFor(own, name, "", dir)
	if err != nil {
		return wrap.PolicySet{}, err
	}
	docs, names, err := policy.LoadNamed(names, name, dir)
	if err != nil {
		return wrap.PolicySet{}, err
	}
	for i, doc := range docs {
		from := "attachment to " + name
		if slices.Contains(own, names[i]) {
			from = "builtin profile " + name
		}
		set.Docs = append(set.Docs, doc)
		set.Origins = append(set.Origins, from)
	}
	return set, nil
}
