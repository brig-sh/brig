package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/session"
	"sigs.k8s.io/yaml"
)

// policyUsage is what `brig policy --help` prints.
const policyUsage = `brig policy -- manage egress policies and what they bind

usage:
  brig policy ls                                    every policy, and what binds it
  brig policy create <name> [--force]               write a starter, then open it
  brig policy edit <name> [--force]                 open yours in $VISUAL or $EDITOR
  brig policy show <name> [--json]                  print the parsed document
  brig policy rm <name> [--force]                   delete it
  brig policy attach <policy> <profile> [-n NAME]   bind it to a profile, or one session
  brig policy detach <policy> <profile> [-n NAME]   reverse an attach
  brig policy check <profile> [-n NAME]             what is bound, and whether it can enforce anything

flags:
  -f, --force   with create: replace a file already there
                with edit: save a rename that would orphan a binding
                with rm: delete one that is bound to something
      --json    with show: render JSON rather than YAML
  -n, --name    with attach/detach/check: one session by name, not every run

A policy is a named document declaring what an agent may reach outbound.
attach binds it to a profile's every run, or -- with -n -- to one session;
a profile's own inline policy: field does the same without a separate attach.
`

// policyCmd groups the policy verbs.
func policyCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("policy needs a subcommand: ls, create, edit, show, rm, attach, detach or check")
	}
	var err error
	switch args[0] {
	case "--help", "-h", "help":
		fmt.Print(policyUsage)
		return nil
	case "ls":
		err = listPolicies()
	// list was never in the help text, so it was found by accident and then
	// scripted. Kept for one release, saying which spelling to keep.
	case "list":
		deprecated("brig policy list", "brig policy ls")
		err = listPolicies()
	case "create":
		err = createPolicy(args[1:])
	case "edit":
		err = editPolicy(args[1:])
	case "show":
		err = showPolicy(args[1:])
	case "rm":
		err = removePolicy(args[1:])
	case "attach":
		err = attachPolicy(args[1:])
	case "detach":
		err = detachPolicy(args[1:])
	case "check":
		err = checkPolicy(args[1:])
	default:
		return fmt.Errorf("unknown policy subcommand %q (ls, create, edit, show, rm, attach, detach, check)", args[0])
	}
	// A verb's own parser reports --help as an error, because that is how
	// the flag package says it. Asking for help is not a mistake, so it is
	// answered with the help and an exit code of zero, the same translation
	// profileCmd and secretCmd make.
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(policyUsage)
		return nil
	}
	return err
}

// loadPolicies calls LoadAll and reports it the same way for every caller:
// a directory it could not even read comes back as an error, so a caller
// does not fall through to a message that means "empty" or "not found" when
// the truth is brig could not look. Anything softer -- one bad file, a
// duplicate name -- is a non-nil map with the same failures LoadAll always
// reported, printed here once rather than in every caller.
func loadPolicies(dir string) (map[string]policy.Entry, error) {
	entries, err := policy.LoadAll(dir)
	if entries == nil {
		return nil, err
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
	}
	return entries, nil
}

// listPolicies prints every policy that parses, by name and description,
// and -- for one that is bound to anything -- what binds it.
//
// A directory holding one policy that fails to parse still lists the
// others: LoadAll reports the failure on stderr and returns everything that
// did load.
func listPolicies() error {
	entries, err := loadPolicies(policy.Dir())
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	// A policy.Bindings failure -- an attachments.yaml that is unreadable
	// or fails to parse -- is not this command's to fail over: listing
	// policies never depended on that file before attach existed, and a
	// broken attachments.yaml is exactly the kind of thing someone would
	// still want `brig policies` to work while diagnosing. Warn and list
	// without the bound-to lines rather than print nothing at all.
	bound, err := policy.Bindings(policy.Dir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
	}
	for _, name := range names {
		fmt.Printf("%-15s %s\n", name, entries[name].Policy.Desc)
		if b := bound[name]; len(b) > 0 {
			fmt.Printf("                bound to: %s\n", strings.Join(b, ", "))
		}
	}
	if len(names) == 0 {
		fmt.Printf("no policies yet; your own live in %s\n", policy.Dir())
		fmt.Printf("brig policy create <name> writes a starter one\n")
	}
	return nil
}

