package main

import (
	"fmt"
	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"os"
	"path/filepath"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/wrap"
)

// `brig stop --target` and `brig rm --target`: the lifecycle verbs on a
// session of a Brigfile target.
//
// They do not load the Brigfile. A session outlives the file that named it:
// the repository can delete its Brigfile or drop the target, and its sandbox
// and home must still be reachable. The sandbox name is built from the
// project root, the target and the session name alone, so those three find
// it, and the project index and the runtime confirm it is one brig started.

// checkTargetSession refuses a session name that is not already its own slug,
// on the rule a ref's label follows: the name is part of the sandbox's
// identity, so a name brig would have to rewrite addresses a session nobody
// typed.
func checkTargetSession(name string) error {
	slug := session.Slug(name)
	switch {
	case slug == "":
		return usagef("--session %q has no usable characters. Session names use lowercase letters, "+
			"digits, dot, dash and underscore", name)
	case slug != name:
		return usagef("--session %q is not usable as it stands: it would have to become %q. Session "+
			"names use lowercase letters, digits, dot, dash and underscore. Type %q if that is the "+
			"session you want", name, slug, slug)
	}
	return nil
}

// projectRoot is the root of the project the line names: the directory of
// the Brigfile -f names, or the project of the working directory. It is the
// root the loader would give the Brigfile there, with its links resolved, and
// it is found without reading the Brigfile, which may be gone.
func projectRoot(file string) (string, error) {
	if file == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return brigfile.Root(wd)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("the directory of %s: %w", file, err)
	}
	return root, nil
}

// projectRef is a session of a target as the messages name it.
type projectRef struct {
	root, target, session, file string
}

func (p projectRef) String() string {
	if p.session != "" {
		return "session " + p.session + " of target " + p.target
	}
	return "target " + p.target
}

// command is the line that runs verb on this session again.
func (p projectRef) command(verb string) string {
	line := "brig " + verb + " --target " + p.target
	if p.session != "" {
		line += " --session " + p.session
	}
	if p.file != "" {
		line += " -f " + shellWord(p.file)
	}
	return line
}

// notFound is the answer when neither the project index nor the runtime has
// the session's sandbox. It names the root and the target it looked for, so
// a reader in the wrong directory sees which project brig took this for.
func (p projectRef) notFound(sandbox string) error {
	return notFoundf("no %s in the project %s: brig has no record of its sandbox %s, "+
		"and the runtime has none. `brig ls` lists the sessions brig knows", p, p.root, sandbox)
}

// projectLifecycle is `brig stop --target` and `brig rm --target`.
func projectLifecycle(verb string, opts options, dryRun bool) error {
	switch {
	case opts.target == "":
		return usagef("--target needs a target name")
	case v1alpha1.CheckName(opts.target) != nil:
		// The Brigfile is not read here, so this is the one check on the
		// name: a word no Brigfile accepts as a target names no session.
		return usagef("--target %q is not a target name. Target names use lowercase letters, digits, "+
			"dot, dash and underscore, and start with a letter or digit", opts.target)
	case opts.load.Workspace != "":
		return usagef("--home and --target ask for different things: a target's guest home is " +
			"brig's own, named after the project and the target")
	}
	root, err := projectRoot(opts.file)
	if err != nil {
		return err
	}
	// The index and the homes are what these verbs act on. With brig's state
	// inside the project the guest could have written either.
	if err := wrap.CheckProjectState(root); err != nil {
		return err
	}
	ref := projectRef{root: root, target: opts.target, session: opts.session, file: opts.file}
	id := wrap.ProjectIdentity(root, opts.target, opts.session, "")
	entry, indexed := wrap.LookupProject(id.Sandbox)
	if !indexed {
		entry = wrap.ProjectSession{Sandbox: id.Sandbox, Root: root, Target: opts.target, Session: opts.session}
	}

	// A target's profile has no runtimeBin: a repository profile may not set
	// one, and the profiles brig ships set none. So the runtime is the one
	// on PATH, the one a run of the target uses.
	rt, err := detectRuntimeFor(runtime.Preference{})
	if err != nil {
		return err
	}
	sayFallback(rt)
	cfg := wrap.ProjectConfig(entry, rt, verbosity)

	if !indexed {
		present, err := projectSandboxPresent(rt, id.Sandbox)
		if err != nil {
			return err
		}
		if !present {
			return ref.notFound(id.Sandbox)
		}
	}
	// As `brig stop <ref>` and `brig rm <ref>` do: a change the guest made to
	// the project's .git is named before the reader follows with git.
	wrap.CheckSandboxGit(id.Sandbox, verbosity)
	if verb == "stop" {
		return cfg.Stop()
	}
	return removeProject(cfg, ref, dryRun)
}

