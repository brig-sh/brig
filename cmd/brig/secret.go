package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/wrap"
)

// openStore is the seam the CLI tests replace. Everything else goes through
// secret.Open, which picks the backend for the host.
var openStore = secret.Open

// secretCmd groups the secret verbs.
//
// Output goes to a writer rather than straight to stdout, because `read`
// writes a credential: a test that could not capture it would have to assert
// on the store instead, which is the one thing these tests are not about.
func secretCmd(out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("secret needs a subcommand: create, read, update, delete, ls or import")
	}
	var err error
	switch args[0] {
	case "--help", "-h", "help":
		_, err = io.WriteString(out, secretUsage)
		return err
	case "create":
		err = writeSecret(args[1:], "create")
	case "update":
		err = writeSecret(args[1:], "update")
	case "read":
		err = readSecret(out, args[1:])
	case "delete", "rm":
		err = deleteSecret(out, args[1:])
	case "ls", "list":
		err = listSecrets(out, args[1:])
	case "import":
		err = importSecrets(out, args[1:])
	default:
		return fmt.Errorf("unknown secret subcommand %q (create, read, update, delete, ls, import)", args[0])
	}
	// A verb's own parser reports --help as an error, because that is how the
	// flag package says it. Asking for help is not a mistake, so it is answered
	// with the help and an exit code of zero.
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, secretUsage)
	}
	return err
}

// secretUsage is what `brig secret --help` prints. Held here rather than in
// main's usage text because it names the flags of five verbs, which is more
// than the one line the top-level list can give them.
const secretUsage = `brig secret -- keep secrets in your keyring

usage:
  brig secret create <name> [-f FILE]   store a new secret
  brig secret update <name> [-f FILE]   replace an existing one
  brig secret read   <name>             print the value
  brig secret delete <name> [-y]        remove it, after asking
  brig secret ls                        list names and dates, never values
  brig secret import <profile>          fill its secrets from your host, once
  brig secret import <profile> <name>   fill just the ones you name

flags:
  -f, --file FILE   read the value from a file, verbatim. ` + "`-`" + ` is stdin
      --stdin       read the value from stdin, which is the default
  -y, --yes         with delete: the answer, given in advance
                    with import: replace a hand-created secret without asking
      --dry-run     with import: report what would be imported, and read the
                    sources to check them
      --from-command '<sh>'
                    with import: take one secret's value from a command's stdout

The value is never an argument, so it stays out of ps and out of your shell
history. Without -f the value is read from stdin, and one trailing line ending
is stripped from it.
`

// writeSecret backs both create and update, which differ only in the store
// call they make and in what a failure of it means.
//
// The order is the point: the name, then somewhere to put the value, and only
// then the value itself. A typo is reported without the secret being handled
// at all, and a host with no store says so before asking anyone to go and find
// a token -- reading first meant Linux answered `brig secret create gh` with
// "no value on stdin. Pipe one in", and only admitted there was nowhere to put
// it once they had.
func writeSecret(args []string, verb string) error {
	name, file, err := nameAndFile(args, verb)
	if err != nil {
		return err
	}
	if err := secret.ValidName(name); err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	value, err := readValue(file)
	if err != nil {
		return err
	}
	// creds.Bind already skips an empty value whatever its source -- binding it
	// would shadow one baked into the image -- so an empty secret could never do
	// anything but confuse whoever went looking for why.
	if len(value) == 0 {
		return fmt.Errorf("%s %s was given an empty value, and brig skips empty "+
			"variables when it forwards them, so it would never reach a sandbox", verb, name)
	}

	if verb == "create" {
		err = store.Create(name, value)
	} else {
		err = store.Update(name, value)
	}
	// Both sentinels name the command that does what the user evidently meant.
	// The two mistakes are each other's mirror, and either one on its own is a
	// dead end without the other spelled out.
	switch {
	case errors.Is(err, secret.ErrExists):
		return fmt.Errorf("a secret named %q already exists. To replace it: brig secret update %s",
			name, name)
	case errors.Is(err, secret.ErrNotFound):
		return fmt.Errorf("no secret named %q. To create it: brig secret create %s", name, name)
	}
	return err
}

