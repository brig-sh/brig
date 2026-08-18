package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/hostsrc"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// newHostReader is the seam the tests replace. Everything else goes through
// hostsrc, which reads the host itself -- and which nothing on the run path
// may import (see internal/hostsrc/arch_test.go).
var newHostReader = func() hostReader { return hostsrc.NewReader() }

// hostReader is the slice of hostsrc the importer uses, named here so a test
// double does not have to embed the concrete type.
type hostReader interface {
	Read(s profile.Source) (hostsrc.Value, bool, error)
}

// commandLocator is the provenance recorded for a --from-command value, and it
// is the word rather than the command line.
//
// Two reasons, and either alone would decide it. The command is the user's own
// text: it can hold a quote, a pipe, or an actual credential, and provenance is
// printed back by `brig secret ls` and by the expiry warning -- which is why
// secret.DecodeProvenance drops a From that does not validate. Recording the
// line would therefore either fail that validation and come back empty, making
// the secret look hand-created on the next import, or print somebody's `op
// read ...` invocation into a terminal. The word says where the value came
// from, which is what provenance is for.
const commandLocator = "command"

// importOptions is one parsed `brig secret import` command line.
type importOptions struct {
	profile string
	names   []string
	dryRun  bool
	// command is whether --from-command was given at all, which is not the
	// same question as whether it is empty: `--from-command "$CMD"` with the
	// variable unset is a line people type, and falling through to the
	// declared sources there would import something the user did not ask for.
	command     bool
	fromCommand string
	yes         bool
}

// importSecrets fills a profile's secrets from where the host already keeps
// them, so that every run afterwards reads only brig's own store.
//
// The whole verb is one pass over the profile's requirement list: what the
// host holds, what the store holds, and what it takes to make the second match
// the first. It never prints a value in any mode -- names, locators and dates
// are the whole of the output, which is the same rule `brig secret ls` follows
// and for the same reason.
func importSecrets(out io.Writer, args []string) error {
	o, err := parseImport(args)
	if err != nil {
		return err
	}
	// Canonical, not as typed: `claude` is an alias and a user's own file can
	// shadow a built-in, so the word on the command line is the wrong one to
	// report back -- and the report is what tells the user which profile they
	// actually filled.
	p, ok := profile.Lookup(o.profile)
	if !ok {
		return unknownImportTarget(o.profile)
	}
	if o.command {
		if o.fromCommand == "" {
			return errors.New("--from-command was given an empty command. Leave it out to read " +
				"the sources the profile declares")
		}
		// A command supplies ONE secret's value, and nothing in the string says
		// which. Guessing would store a credential under the wrong name, which
		// is the one mistake here that is silent afterwards.
		if len(o.names) != 1 {
			return fmt.Errorf("--from-command fills one secret, so it needs one name: "+
				"`brig secret import %s <name> --from-command '...'`", p.Name)
		}
	}
	selected, err := selectForImport(p, o.names, o.command)
	if err != nil {
		return err
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	// Listed once, for provenance: it is the only thing that tells a secret
	// brig wrote from one the user created by hand, and List reads it without
	// decrypting anything.
	stored, err := storedByName(store)
	if err != nil {
		return err
	}
	// One reader for the whole run, so two secrets naming the same keychain
	// item raise one approval dialog rather than two.
	reader := newHostReader()

	if o.dryRun {
		fmt.Fprintf(out, "%s: --dry-run, reading your host and writing nothing\n", p.Name)
	} else {
		fmt.Fprintf(out, "%s: importing %s\n", p.Name, count(len(selected), "secret"))
	}
	var failures []error
	for _, d := range selected {
		if err := importOne(out, store, stored, reader, p, d, o); err != nil {
			failures = append(failures, err)
		}
	}
	// A name with no importer is informational: a wholly successful import of a
	// profile that mixes imported and hand-created secrets must not report
	// failure, or `brig secret import x && brig run x` breaks. Only listed
	// without [name...], because a named one is an error rather than a note.
	if len(o.names) == 0 {
		for _, d := range p.Secrets {
			if !d.Importable() {
				fmt.Fprintf(out, "  %s: no source on your host, so it is one you supply: "+
					"brig secret create %s\n", d.Name, d.Name)
			}
		}
	}
	reportOtherProfiles(out, p, selected)

	return importFailure(failures, len(selected))
}

// importOne fills one secret, in the order the rules require: read, extract,
// size, compare, then -- and only then -- write.
func importOne(
	out io.Writer,
	store secret.Store,
	stored map[string]secret.Secret,
	reader hostReader,
	p profile.Profile,
	d profile.SecretDecl,
	o importOptions,
) error {
	v, ok, err := readFor(reader, d, o)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("nothing to import for %q: %s held no value%s",
			d.Name, sourceNames(d, o), advice(d))
	}
	value, expiry, err := hostsrc.Extract(v, d)
	if err != nil {
		return err
	}

	current, exists, err := currentValue(store, d.Name)
	if err != nil {
		return err
	}
	// Before the write, not after it. security truncates an over-long line
	// silently on a four-byte boundary, so the short value still base64-decodes
	// and still resolves -- and verify explicitly cannot roll back an update.
	// Checking here is what stops a re-import destroying a good value and
	// leaving a resolvable bad one behind.
	if sizer, ok := store.(secret.Sizer); ok {
		if max := sizer.MaxValue(d.Name, exists); len(value) > max {
			return fmt.Errorf("the value for %q is %d bytes and the store takes at most %d, "+
				"so nothing was written", d.Name, len(value), max)
		}
	}
	// A byte-identical value is skipped rather than rewritten: otherwise
	// Modified comes to mean "an import last ran" rather than "the value last
	// changed", and it is the freshness signal users read in `brig secret ls`.
	if exists && bytes.Equal(current, value) {
		fmt.Fprintf(out, "  %s: unchanged, %s holds what is already stored\n",
			d.Name, describe(v.From))
		return nil
	}
	// No provenance means brig did not write it, so replacing it would discard
	// something the user supplied themselves. That is not a thing to do quietly.
	if exists && stored[d.Name].Provenance.From == "" && !o.yes {
		return fmt.Errorf("%q is already stored and brig did not put it there, so importing "+
			"would replace a value you supplied. To replace it: brig secret import %s %s -y",
			d.Name, p.Name, d.Name)
	}

	verb, would := "stored", "would be stored"
	if exists {
		verb, would = "replaced", "would be replaced"
	}
	if o.dryRun {
		fmt.Fprintf(out, "  %s: %s from %s%s\n", d.Name, would, describe(v.From), expires(expiry))
		return nil
	}
	// One Update, never delete-then-create. That window lets a concurrent run
	// fail with "missing secret" while the very command that fills it is
	// running, and lets one of two concurrent imports read back the other's
	// value in verify and delete it.
	// SafeFrom, not v.From: the locator comes from profile data and the
	// decoder drops anything outside its charset, which would make brig's own
	// import read back as hand-created and refuse the next one without -y.
	prov := secret.Provenance{V: secret.ProvenanceVersion, From: secret.SafeFrom(v.From), ExpiresAt: expiry}
	if err := writeValue(store, d.Name, value, prov, exists); err != nil {
		return err
	}
	fmt.Fprintf(out, "  %s: %s from %s%s\n", d.Name, verb, describe(v.From), expires(expiry))
	return nil
}