// lookupPolicy finds one policy by name, or says which command lists what
// exists. Feeds show, edit and rm alike.
func lookupPolicy(name string) (policy.Entry, error) {
	entries, err := loadPolicies(policy.Dir())
	if err != nil {
		return policy.Entry{}, err
	}
	e, ok := entries[name]
	if !ok {
		return policy.Entry{}, fmt.Errorf("unknown policy %q. `brig policy ls` lists them", name)
	}
	return e, nil
}

// parseWords runs fs against args, lifting each bare word past whatever
// flags fs defines, so `create no-net --force` and `create --force no-net`
// -- either order anyone actually types -- parse the same. Parse alone
// stops at the first bare word and would otherwise leave a flag that comes
// after it sitting unparsed in Args.
//
// verb and takes name the subcommand and the flags it accepts, both only
// for the error an unrecognised flag produces.
func parseWords(verb, takes string, fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			msg := err.Error()
			if flagName, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(flagName)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return nil, fmt.Errorf("%s (%s takes %s)", msg, verb, takes)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return words, nil
}

// showPolicy prints the parsed document for one policy: YAML by default,
// --json for anything consuming it programmatically.
func showPolicy(args []string) error {
	asJSON := false
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.BoolVar(&asJSON, "json", false, "")
	words, err := parseWords("show", "--json", fs, args)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return errors.New("policy show needs a name, for example `brig policy show no-net`")
	}
	if len(words) > 1 {
		return fmt.Errorf("policy show takes one name, not %q", words[1])
	}
	entry, err := lookupPolicy(words[0])
	if err != nil {
		return err
	}
	var blob []byte
	if asJSON {
		blob, err = json.MarshalIndent(entry.Policy, "", "  ")
		blob = append(blob, '\n')
	} else {
		blob, err = yaml.Marshal(entry.Policy)
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(blob)
	return err
}

// parseNameAndForce pulls one name and an optional --force/-f out of args,
// working the same way around a bare word landing after the flag that
// showPolicy and parseAttachArgs also do. verb names the subcommand, for
// its error messages.
func parseNameAndForce(verb string, args []string) (name string, force bool, err error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	words, err := parseWords(verb, "--force", fs, args)
	if err != nil {
		return "", false, err
	}
	if len(words) == 0 {
		return "", false, fmt.Errorf("policy %s needs a name, for example `brig policy %s no-net`", verb, verb)
	}
	if len(words) > 1 {
		return "", false, fmt.Errorf("policy %s takes one name, not %q", verb, words[1])
	}
	return words[0], force, nil
}