func readSecret(out io.Writer, args []string) error {
	name, err := onlyName(args, "read")
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	value, err := store.Read(name)
	if errors.Is(err, secret.ErrNotFound) {
		return fmt.Errorf("no secret named %q. `brig secret ls` lists them", name)
	}
	if err != nil {
		return err
	}
	if _, err := out.Write(value); err != nil {
		return err
	}
	// Nothing is added to a pipe, so `brig secret read x | ...` is byte-exact.
	// A terminal gets a newline, so the shell prompt is not left mid-line
	// against the tail of a token.
	if f, ok := out.(*os.File); ok && wrap.IsTerminal(f) {
		fmt.Fprintln(out)
		// create refuses to take a value from a terminal because the terminal
		// keeps what passes through it. That reason does not stop applying on
		// the way out: the value is now in the scrollback of a window that
		// outlives the command, and may be in a saved session besides. Said on
		// stderr, so a pipe is unaffected and so the value itself stays the
		// only thing on stdout.
		fmt.Fprintf(os.Stderr, "brig: %s is now in this terminal's scrollback. "+
			"Pipe it instead to keep it out: brig secret read %s | ...\n", name, name)
	}
	return nil
}

func deleteSecret(out io.Writer, args []string) error {
	name, yes, err := nameAndYes("delete", "brig secret delete gh-token", args)
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	if err := confirmDelete(name, yes); err != nil {
		return err
	}
	if err := store.Delete(name); errors.Is(err, secret.ErrNotFound) {
		return fmt.Errorf("no secret named %q. `brig secret ls` lists them", name)
	} else if err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted %s\n", name)
	return nil
}