// readFor reads one secret's value, from the command if one was given and from
// the declared sources otherwise.
//
// --from-command SUPPLIES a source rather than selecting one, which is why it
// works on a secret that declares none at all: that is how a credential kept
// in an external secret manager is imported without putting `sh -c` into
// profile data, where it would be a shareable artifact that runs a host
// command.
func readFor(reader hostReader, d profile.SecretDecl, o importOptions) (hostsrc.Value, bool, error) {
	if o.command {
		blob, err := runImportCommand(o.fromCommand)
		if err != nil {
			return hostsrc.Value{}, false, err
		}
		if len(blob) == 0 {
			return hostsrc.Value{}, false, nil
		}
		return hostsrc.Value{Bytes: blob, From: commandLocator}, true, nil
	}
	for _, s := range d.SourceList() {
		v, ok, err := reader.Read(s)
		// A refusal is not absence: falling through to the next source after a
		// denied dialog ends with "run the agent on the host once to log in"
		// shown to somebody who has already done exactly that.
		if err != nil {
			return hostsrc.Value{}, false, err
		}
		if ok {
			return v, true, nil
		}
	}
	return hostsrc.Value{}, false, nil
}

// capped is an io.Writer that stops at a limit and says so, which is what
// makes it safe to hand to exec: os/exec propagates a write error out of
// Wait, so the command is torn down instead of being read to the end.
type capped struct {
	buf   bytes.Buffer
	limit int
	what  string
	// err is kept because the caller cannot get it from Run: closing the pipe
	// on the writing process kills it with SIGPIPE, and an *ExitError takes
	// precedence over a copy error in Wait's return. Reporting "exit status
	// 141" for a value that was too long would name the signal instead of the
	// reason.
	err error
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		if room := c.limit - c.buf.Len(); room > 0 {
			c.buf.Write(p[:room])
		}
		c.err = fmt.Errorf("the value on %s is over %d bytes, which is larger than any "+
			"secret brig can store. If that is a file or a stream rather than a "+
			"credential, this is the wrong one", c.what, c.limit)
		return 0, c.err
	}
	return c.buf.Write(p)
}

