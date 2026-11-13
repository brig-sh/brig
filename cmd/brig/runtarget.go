package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/wrap"
)

// `brig run --target <name>`: a run of one target of the Brigfile, gated by
// an approval you give outside the repository.
//
// The agent in a sandbox can edit the repository, the Brigfile and the
// policies it names included. So reading a Brigfile authorizes nothing. The
// run builds the subject of an approval from the one read of the Brigfile and
// the Config it resolved, and boots only when an approval you gave covers
// that subject: one stored in brig's state directory, the digest on the line,
// or a yes at the terminal. Nothing after the gate reads the repository
// again, so what boots is what was approved.

// approvalStdin is where the answer to the approval is read, and
// approvalTerminal whether there is somebody there to give it. Tests set
// both.
var (
	approvalStdin    = func() io.Reader { return os.Stdin }
	approvalTerminal = func() bool { return wrap.IsTerminal(os.Stdin) }
)

// runNeedsProfile is what a bare `brig run` says on a host with no Brigfile,
// as it did before a run could read one.
func runNeedsProfile(verb string) error {
	return fmt.Errorf("%s needs a profile, for example `brig %s claude`. "+
		"`brig agent ls` lists them", verb, verb)
}

// defaultTarget is the Brigfile of a run line that names no target, with the
// target it runs: the Brigfile's defaultTarget. A line with no Brigfile to
// read is what it was before a run could read one.
func defaultTarget(opts options) (*brigfile.Loaded, string, error) {
	if opts.file == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, "", err
		}
		if _, err := brigfile.Discover(wd); errors.Is(err, brigfile.ErrNotFound) {
			if opts.sessionGiven || opts.approveGiven || opts.recreate {
				return nil, "", usagef("--session, --approve and --recreate go with a target of a Brigfile, " +
					"and there is no Brigfile here. `brig run <profile>` runs a profile")
			}
			return nil, "", runNeedsProfile("run")
		}
	}
	l, err := loadBrigfile(opts.file)
	if err != nil {
		return nil, "", err
	}
	if l.Project.DefaultTarget == "" {
		return nil, "", usagef("%s names no defaultTarget, so `brig run` needs --target <name>. Its targets "+
			"are %s", l.Path, strings.Join(targetNamesOf(l), ", "))
	}
	return l, l.Project.DefaultTarget, nil
}

// runTarget is `brig run --target <name>`, and a bare `brig run` in a
// checkout whose Brigfile names a default target.
func runTarget(opts options, tail []string, wantJSON bool) error {
	switch {
	case opts.targetGiven && opts.target == "":
		return usagef("--target needs a target name")
	case opts.approveGiven && opts.approve == "":
		return usagef("--approve needs the digest of the approval, as the refusal prints it")
	}
	var l *brigfile.Loaded
	if !opts.targetGiven {
		var err error
		if l, opts.target, err = defaultTarget(opts); err != nil {
			return err
		}
	}
	tr, err := loadTarget("run", opts, l)
	if err != nil {
		return err
	}
	cfg := tr.cfg
	if jsonRun != nil {
		jsonRun.sandbox = cfg.VMName
	}
	t := cfg.Profile
	// After --, the arguments replace the target's own, all of them: a run
	// that wants fewer says so with fewer.
	args := tr.resolved.Args
	if opts.argsGiven {
		args = tail
	}
	if t.IsGUI() && len(args) > 0 {
		return fmt.Errorf("target %s runs a graphical agent, which takes no arguments", tr.resolved.Name)
	}
	if t.IsShell() && !opts.detach {
		if err := checkShellCommand(args); err != nil {
			return err
		}
	}
	// What the guest is started with, as handOver builds it.
	final := args
	if !t.IsShell() && !t.IsGUI() {
		final = agentArgs(cfg, t, args)
	}

	// A run brig refuses is nothing to approve. Asked first, with the class
	// the run would exit with.
	if err := cfg.Refusal(); err != nil {
		return err
	}
	d := cfg.Plan()
	if err := missingRequired(cfg, d); err != nil {
		return err
	}
	subject, _ := cfg.ApprovalSubject(wrap.ApprovalInput{
		Brigfile:  tr.loaded.Raw,
		Documents: targetDocuments(tr.resolved),
		Args:      final,
	})
	// A sandbox keeps what it was booted with. Asked before the approval, so
	// you are not asked to approve a run that is then refused. What changed
	// since your approval is said here too, since that is usually why.
	if changed, _ := cfg.ConfigChanged(); changed && !opts.recreate {
		if stored, have := wrap.Approved(cfg.VMName); have && stored.Digest != wrap.ApprovalDigest(subject) {
			sayChanges(stored, subject)
		}
		return fmt.Errorf("the sandbox of %s was booted with another configuration, and brig does not "+
			"start this one on it. `%s --recreate` boots it again on the same guest home, and `%s` "+
			"removes it and its home", tr.ref, tr.ref.command("run"), tr.ref.command("rm"))
	}
	if err := approve(cfg, tr, subject, d, final, opts, wantJSON); err != nil {
		return err
	}
	if err := cfg.Admit(subject); err != nil {
		return err
	}

	// After the approval: the look a later command compares against is
	// taken now, and the recreate removes a sandbox.
	cfg.CheckProjectGit()
	if opts.recreate {
		if _, recorded := wrap.LookupProject(cfg.VMName); recorded {
			if err := cfg.Recreate(); err != nil {
				return err
			}
		}
	}
	// BuildEnv reads secret values and writes into the guest home, so it
	// waits for the approval too.
	set, err := cfg.BuildEnv()
	if err != nil {
		return err
	}
	if showsEnvelope("run", verbosity) {
		cfg.PrintPreRunEnvelope(set)
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return err
	}
	// Here, between the boot and the handover, which does not return. The
	// entry owns the session's guest home from now on.
	if ps, ok := cfg.ProjectSession(); ok {
		if err := wrap.RecordProject(ps); err != nil {
			warnf("could not record the session of %s: %v", tr.ref, err)
		}
	}
	return handOver(cfg, set, t, args, opts.detach, jsonRun != nil)
}