// listSecrets prints the names and nothing else. Reading each value would be a
// separate decrypt of something nobody asked for, which is also what keeps
// `ls` free of any keychain prompt.
//
// Listing is the whole of it, so there is nothing an argument could mean.
// Taking one silently is how `brig secret ls gh-token` -- someone reaching for
// read -- prints everything and exits 0.
func listSecrets(out io.Writer, args []string) error {
	if len(args) > 0 {
		if isHelp(args[0]) {
			return flag.ErrHelp
		}
		if strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("ls takes no flags, so it has no use for %s", args[0])
		}
		return fmt.Errorf("ls lists every secret and takes no name. For one secret's "+
			"value: `brig secret read %s`", args[0])
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	list, err := store.List()
	if err != nil {
		return err
	}
	// An empty store is an ordinary state, not an error, and is the moment to
	// say how to leave it.
	if len(list) == 0 {
		fmt.Fprintln(out, "no secrets yet. To add one: brig secret create <name>")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tUPDATED\tFROM")
	for _, s := range list {
		// A backend that cannot supply a date returns the zero time, and a
		// dash is what that looks like. See secret.Secret.Modified: inventing
		// one to keep the column full would be worse than a visible gap.
		updated := "-"
		if !s.Modified.IsZero() {
			updated = s.Modified.Local().Format("2006-01-02 15:04")
		}
		// A hand-created secret has no provenance and reads as a dash, the
		// same way an absent date does. Inventing "manual" would claim brig
		// knows something it does not: an item another tool wrote into the
		// namespace looks identical.
		from := "-"
		if s.Provenance.From != "" {
			from = s.Provenance.From
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, updated, from)
	}
	return w.Flush()
}

// isHelp reports whether a token is someone asking for help. The verbs that
// parse with the flag package get -h and --help from it; this is for the ones
// that read their arguments themselves, so that all five answer the same way.
func isHelp(arg string) bool {
	return arg == "--help" || arg == "-h" || arg == "help"
}

// confirmDelete asks before a delete goes through.
//
// A deleted secret is gone: nothing in brig keeps a copy, and the keychain
// keeps no history of its own, so the value is unrecoverable the moment the
// item is removed. Every other destructive brig command acts on something that
// can be recreated -- a sandbox reboots, a profile is re-exported -- which is
// why this is the one verb that stops to ask.
//
// Without a terminal there is nobody to answer, and assuming yes would make
// the scripted case the one that cannot be stopped. So it refuses and names
// the flag that answers in advance, the same shape wrap's own confirm uses for
// an unverified image.
//
// Both refusals name -y, because the two arrive at the same place from either
// direction: no terminal to ask on, or a terminal that answered nothing.
// IsTerminal now asks the terminal driver rather than the file's mode, so a
// run with stdin on /dev/null takes the first path instead of the second --
// it used to count as a terminal, put the question to a file, and reach the
// EOF below. Either way that is a cron job or a unit file, exactly the caller
// who needs to be told about the flag.
func confirmDelete(name string, yes bool) error {
	if yes {
		return nil
	}
	if !wrap.IsTerminal(os.Stdin) {
		return fmt.Errorf("deleting %q cannot be undone, and there is no terminal to ask on. "+
			"Pass -y to answer in advance: brig secret delete %s -y", name, name)
	}
	// The question goes to stderr, so a delete inside a pipeline still asks it
	// where a person can see it rather than into whatever is reading stdout.
	fmt.Fprintf(os.Stderr, "brig: delete %q? The value cannot be recovered [y/N] ", name)
	line, err := readAnswer(os.Stdin)
	if err != nil {
		// EOF is the answer a closed stdin gives, and it is not yes.
		return fmt.Errorf("aborted: %q was not deleted. To answer in advance: "+
			"brig secret delete %s -y", name, name)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("aborted: %q was not deleted", name)
}

// maxAnswerBytes is how much of a yes/no answer is worth reading. "yes" and a
// line ending is four; the rest of the allowance is for a fat-fingered line
// somebody still means as an answer.
const maxAnswerBytes = 64

// readAnswer reads one line of yes-or-no and stops there.
//
// Unbounded, this read is a question that can be answered with a stream: the
// delete prompt pointed at /dev/zero reached 6.75 GB of resident memory
// looking for a newline that was never coming. Nothing past the first line
// could change the answer, so nothing past it is read. A source that fills the
// allowance without ending a line is not answering the question, and the
// caller treats the error the same way it treats EOF -- as not-yes.
func readAnswer(r io.Reader) (string, error) {
	return bufio.NewReader(io.LimitReader(r, maxAnswerBytes)).ReadString('\n')
}

// nameAndYes parses the arguments of a verb that takes one name and the -y
// that says its question has already been answered.
//
// Same shape as nameAndFile, and for the same reason: `delete -y gh-token` and
// `delete gh-token -y` are both lines people type, and Parse stops at the
// first bare word.
//
// The verb and the example it prints are parameters because `brig profile rm`
// asks a question of its own now, and every word of these messages is about
// the command the person typed. Two copies of the loop below would drift.
func nameAndYes(verb, example string, args []string) (name string, yes bool, err error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&yes, "y", false, "")
	fs.BoolVar(&yes, "yes", false, "")

	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", false, err
			}
			msg := err.Error()
			if flagName, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(flagName)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return "", false, fmt.Errorf("%s (%s takes -y)", msg, verb)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch len(words) {
	case 0:
		return "", false, fmt.Errorf("%s needs a name, for example `%s`", verb, example)
	case 1:
		name = words[0]
	default:
		return "", false, fmt.Errorf("%s takes one name, not %q", verb, words[1])
	}
	return name, yes, nil
}

// onlyName takes the single bare word a read gets. It takes no flag, so
// anything beginning with a dash is a mistake worth naming rather than a name
// worth storing.
func onlyName(args []string, verb string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%s needs a name, for example `brig secret %s gh-token`", verb, verb)
	}
	// Ahead of the count, so `read -f key.pem` is reported as the flag it is
	// rather than as two names. Checked at all because otherwise a flag
	// becomes a name to look up, and the error is the baffling
	// `no secret named "-f"`.
	for _, a := range args {
		if isHelp(a) {
			return "", flag.ErrHelp
		}
		if strings.HasPrefix(a, "-") {
			return "", fmt.Errorf("%s takes no flags, so it has no use for %s. "+
				"It takes a name: `brig secret %s gh-token`", verb, a, verb)
		}
	}
	if len(args) > 1 {
		return "", fmt.Errorf("%s takes one name, not %q", verb, args[1])
	}
	return args[0], nil
}