// runImportCommand runs the string the user typed and takes its stdout.
//
// /bin/sh rather than a bare `sh` for the reason internal/secret pins
// securityBin: this call decides what byte string becomes a stored
// credential, so a shell earlier in $PATH would choose it.
//
// `sh -c` at all is safe here in the way `from: command` in profile data
// would not be: the string always came from this user's own command line,
// never from an imported profile that carried it onto their host.
//
// stdout is capped. Reading first and refusing afterwards is what `brig
// secret create` already stopped doing -- pointed at /dev/zero it read until
// it had 12.5 GB in memory -- and a command is the easier way to reach that,
// because nobody has to redirect anything.
//
// stderr is passed straight through rather than captured and quoted into the
// error. What a failing command says there is what makes it fixable -- an
// expired vault session says so -- but it is not brig's to repeat: a wrapper
// that logs the credential it fetched before exiting non-zero would put the
// value into a brig error message, and no output of brig's own ever holds a
// value. Passing it through shows the user exactly what their command wrote,
// as their command wrote it, and leaves brig holding none of it.
func runImportCommand(script string) ([]byte, error) {
	cmd := exec.Command("/bin/sh", "-c", script)
	out := &capped{limit: maxValueBytes, what: "--from-command stdout"}
	cmd.Stdout, cmd.Stderr = out, os.Stderr
	err := cmd.Run()
	// The cap first: it is the reason the command died, and the signal it
	// died of is not what the reader needs to know.
	if out.err != nil {
		return nil, out.err
	}
	if err != nil {
		return nil, fmt.Errorf("--from-command failed: %w (anything it reported is above)", err)
	}
	// One trailing line ending, the same rule `brig secret create` applies to
	// stdin: `--from-command 'gh auth token'` is the line people type, and a
	// stored newline breaks an auth header in a way that reads like a bad
	// token.
	value := out.buf.Bytes()
	if v, ok := bytes.CutSuffix(value, []byte("\n")); ok {
		return bytes.TrimSuffix(v, []byte("\r")), nil
	}
	return value, nil
}

// selectForImport decides which declarations this run fills.
//
// Without [name...] that is every one an importer covers. With names it is
// exactly those, and a named secret with no source is an ERROR rather than a
// note: the user asked for something that cannot be imported, and skipping it
// silently would report success for work that did not happen.
func selectForImport(p profile.Profile, names []string, viaCommand bool) ([]profile.SecretDecl, error) {
	if len(names) == 0 {
		var out []profile.SecretDecl
		for _, d := range p.Secrets {
			if d.Importable() {
				out = append(out, d)
			}
		}
		return out, nil
	}
	var out []profile.SecretDecl
	for _, name := range names {
		d, ok := p.Secret(name)
		if !ok {
			if len(p.Secrets) == 0 {
				return nil, fmt.Errorf("%s declares no secrets, so there is nothing to import "+
					"for %q", p.Name, name)
			}
			return nil, fmt.Errorf("%s declares no secret %q. It declares: %s",
				p.Name, name, strings.Join(profile.SecretNames(p.Secrets), " "))
		}
		// --from-command supplies the source, so a secret that declares none is
		// exactly the case it exists for.
		if !d.Importable() && !viaCommand {
			return nil, fmt.Errorf("%q has no source brig can read on your host, so it is one "+
				"you supply: brig secret create %s", name, name)
		}
		out = append(out, d)
	}
	return out, nil
}

// storedByName is the store's own listing, keyed for lookup. Names only and no
// values: List reads attributes, so this costs no decrypt and raises no
// dialog.
func storedByName(store secret.Store) (map[string]secret.Secret, error) {
	list, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("could not list brig's secret store: %w", err)
	}
	byName := make(map[string]secret.Secret, len(list))
	for _, s := range list {
		byName[s.Name] = s
	}
	return byName, nil
}

// currentValue reads what is stored under a name now, which is what the
// byte-identical skip compares against. Absence is ordinary; anything else is
// a store that cannot answer, and importing on top of an unreadable value
// would be a write whose outcome nobody could check.
func currentValue(store secret.Store, name string) (value []byte, exists bool, err error) {
	value, err = store.Read(name)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, secret.ErrNotFound):
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("could not read the stored %q: %w", name, err)
}