// targetDocuments are the documents a target reads through its Brigfile:
// its profile and its policies, each with the bytes the loader read.
func targetDocuments(rt brigfile.ResolvedTarget) []wrap.ApprovalSource {
	docs := []wrap.ApprovalSource{{Kind: "profile", Alias: rt.Profile.Alias,
		Source: rt.Profile.Source.String(), Bytes: rt.Profile.Doc}}
	for _, p := range rt.Policies {
		docs = append(docs, wrap.ApprovalSource{Kind: "policy", Alias: p.Alias, Source: p.Source.String(),
			Bytes: p.Doc})
	}
	return docs
}

// missingRequired refuses a run whose profile needs a secret the store does
// not hold, before it is approved. The plan lists the store and reads no
// value. A missing secret is never advised as an import: import fills a
// profile of yours by that name, and the name here is the repository's.
func missingRequired(cfg *wrap.Config, d wrap.PlanDocument) error {
	var missing []creds.Missing
	for _, cr := range d.Credentials {
		if !cr.Required || cr.State != creds.StateUnresolved || cr.Secret == "" {
			continue
		}
		missing = append(missing, creds.Missing{Name: cr.Secret, Required: true, Reason: secret.ErrNotFound})
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return &creds.MissingSecretsError{Sandbox: cfg.VMName, Missing: missing}
}

// approve decides whether the run of subject may boot. It returns nil when
// an approval covers it, and an ApprovalRequiredError when none does.
//
// The order is the order of the guarantees. A digest on the line must be
// this run's, whatever is stored. A stored approval of this digest boots
// with no question. Otherwise the plan is shown, and only a yes at a
// terminal approves it: no terminal, --json, or an answer other than yes is
// a refusal that names the digest.
func approve(cfg *wrap.Config, tr *targetRun, subject wrap.ApprovalSubject, d wrap.PlanDocument,
	final []string, opts options, wantJSON bool) error {
	digest := wrap.ApprovalDigest(subject)
	stored, have := wrap.Approved(cfg.VMName)
	if opts.approveGiven {
		if opts.approve != digest {
			showApproval(cfg, tr, subject, d, final, stored, have)
			return &wrap.ApprovalRequiredError{Err: fmt.Errorf("--approve %s is not the approval of this run "+
				"of %s, whose digest is %s. Read the plan above, and pass that digest if you approve it",
				opts.approve, tr.ref, digest)}
		}
		record(cfg, subject)
		return nil
	}
	if have && stored.Digest == digest {
		return nil
	}
	showApproval(cfg, tr, subject, d, final, stored, have)
	if wantJSON || cfg.NoTerminal || !approvalTerminal() {
		why := "there is no terminal to ask on"
		if wantJSON {
			why = "a run under --json asks nothing"
		}
		return &wrap.ApprovalRequiredError{Err: fmt.Errorf("%s is not approved to run as shown above, and %s. "+
			"To approve it, run the same line with --approve %s", tr.ref, why, digest)}
	}
	wrap.Stderr.Ask("run " + tr.ref.String() + " as shown above?")
	line, err := readAnswer(approvalStdin())
	if err != nil {
		// EOF is the answer a closed stdin gives, and it is not yes.
		return &wrap.ApprovalRequiredError{Err: fmt.Errorf("not approved: nothing was started. To approve "+
			"this run without a question, pass --approve %s", digest)}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		record(cfg, subject)
		return nil
	}
	return &wrap.ApprovalRequiredError{Err: errors.New("not approved: nothing was started")}
}

// record stores the approval. A store brig cannot write costs the next run
// a question, not this one, which you just approved.
func record(cfg *wrap.Config, subject wrap.ApprovalSubject) {
	if err := wrap.Approve(cfg.VMName, subject); err != nil {
		warnf("could not record the approval, so the next run asks again: %v", err)
	}
}

// showApproval prints, on stderr, what a run of a target asks you to
// approve: the rows of its plan with the arguments the agent gets, the
// approval digest, and, when you approved this session before, each part
// that changed since.
func showApproval(cfg *wrap.Config, tr *targetRun, subject wrap.ApprovalSubject, d wrap.PlanDocument,
	final []string, stored wrap.ApprovalRecord, have bool) {
	if d.Target != nil {
		d.Target.Args = final
	}
	head := notice.Newf("%s in %s asks to run with this configuration", tr.ref, tr.loaded.Root).
		Note("the Brigfile and the files it names are the repository's, and the agent can change them").
		Note("nothing runs until you approve it")
	if !subject.Image.Pinned {
		head.Note("the image is named by a tag, which can point at other bytes after you approve")
	}
	wrap.Stderr.Say(head.String())
	cfg.PrintApproval(os.Stderr, d, wrap.ApprovalDigest(subject))
	if have {
		sayChanges(stored, subject)
	}
}

// sayChanges names, on stderr, each part of subject that differs from the
// approval stored.
func sayChanges(stored wrap.ApprovalRecord, subject wrap.ApprovalSubject) {
	block := notice.Newf("this differs from what you approved on %s", stored.At.Local().Format(time.DateTime))
	for _, c := range wrap.ApprovalChanges(stored.Subject, subject) {
		block.Note("%s", c)
	}
	wrap.Stderr.Say(block.String())
}