// createPolicy writes a starter policy document, then opens it in your
// editor: $VISUAL, then $EDITOR, then vi.
func createPolicy(args []string) error {
	name, force, err := parseNameAndForce("create", args)
	if err != nil {
		return err
	}
	if err := policy.CheckName(name); err != nil {
		return err
	}
	// The file this name would produce is one the policy directory keeps
	// for something else, so writing it would destroy that file's contents
	// and the policy would be invisible anyway -- LoadAll skips the name.
	// Refused regardless of --force: forcing is for replacing a policy you
	// own, not for overwriting brig's own bookkeeping.
	if policy.IsReservedName(name) {
		return fmt.Errorf("%q is reserved: brig keeps %s.yaml in the policy directory for its own "+
			"records, so a policy cannot be called that. Pick another name", name, name)
	}

	dir := policy.Dir()
	path := filepath.Join(dir, name+".yaml")

	// The name lives inside the file, not in its filename, so a name taken
	// by some other file is refused here regardless of --force: forcing
	// would just leave two files declaring the same name, which is the
	// problem this check exists to prevent.
	entries, err := loadPolicies(dir)
	if err != nil {
		return err
	}
	if existing, ok := entries[name]; ok && existing.Path != path {
		return fmt.Errorf("policy %q already exists, declared in %s. Edit it directly with "+
			"`brig policy edit %s`, or remove that file first", name, existing.Path, name)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A starter document is generated bytes with nothing worth keeping, but
	// the file it would replace might not be, so create refuses to
	// overwrite one without --force.
	mode := os.FileMode(0o644)
	existed := false
	if info, err := os.Stat(path); err == nil {
		existed = true
		mode = info.Mode().Perm()
	}
	if !force && existed {
		return fmt.Errorf("%s already exists. Edit it directly with `brig policy edit %s`, "+
			"or pass --force to replace it with a fresh starter", path, name)
	}

	// The starter is written beside path, not over it, and only moved into
	// place once the save is known to parse. path is never touched until
	// that rename -- whether it is about to be created for the first time
	// or --force is replacing what is already there -- so a failing editor
	// or an unparseable save can never destroy real content that --force
	// would otherwise have overwritten before the editor even ran.
	tmp, err := os.CreateTemp(dir, ".brig-policy-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.WriteString(policyStarter(name)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	editor := editorCommand()
	cmd := exec.Command(editor[0], append(editor[1:], tmpPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("%s: %w", editor[0], err)
	}

	if _, err := readPolicyFile(tmpPath); err != nil {
		// Not written: path is exactly as it was before create ran (or
		// still absent). The draft is left at tmpPath, named in the
		// error, so it is not lost -- only not yet valid.
		return fmt.Errorf("%s was not written: %w\nyour edit is still at %s",
			path, err, tmpPath)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	fmt.Printf("%s created\n", path)
	return nil
}

// policyStarter is a minimal, valid policy document to start editing from.
func policyStarter(name string) string {
	return fmt.Sprintf(`apiVersion: %s
name: %s
desc:
egress:
  default: deny
  allow:
    - host: api.anthropic.com
`, policy.APIVersion, name)
}

// editPolicy opens an existing policy in a scratch copy, and only replaces
// the file with what you saved if the copy still parses and validates: the
// real file is untouched until that check passes, rather than edited in
// place and reported afterwards.
func editPolicy(args []string) error {
	name, force, err := parseNameAndForce("edit", args)
	if err != nil {
		return err
	}
	entry, err := lookupPolicy(name)
	if err != nil {
		return err
	}

	original, err := os.ReadFile(entry.Path)
	if err != nil {
		return err
	}
	// The system temp directory, not policy.Dir(): a scratch copy that
	// failed to clean up would otherwise sit in the policy directory with a
	// name isPolicyFile matches, and every later `brig policy ls` would
	// report it as a broken policy rather than leftover editor debris.
	scratch, err := os.CreateTemp("", "brig-policy-edit-*"+filepath.Ext(entry.Path))
	if err != nil {
		return err
	}
	scratchPath := scratch.Name()
	if _, err := scratch.Write(original); err != nil {
		scratch.Close()
		os.Remove(scratchPath)
		return err
	}
	if err := scratch.Close(); err != nil {
		os.Remove(scratchPath)
		return err
	}

	editor := editorCommand()
	cmd := exec.Command(editor[0], append(editor[1:], scratchPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(scratchPath)
		return fmt.Errorf("%s: %w", editor[0], err)
	}

	edited, err := readPolicyFile(scratchPath)
	if err != nil {
		// Not saved: the real file is exactly as it was before edit ran.
		// The scratch copy is left in place, named in the error, so the
		// edit itself is not lost -- only not yet valid.
		return fmt.Errorf("not saved, %s is unchanged: %w\nyour edit is still at %s",
			entry.Path, err, scratchPath)
	}
	// The name lives inside the file, so a rename can collide with a name
	// some other file already declares. entry.Path still holds its old
	// name in others below, which is what lets a name unchanged by the
	// edit, or a rename to one nothing else uses, pass cleanly.
	others, err := loadPolicies(policy.Dir())
	if err != nil {
		return err
	}
	if other, ok := others[edited.Name]; ok && other.Path != entry.Path {
		return fmt.Errorf("not saved, %s is unchanged: name %q is already declared in %s\n"+
			"your edit is still at %s", entry.Path, edited.Name, other.Path, scratchPath)
	}
	// The same refusal create makes. This file is not the reserved one, so
	// saving the rename would destroy nothing -- but it would leave a
	// policy under a name create cannot produce and brig keeps for its own
	// records, reachable only because it came in through edit. Two ways in
	// with different rules is the shape of a bug either way.
	if policy.IsReservedName(edited.Name) {
		return fmt.Errorf("not saved, %s is unchanged: %q is reserved -- brig keeps %s.yaml in "+
			"the policy directory for its own records, so a policy cannot be called that\n"+
			"your edit is still at %s", entry.Path, edited.Name, edited.Name, scratchPath)
	}
	// A rename leaves whatever named entry.Policy.Name (an attach, or a
	// profile's own inline policy: entry) pointing at a name this file no
	// longer declares. A name unchanged by the edit needs no such check --
	// the file it is bound to is still right here.
	if !force && edited.Name != entry.Policy.Name {
		b, err := boundTo(entry.Policy.Name)
		if err != nil {
			return err
		}
		if len(b) > 0 {
			return fmt.Errorf("not saved, %s is unchanged: renaming %s to %s would leave %s "+
				"pointing at a name nothing declares. %s first, or pass --force to rename it anyway\n"+
				"your edit is still at %s",
				entry.Path, entry.Policy.Name, edited.Name, strings.Join(b, ", "), howToUnbind(b), scratchPath)
		}
	}
	editedBytes, err := os.ReadFile(scratchPath)
	if err != nil {
		return err
	}
	// A direct write truncates entry.Path before the new bytes land, so a
	// full disk or a kill between those two steps leaves it empty or half
	// written -- exactly what the scratch copy above exists to prevent,
	// one step later. Write beside it instead and rename over it: same
	// directory, so the same filesystem, so the rename is atomic -- the
	// file is always the old content or the new one, never a partial
	// write. The name does not end in .yaml/.yml/.json, so a rename that
	// never happens leaves debris isPolicyFile ignores, not a policy
	// brig policy ls reports as broken.
	//
	// os.WriteFile never changed entry.Path's mode -- an existing file
	// keeps whatever it already had, and only a newly created one takes
	// the mode passed in. A rename replaces the file outright, so the
	// temp file's own mode has to be set to match by hand, or a policy
	// someone had chmoded down would come back world-readable.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(entry.Path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(entry.Path), ".brig-policy-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(editedBytes); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, entry.Path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	os.Remove(scratchPath)
	fmt.Printf("%s updated\n", entry.Path)
	return nil
}

// removePolicy deletes a policy's file. It refuses one that is bound to
// anything -- an inline policy: entry, a profile-level attach, or a
// session-level attach -- unless --force: the file would be gone but
// whatever named it would still be pointing at nothing.
//
// Bindings failing (an attachments.yaml that is unreadable or fails to
// parse) refuses too, deliberately unlike listPolicies' warn-and-degrade:
// listing is read-only and a partial answer is still useful, but rm is
// destructive, and brig cannot tell you it is safe to delete something
// when it cannot read the record of what points at it. --force skips the
// check outright, the same as it does when the record reads fine.
func removePolicy(args []string) error {
	name, force, err := parseNameAndForce("rm", args)
	if err != nil {
		return err
	}
	entry, err := lookupPolicy(name)
	if err != nil {
		return err
	}
	if !force {
		b, err := boundTo(name)
		if err != nil {
			return err
		}
		if len(b) > 0 {
			return fmt.Errorf("%s is bound to %s. %s first, or pass --force to remove it anyway",
				name, strings.Join(b, ", "), howToUnbind(b))
		}
	}
	if err := os.Remove(entry.Path); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", entry.Path)
	return nil
}

// boundTo is what a name is bound to, for a caller about to make that name
// stop declaring a policy (rm deleting the file, edit renaming it) and
// needing to refuse first if anything still points at it.
func boundTo(name string) ([]string, error) {
	bound, err := policy.Bindings(policy.Dir())
	if err != nil {
		return nil, err
	}
	return bound[name], nil
}

// howToUnbind tells removePolicy's refusal what to say to actually clear
// b: "detach it" is wrong advice for an inline entry, since detach
// explicitly refuses to touch one -- policy.InlineSuffix is what tells one
// apart from an attach, the same mark policy.Bindings used to print it.
func howToUnbind(b []string) string {
	var inline, attached bool
	for _, x := range b {
		if strings.HasSuffix(x, policy.InlineSuffix) {
			inline = true
		} else {
			attached = true
		}
	}
	switch {
	case attached && inline:
		return "Detach it and edit the profile's policy: list"
	case inline:
		return "Edit the profile's policy: list"
	default:
		return "Detach it"
	}
}

// readPolicyFile reads and parses one policy file, for the re-check `create`
// and `edit` run after the editor closes.
func readPolicyFile(path string) (policy.Policy, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return policy.Policy{}, err
	}
	return policy.Parse(blob)
}

// sessionGivenEmpty reports whether -n or --name was given on the command
// line, registered on fs, with an empty value -- `-n ""`, or `-n
// "$SESSION"` with an unset $SESSION. That is not the same as -n omitted,
// and must not be read as one: silently falling through to the
// profile-wide (or whole-profile check) path below would attach, detach
// or check far more than a caller asking for one session meant.
func sessionGivenEmpty(fs *flag.FlagSet, session string) bool {
	if session != "" {
		return false
	}
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "n" || f.Name == "name" {
			given = true
		}
	})
	return given
}

// checkSessionName refuses a -n value that cannot name a session brig would
// actually start.
//
// An attachment is an address: it says which session a policy binds, and a
// session's sandbox and workspace are named from its slug (see
// internal/wrap/config.go). A value Slug would rewrite therefore addresses
// a session nobody typed -- `-n "My Work"` would record "My Work" while
// `brig run -n "My Work"` starts my-work, so the binding would apply to
// nothing and `check -n my-work` would report nothing bound. This is the
// same reasoning ParseRef gives for refusing a label in the strict
// agent@label form: refusing keeps an attachment an address, rather than
// letting one spelling quietly stand in for a session started under
// another.
//
// run's --name stays lenient and sanitises instead, but that is its older
// behaviour kept for compatibility, and even it reports the directory it
// landed on. -n here is new and carries no such debt, so it takes the
// strict rule.
//
// The other two refusals are Resolve's, which is what run --name is held
// to: a value with nothing usable in it, and one that is some profile's
// own workspace. Both name a session run would refuse to start, so an
// attachment to either could never apply.
//
// The character rules stay in Slug and are not restated here, so the two
// cannot drift.
func checkSessionName(name string) error {
	slug := session.Slug(name)
	if slug == "" {
		return fmt.Errorf("session %q has no usable characters. "+
			"Sessions use letters, digits, dot, dash and underscore", name)
	}
	// Reserved first, and against the slug, so a name is never turned away
	// with advice that is itself refused: `-n Desktop` would otherwise be
	// told to type "desktop", which is the workspace claude-desktop owns.
	// Resolve tests it in this order for the same reason.
	if owner, ok := profile.Reserved(slug); ok {
		return fmt.Errorf("session %q becomes %q, which the %s profile already uses. "+
			"Pick another name", name, slug, owner)
	}
	if slug != name {
		return fmt.Errorf("session %q is not usable as it stands: it would have to become %q. "+
			"Type %q if that is the session you want", name, slug, slug)
	}
	return nil
}

// parseAttachArgs pulls a policy name, a profile name, and an optional -n
// session out of args.
func parseAttachArgs(verb string, args []string) (policyName, profileName, session string, err error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.StringVar(&session, "n", "", "")
	fs.StringVar(&session, "name", "", "")
	words, err := parseWords(verb, "-n/--name", fs, args)
	if err != nil {
		return "", "", "", err
	}
	if sessionGivenEmpty(fs, session) {
		return "", "", "", fmt.Errorf("policy %s -n/--name needs a value", verb)
	}
	if len(words) < 2 {
		return "", "", "", fmt.Errorf(
			"policy %s needs a policy and a profile, for example `brig policy %s no-net claude-code`",
			verb, verb)
	}
	if len(words) > 2 {
		return "", "", "", fmt.Errorf("policy %s takes a policy and a profile, not %q", verb, words[2])
	}
	return words[0], words[1], session, nil
}

// attachPolicy binds a policy to every run of a profile, or -- with -n --
// to one session by name instead.
func attachPolicy(args []string) error {
	policyName, profileName, session, err := parseAttachArgs("attach", args)
	if err != nil {
		return err
	}
	// Checked here and not in the shared parser: this is the one command
	// that writes a session name down, and it is writing an address that
	// has to match a session brig would start. detach and check read the
	// record instead, and have to be able to name whatever is in it --
	// including a key some earlier build or a hand edit put there. Holding
	// them to this rule would refuse the exact spelling a listing prints
	// and leave nothing able to remove it.
	if session != "" {
		if err := checkSessionName(session); err != nil {
			return err
		}
	}
	if _, err := lookupPolicy(policyName); err != nil {
		return err
	}
	p, ok := profile.Lookup(profileName)
	if !ok {
		return notFoundf("unknown profile %q. `brig profiles` lists them", profileName)
	}
	if err := policy.CheckCoverage(p); err != nil {
		return fmt.Errorf("cannot attach %s to %s: %w. Nothing was written", policyName, p.Name, err)
	}
	// p's inline policy: list already binds every run of p, in every
	// session, so attaching the same name on top adds nothing to what is
	// effectively bound -- refuse before writing an entry that would only
	// be redundant.
	if slices.Contains(p.Policy, policyName) {
		return fmt.Errorf("%s is already declared inline in %s's policy: list, which binds "+
			"every run already. Nothing was written", policyName, p.Name)
	}

	dir := policy.Dir()
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		return err
	}
	// p.Name, not profileName: profileName may be an alias (`claude` for
	// `claude-code`), and attachments are keyed by the profile's canonical
	// name so detach and any later lookup agree on it.
	describe := p.Name
	if session != "" {
		a.AttachToSession(policyName, p.Name, session)
		describe = fmt.Sprintf("%s -n %s", p.Name, session)
	} else {
		a.AttachToProfile(policyName, p.Name)
	}
	// Saved before it is announced: a dir that cannot be written would
	// otherwise put "attached" on stdout while the command exits non-zero,
	// and anything reading stdout would believe the binding landed. create
	// and rm print after the write for the same reason.
	if err := a.Save(dir); err != nil {
		return err
	}
	fmt.Printf("attached %s to %s\n", policyName, describe)
	// Said on the way out, not buried in the docs: "attached" on its own
	// reads as a rule that is now in force. On stderr, where every other
	// advisory in this CLI goes, so stdout stays the command's answer and
	// nothing parsing it reads the note as part of one.
	fmt.Fprintln(os.Stderr, policy.NotEnforcedNote)
	return nil
}

// detachPolicy reverses what attach did. It removes only what attach
// added: a policy a profile declares inline, in its own file, is refused
// rather than silently left in place, so the refusal is as loud as the
// bind it cannot undo.
func detachPolicy(args []string) error {
	policyName, profileName, session, err := parseAttachArgs("detach", args)
	if err != nil {
		return err
	}

	// Resolve the same alias attach would have, so detaching by an alias
	// (`claude` for `claude-code`) reaches the binding attach actually
	// stored. A profile that no longer exists falls back to the typed
	// name: detach is a no-op either way if nothing is bound under it.
	profileKey := profileName
	if p, ok := profile.Lookup(profileName); ok {
		profileKey = p.Name
		// Inline policy: is declared in the profile's own file, not in
		// attachments.yaml -- attach never wrote it, so detach cannot be
		// the thing that removes it. Session-scoped detach is exempt: the
		// inline list binds every run, a session binding is narrower, and
		// the two do not name the same thing to remove.
		if session == "" && slices.Contains(p.Policy, policyName) {
			return fmt.Errorf("%s is declared inline in %s's policy: list, not attached; "+
				"edit the profile directly to remove it", policyName, p.Name)
		}
	}

	dir := policy.Dir()
	a, err := policy.LoadAttachments(dir)
	if err != nil {
		return err
	}
	var removed bool
	var describe string
	if session != "" {
		removed = a.DetachFromSession(policyName, profileKey, session)
		describe = fmt.Sprintf("%s -n %s", profileKey, session)
	} else {
		removed = a.DetachFromProfile(policyName, profileKey)
		describe = profileKey
	}
	// Nothing changed: attach never bound this name here, so there is
	// nothing to write back, and saying "detached" would claim a removal
	// that did not happen.
	if !removed {
		fmt.Printf("%s was not attached to %s\n", policyName, describe)
		return nil
	}
	// Saved before it is announced, the same as attach: "detached" on
	// stdout beside a non-zero exit would read as a removal that landed.
	if err := a.Save(dir); err != nil {
		return err
	}
	fmt.Printf("detached %s from %s\n", policyName, describe)
	return nil
}

// parseCheckArgs pulls a profile name and an optional -n session out of
// args.
func parseCheckArgs(args []string) (profileName, session string, err error) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&session, "n", "", "")
	fs.StringVar(&session, "name", "", "")
	words, err := parseWords("check", "-n/--name", fs, args)
	if err != nil {
		return "", "", err
	}
	if sessionGivenEmpty(fs, session) {
		return "", "", errors.New("policy check -n/--name needs a value")
	}
	if len(words) == 0 {
		return "", "", errors.New("policy check needs a profile, for example `brig policy check claude-code`")
	}
	if len(words) > 1 {
		return "", "", fmt.Errorf("policy check takes one profile, not %q", words[1])
	}
	return words[0], session, nil
}

