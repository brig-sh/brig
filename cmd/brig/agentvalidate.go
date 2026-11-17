package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/brig-sh/brig/internal/profile"
)

// validateAgent is `brig agent validate <file|agent|->`: whether brig would
// take this profile, answered before the file travels to someone else.
//
// The question it answers is the one `brig agent import` answers, because it
// asks the same function (profile.Check) and stops before the write. A weaker
// question would tell an author "valid" about a file import then refuses. A
// stronger one, whether the image exists or the runtime is installed, is about
// a machine and not about the file, and `brig doctor` already owns it; keeping
// it out is what lets a profile repository run this in CI with no runtime.
//
// The report stops at the first error, as the load path does: every command
// and brigd read a profile through that path, and a report of every error would
// mean rewriting it.
//
// With --json the report is the answer, so it goes to stdout whether the file is
// good or not, and the exit status carries the verdict. That differs from the
// read verbs, whose errors go to stderr as text: a CI job reading the report
// wants the verdict in the same stream either way. The usual error line still
// goes to stderr for a person. A usage mistake or a file that is not there has
// no report to make, so those stay text.
func validateAgent(args []string) error {
	line, err := parseExportLine("brig agent validate", args, exportFlags{
		takes: "one file or agent, and --json",
	})
	if err != nil {
		// --help is answered by the group, as for every verb in it. Anything
		// else the parser refuses is a mistyped command line, which is a usage
		// error whatever the flag package calls it.
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usagef("%s", err)
	}
	switch len(line.words) {
	case 0:
		return usagef("validate needs a file or an agent, for example " +
			"`brig agent validate mine.yaml` (- reads stdin)")
	case 1:
	default:
		return usagef("unexpected argument %q; `brig agent validate` takes one file "+
			"or agent and --json", line.words[1])
	}
	label, blob, err := readValidateOperand(line.words[0])
	if err != nil {
		return err
	}

	p, checkErr := profile.Check(blob)
	if line.asJSON || globalJSON {
		report := agentValidationJSON{Source: label, Valid: checkErr == nil, Errors: []string{}}
		if checkErr == nil {
			report.Name, report.Kind, report.SchemaVersion = p.Name, string(p.Kind), p.Schema()
		} else {
			report.Errors = append(report.Errors, checkErr.Error())
		}
		if err := writeJSONDocument(os.Stdout, "AgentValidation", report); err != nil {
			return err
		}
	}
	if checkErr != nil {
		return fmt.Errorf("%s: %w", label, checkErr)
	}
	if !line.asJSON && !globalJSON {
		fmt.Printf("%s: valid: %s %s, %s\n", label, p.Kind, p.Name, schemaPhrase(p))
	}
	return nil
}

// agentValidationJSON is the AgentValidation payload. Errors is always an array,
// empty for a good file, and holds at most one entry today because the load path
// stops at the first. It is an array now so that reporting more later adds no
// field: within one apiVersion the JSON only gains fields, never renames one.
//
// Nothing in it is a value read from the file beyond names the file already
// shows its author. In particular an env literal is never echoed, which the
// validation rules guarantee by naming the binding and not its value; the test
// that plants one pins it.
type agentValidationJSON struct {
	Source        string   `json:"source"`
	Valid         bool     `json:"valid"`
	Name          string   `json:"name,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	SchemaVersion int      `json:"schemaVersion,omitempty"`
	Errors        []string `json:"errors"`
}

// schemaPhrase says which schema a good profile was read by, and when that was
// a default: an author whose file names none should see that brig read it as 1
// and did not find a 1 in it.
func schemaPhrase(p profile.Profile) string {
	if p.SchemaVersion == 0 {
		return fmt.Sprintf("schemaVersion %d (the file names none)", p.Schema())
	}
	return fmt.Sprintf("schemaVersion %d", p.Schema())
}

// readValidateOperand turns the one word validate takes into the bytes to check
// and the label to report them under.
//
// What the word is, is decided by how it is spelled and never by what happens
// to exist, so one word cannot mean a file today and an agent tomorrow: - is
// stdin; a word with a path separator or a .yaml, .yml or .json suffix is a
// file; anything else is an agent name, which brig resolves as it does for show
// and export, aliases included. An agent's own source bytes are what is checked,
// so the answer is about the file brig read and not about a re-marshalled copy.
func readValidateOperand(word string) (label string, blob []byte, err error) {
	switch {
	case word == "-":
		blob, err = io.ReadAll(os.Stdin)
		return "<stdin>", blob, err
	case looksLikeProfilePath(word):
		blob, err = os.ReadFile(word)
		if os.IsNotExist(err) {
			return "", nil, notFoundf("no file %q", word)
		}
		return word, blob, err
	}
	p, ok := profile.Lookup(word)
	if !ok {
		msg := fmt.Sprintf("no agent named %q (`brig agent ls` lists them)", word)
		// The word was typed as a name, so a file of that name beside the
		// reader is the likelier thing they meant than an agent that is not
		// there; say how to ask for it rather than guessing for them.
		if fi, statErr := os.Stat(word); statErr == nil && fi.Mode().IsRegular() {
			msg += fmt.Sprintf("; there is a file called %[1]s here, and `brig agent validate ./%[1]s` checks it", word)
		}
		return "", nil, notFoundf("%s", msg)
	}
	if blob, ok = profile.SourceBytes(p.Name); !ok {
		if _, blob, ok = profile.BuiltIn(p.Name); !ok {
			return "", nil, fmt.Errorf("brig has no source for agent %q to check", p.Name)
		}
	}
	return p.Name, blob, nil
}

// looksLikeProfilePath reports whether a word is spelled as a file: it has a
// path separator, or ends in one of the suffixes brig reads profiles from.
func looksLikeProfilePath(word string) bool {
	if strings.ContainsRune(word, '/') || strings.ContainsRune(word, filepath.Separator) {
		return true
	}
	switch strings.ToLower(filepath.Ext(word)) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}