// projectSandboxPresent reports whether the runtime has the sandbox, stopped
// or running. The listing comes first. A runtime whose listing can leave out
// a stopped sandbox is then asked for the one name.
func projectSandboxPresent(rt runtime.Runtime, name string) (bool, error) {
	present, err := sandboxPresent(rt, name)
	if err != nil || present {
		return present, err
	}
	if ex, ok := rt.(runtime.Exister); ok {
		return ex.Exists(name)
	}
	return false, nil
}

// removeProject is `brig rm --target`. It follows removeSandbox, with the
// project index in place of the session index: the sandbox goes, and with it
// the home and the entry. When the runtime reports no such sandbox, rm
// forgets the session and keeps the home, as `brig rm <ref>` does.
func removeProject(cfg *wrap.Config, ref projectRef, dryRun bool) error {
	present, err := projectSandboxPresent(cfg.Runtime, cfg.VMName)
	if err != nil {
		return err
	}
	if !present {
		if !dryRun {
			if err := revokeRoutes(cfg.VMName); err != nil {
				warnf("the credential broker could not revoke the session tokens of %s: %v", cfg.VMName, err)
			}
			if ports, err := runtime.Publications(cfg.VMName); err == nil && len(ports) > 0 {
				runtime.ForgetPublications(cfg.VMName)
				warnf("dropped the ports recorded for %s", ref)
			}
		}
		// Present is false only on a runtime that answered for the one name,
		// or that cannot answer for it. Only the first is an absence to act on.
		if _, ok := cfg.Runtime.(runtime.Exister); !ok {
			return notFoundf("no sandbox for %s. `brig ls` lists them", ref)
		}
		stays := kept(cfg, true)
		if dryRun {
			for _, n := range stays {
				warnf("%s", n)
			}
			return notFoundf("no sandbox for %s; the runtime reported none, so `%s` would forget its session",
				ref, ref.command("rm"))
		}
		if _, err := wrap.ForgetProject(cfg.VMName); err != nil {
			return fmt.Errorf("no sandbox for %s; the runtime reported none, but brig could not forget its "+
				"session: %w", ref, err)
		}
		_, _ = wrap.ForgetSandbox(cfg.VMName)
		runtime.ForgetBootedNet(cfg.VMName)
		wrap.ForgetSlugClaim(cfg.VMName)
		for _, n := range stays {
			warnf("%s", n)
		}
		return notFoundf("no sandbox for %s; the runtime reported none, so brig forgot its session", ref)
	}
	if dryRun {
		if home := wrap.EphemeralHomeOf(cfg.VMName); home != "" {
			fmt.Printf("would remove %s (sandbox %s) and its guest home %s\n", ref, cfg.VMName, home)
			for _, n := range kept(cfg, false) {
				warnf("%s", n)
			}
		} else {
			fmt.Printf("would remove %s (sandbox %s)\n", ref, cfg.VMName)
			for _, n := range kept(cfg, true) {
				warnf("%s", n)
			}
		}
		return nil
	}
	if err := cfg.Remove(); err != nil {
		return err
	}
	switch {
	case cfg.HomeErr != nil:
		for _, n := range kept(cfg, false) {
			warnf("%s", n)
		}
		return fmt.Errorf("removed %s, but %w", ref, cfg.HomeErr)
	case cfg.RemovedHome != "":
		warnf("%s", notes(notice.New("removed "+ref.String()+" and its guest home "+cfg.RemovedHome), kept(cfg, false)))
	default:
		warnf("%s", notes(notice.New("removed "+ref.String()), kept(cfg, true)))
	}
	return nil
}

// projectLabel is how a listing names the session of a project sandbox:
// its target, its session and its project, from the project index.
func projectLabel(name string) string {
	ps, ok := wrap.LookupProject(name)
	if !ok {
		return "a session of a Brigfile target that brig has no record of"
	}
	label := "target " + ps.Target
	if ps.Session != "" {
		label = "session " + ps.Session + " of " + label
	}
	return label + " in " + ps.Root
}

// brigfileHint says, once, that a run of a profile in a project with a
// Brigfile does not read it, and names the commands that do. A reader who
// added a Brigfile and then ran a profile would otherwise take the run for
// one of its targets.
//
// The project is the one the run mounts, or the working directory's when it
// names none. The hint looks only for the file: a Brigfile is the
// repository's to break, and a broken one must not break a run that never
// reads it. Every error is dropped. warnf keeps it off -q, and a run under
// --json does not call it, since a script reads that output.
func brigfileHint(project string) {
	dir := project
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return
		}
		dir = wd
	}
	root, err := brigfile.Root(dir)
	if err != nil {
		return
	}
	if _, err := os.Lstat(filepath.Join(root, brigfile.FileName)); err != nil {
		return
	}
	warnf("%s has a Brigfile, and `brig run <profile>` does not read it. `brig target ls` lists "+
		"its targets, and `brig plan --target <name>` shows what one gets", root)
}