// checkPolicy reports the policies effectively bound to a run of profile,
// or -- with -n -- to one of its sessions, and whether brig can enforce
// anything against it at all.
//
// This does not check anything about the rules those policies contain:
// nothing today judges whether a specific host: or cidr: entry can be
// enforced (see docs/policies.md, "What this does not do yet"). What it
// does check is that a bound name still resolves to a policy at all --
// --force on rm or on edit's rename can leave one that does not -- and
// CheckCoverage's refusal of a kind: shell/kind: gui profile, which no
// policy can bind regardless of what it says.
func checkPolicy(args []string) error {
	profileName, session, err := parseCheckArgs(args)
	if err != nil {
		return err
	}
	p, ok := profile.Lookup(profileName)
	if !ok {
		return notFoundf("unknown profile %q. `brig profiles` lists them", profileName)
	}
	names, err := policy.EffectivePolicies(p, session, policy.Dir())
	if err != nil {
		return err
	}
	entries, err := loadPolicies(policy.Dir())
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Printf("no policy applies to %s\n", p.Name)
	}
	var missing []string
	for _, name := range names {
		if _, ok := entries[name]; ok {
			fmt.Println(name)
			continue
		}
		fmt.Printf("%s (no such policy)\n", name)
		missing = append(missing, name)
	}
	if err := policy.CheckCoverage(p); err != nil {
		return fmt.Errorf("cannot enforce any policy on %s: %w", p.Name, err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is bound to %s, which does not exist as a policy -- "+
			"nothing can enforce what is not there", p.Name, strings.Join(missing, ", "))
	}
	// Only where the answer would otherwise read as a verdict: names
	// printed and an exit code of zero, from a verb that means "confirm
	// this is in force". A refusal above already says what cannot be
	// enforced, and "no policy applies" leaves nothing to be misread, so
	// neither needs it. On stderr: check prints one policy name per line,
	// and anything looping over that would read the note as a name.
	if len(names) > 0 {
		fmt.Fprintln(os.Stderr, policy.NotEnforcedNote)
	}
	return nil
}