// writeValue stores a value with its provenance where the backend can carry
// one, and without where it cannot.
//
// The Annotator arm is a single call in both directions -- create and update --
// because delete-then-create is the shape this must never take.
func writeValue(store secret.Store, name string, value []byte, p secret.Provenance, update bool) error {
	if a, ok := store.(secret.Annotator); ok {
		return a.Write(name, value, p, update)
	}
	// A backend with no room for provenance still imports: the value is what
	// the run needs, and absent provenance is the same zero value a
	// hand-created secret carries. Same contract as secret.Secret.Modified.
	if update {
		return store.Update(name, value)
	}
	return store.Create(name, value)
}

// reportOtherProfiles names the other profiles a fill reaches.
//
// Secret names are flat and global, so importing `mytool-token` for one
// profile fills it for every profile that declares that name. That is the
// point of a flat namespace and it is also its blast radius, so the output
// says it rather than leaving it to be discovered.
func reportOtherProfiles(out io.Writer, p profile.Profile, selected []profile.SecretDecl) {
	for _, d := range selected {
		var others []string
		for _, q := range profile.All() {
			if q.Name == p.Name {
				continue
			}
			if _, ok := q.Secret(d.Name); ok {
				others = append(others, q.Name)
			}
		}
		if len(others) > 0 {
			fmt.Fprintf(out, "note: %s also declares %s, so this fills it there too\n",
				strings.Join(others, ", "), d.Name)
		}
	}
}

// importFailure is the exit status rule: non-zero only when a name an importer
// covers could not be filled.
func importFailure(failures []error, selected int) error {
	switch len(failures) {
	case 0:
		return nil
	case 1:
		return failures[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %s could not be imported:", len(failures), count(selected, "secret"))
	for _, err := range failures {
		fmt.Fprintf(&b, "\n  %v", err)
	}
	return errors.New(b.String())
}

// unknownImportTarget answers the mistake this verb invites. import is the
// only secret verb whose first argument is not a secret name, so
// `brig secret import claude-credentials` -- typed by someone who has just
// read a message naming that secret -- is the line people actually type.
func unknownImportTarget(name string) error {
	for _, p := range profile.All() {
		if _, ok := p.Secret(name); ok {
			return fmt.Errorf("%q is a secret, not a profile, and import takes the profile that "+
				"declares it: brig secret import %s %s", name, p.Name, name)
		}
	}
	return fmt.Errorf("unknown profile %q. `brig profiles` lists them", name)
}

// describe renders a locator for a person. A command's provenance is the bare
// word `command`, which on its own reads like a locator with its value missing.
func describe(from string) string {
	if from == commandLocator {
		return "the command you gave"
	}
	return from
}

// sourceNames lists what was actually read, so "nothing to import" says where
// brig looked rather than leaving the user to guess.
func sourceNames(d profile.SecretDecl, o importOptions) string {
	if o.command {
		return "the command you gave"
	}
	locators := make([]string, 0, len(d.SourceList()))
	for _, s := range d.SourceList() {
		locators = append(locators, s.Locator())
	}
	return strings.Join(locators, ", ")
}

// advice is the profile's own hint for a source that held nothing -- "run
// `claude` on the host once to log in". The same text the run path's warning
// carries, through the same accessor, because a user who reads both must not
// be told two different things about one secret.
func advice(d profile.SecretDecl) string {
	if hint := d.HintText(); hint != "" {
		return ". " + hint
	}
	return ""
}

// expires renders the expiry a source carried, or nothing when it carried
// none. Absence is not evidence of expiry, so an absent one says nothing at
// all rather than "expires never".
func expires(millis int64) string {
	if millis == 0 {
		return ""
	}
	at := time.UnixMilli(millis).Local().Format("2006-01-02 15:04")
	if millis < time.Now().UnixMilli() {
		return ", expired " + at
	}
	return ", expires " + at
}

// count writes "1 secret" and "2 secrets", because a report that says
// "1 secrets" reads as a bug in the thing that just touched your credentials.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// parseImport parses `import <profile> [name...]` and its flags.
//
// Same shape as nameAndFile and nameAndYes, and for the same reason: Parse
// stops at the first bare word, and `import mytool -y` and `import -y mytool`
// are both lines people type.
func parseImport(args []string) (importOptions, error) {
	var o importOptions
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.StringVar(&o.fromCommand, "from-command", "", "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")

	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return o, err
			}
			msg := err.Error()
			if flagName, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(flagName)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return o, fmt.Errorf("%s (import takes --dry-run, --from-command and -y)", msg)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	o.command = seen(fs, "from-command")
	if len(words) == 0 {
		return o, errors.New("import needs a profile, for example " +
			"`brig secret import claude-code`")
	}
	o.profile, o.names = words[0], words[1:]
	return o, nil
}