// nameAndFile parses a write's arguments.
//
// Nothing here passes through to another program, so an unrecognised flag is a
// mistake rather than someone else's argument, and the flag package can reject
// it outright. Parse stops at the first bare word, so the name -- which comes
// before the flag in `create gh -f key.pem` -- is lifted out and parsing
// continues, exactly as exportProfile does.
func nameAndFile(args []string, verb string) (name, file string, err error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&file, "f", "", "")
	fs.StringVar(&file, "file", "", "")
	// --stdin is the default, and is accepted so that writing it out loud
	// works rather than failing on a flag the docs describe.
	var stdin bool
	fs.BoolVar(&stdin, "stdin", false, "")

	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", "", err
			}
			msg := err.Error()
			if flagName, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(flagName)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return "", "", fmt.Errorf("%s (%s takes -f and --stdin)", msg, verb)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch len(words) {
	case 0:
		return "", "", fmt.Errorf("%s needs a name, for example `brig secret %s gh-token`", verb, verb)
	case 1:
		name = words[0]
	default:
		return "", "", fmt.Errorf("%s takes one name, not %q", verb, words[1])
	}
	// An empty -f is not the same line as no -f at all, which is what seen
	// distinguishes. `-f "$KEYFILE"` with the variable unset is how it gets
	// typed, and falling through to stdin there stores whatever the script had
	// on it under the name and reports success.
	if seen(fs, "f", "file") && file == "" {
		return "", "", errors.New("-f was given an empty path. Leave it out to read stdin, " +
			"or pass `-f -` to say so")
	}
	// Two sources named at once is a line whose meaning cannot be guessed, and
	// guessing it would silently store the wrong one of them.
	if stdin && file != "" && file != "-" {
		return "", "", errors.New("--stdin and -f name two different sources; pass one")
	}
	return name, file, nil
}

// readValue reads the secret from a file, or from stdin by default.
//
// A single trailing line ending is stripped from stdin because
// `echo tok | brig secret create x` is the line people type, and a stored
// newline breaks an auth header in a way that reads like a bad token. CRLF
// counts as one line ending: a lone \r left behind fails the same way and is
// harder to spot in a bug report. A \r without a \n is kept, because nothing
// produces one by accident the way `echo` produces the other.
//
// A file is taken verbatim: its bytes are what it holds, and a PEM key's final
// newline belongs to it. `printf` covers wanting exact bytes from stdin, so
// there is no flag for that.
func readValue(file string) ([]byte, error) {
	if file != "" && file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return readCapped(f, file)
	}
	// Without this, `brig secret create x` at a prompt looks like a hang, and
	// the way out of it -- typing the secret, then Ctrl-D -- leaves the value
	// in the terminal scrollback.
	if wrap.IsTerminal(os.Stdin) {
		return nil, errors.New("no value on stdin. Pipe one in, or pass -f <file>:\n" +
			"  printf %s \"$TOKEN\" | brig secret create gh-token\n" +
			"  brig secret create deploy-key -f ~/.ssh/id_ed25519")
	}
	value, err := readCapped(os.Stdin, "stdin")
	if err != nil {
		return nil, err
	}
	if v, ok := bytes.CutSuffix(value, []byte("\n")); ok {
		return bytes.TrimSuffix(v, []byte("\r")), nil
	}
	return value, nil
}

// maxValueBytes is where reading a secret stops.
//
// The store this feeds takes less: the keychain command line is one 4096-byte
// line, the value travels base64-encoded, and what is left after the command
// and the name is about 3 KB. So a value over this ceiling was never going to
// be stored, and reading it first is not free -- `brig secret create x`
// pointed at /dev/zero read until it had 12.5 GB in memory, three seconds in,
// on its way to being refused for being 3 KB too long. The cap is a little
// above what the store accepts so the error that comes back is the store's
// own, about this secret and this name, in every case except the one where
// there is no plausible secret at the other end at all.
const maxValueBytes = 4096

// readCapped reads a secret and refuses one that does not end.
func readCapped(r io.Reader, what string) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(r, maxValueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maxValueBytes {
		return nil, fmt.Errorf("the value on %s is over %d bytes, which is larger than any "+
			"secret brig can store. If that is a file or a stream rather than a "+
			"credential, this is the wrong one", what, maxValueBytes)
	}
	return value, nil
}
