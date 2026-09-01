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
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/policy"
	"sigs.k8s.io/yaml"
)

// policyCmd groups the policy verbs.
func policyCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("policy needs a subcommand: ls, create, edit, show or rm")
	}
	switch args[0] {
	case "ls":
		return listPolicies()
	// list was never in the help text, so it was found by accident and then
	// scripted. Kept for one release, saying which spelling to keep.
	case "list":
		deprecated("brig policy list", "brig policy ls")
		return listPolicies()
	case "create":
		return createPolicy(args[1:])
	case "edit":
		return editPolicy(args[1:])
	case "show":
		return showPolicy(args[1:])
	case "rm":
		return removePolicy(args[1:])
	default:
		return fmt.Errorf("unknown policy subcommand %q (ls, create, edit, show, rm)", args[0])
	}
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

// listPolicies prints every policy that parses, by name and description.
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
	for _, name := range names {
		fmt.Printf("%-15s %s\n", name, entries[name].Policy.Desc)
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

// showPolicy prints the parsed document for one policy: YAML by default,
// --json for anything consuming it programmatically.
func showPolicy(args []string) error {
	asJSON := false
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&asJSON, "json", false, "")

	// Parse stops at the first bare word, so `brig policy show no-net
	// --json` -- the order anyone actually types -- would otherwise leave
	// --json sitting unparsed in Args. Lift the word and parse on.
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(name)
			}
			return fmt.Errorf("%s (show takes --json)", msg)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
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

// createPolicy writes a starter policy document, then opens it in your
// editor: $VISUAL, then $EDITOR, then vi.
func createPolicy(args []string) error {
	force := false
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")

	// Parse stops at the first bare word, so `brig policy create no-net
	// --force` -- the order anyone actually types -- would otherwise leave
	// --force sitting unparsed in Args. Lift the word and parse on.
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(name)
			}
			return fmt.Errorf("%s (create takes --force)", msg)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(words) == 0 {
		return errors.New("policy create needs a name, for example `brig policy create no-net`")
	}
	if len(words) > 1 {
		return fmt.Errorf("policy create takes one name, not %q", words[1])
	}
	name := words[0]
	if err := policy.CheckName(name); err != nil {
		return err
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
	if len(args) == 0 {
		return errors.New("policy edit needs a name, for example `brig policy edit no-net`")
	}
	entry, err := lookupPolicy(args[0])
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

// removePolicy deletes the file backing a policy. With no attachment concept
// yet, there is nothing else to check first.
func removePolicy(args []string) error {
	if len(args) == 0 {
		return errors.New("policy rm needs a name")
	}
	entry, err := lookupPolicy(args[0])
	if err != nil {
		return err
	}
	if err := os.Remove(entry.Path); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", entry.Path)
	return nil
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
