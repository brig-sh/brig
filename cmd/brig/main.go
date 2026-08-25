// Command brig runs a coding agent in a sandbox.
//
// The sandbox is a microVM on macOS and a container on Linux, and brig is
// neither: it resolves your credentials on the host, forwards them in per
// exec, mounts a workspace as the agent's home, verifies the guest image, and
// delegates every mechanical operation to hull or nerdctl underneath. The
// product logic lives in internal/wrap and is shared by both, so the two
// operating systems cannot drift apart.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// version is stamped at build time by goreleaser.
var version = "dev"

// sandboxPrefix is how brig recognises its own sandboxes in a runtime that
// may be running other things.
const sandboxPrefix = "brig-"

const usage = `brig -- run a coding agent in a sandbox

usage:
  brig run    <profile> [flags] [agent args...]  start the sandbox and run it
  brig create <profile> [flags]                  start the sandbox without attaching
  brig exec   <profile> -- cmd [args...]         run one command inside the sandbox
  brig shell  <profile> [cmd...]                 open a shell inside the sandbox
  brig stop   <profile>                          stop the sandbox, keep it
  brig rm     <profile>                          stop and remove the sandbox
  brig ls                                        list sandboxes
  brig reset                                     stop and remove every brig sandbox
  brig env    <profile>                          report the environment, by name -- fails
                                                 if a declared secret is missing
  brig profiles                                  list the profiles
  brig profile ls|export|import|edit|rm          manage profiles
  brig export <profile> [name] [--json]          print a profile, or save it
                                                 as <name> in the profile dir
  brig secret create|read|update|delete|ls       keep secrets in your keyring
  brig secret import <profile>                   fill a profile's secrets from
                                                 your host, once
  brig version

flags (before the agent's own arguments; -- ends brig's parsing):
  -n, --name NAME        a session of its own: own workspace, own sandbox
  -t, --image IMAGE      guest image to boot
  -w, --workspace PATH   host directory to mount as the guest home
  -m, --memory MB        guest memory
      --cpus N           guest vCPUs
  -d, --detach           with run: start the sandbox and exit
      --skills           project your own ~/.claude skills and plugins into
                         the guest, read-only (or BRIG_SKILLS=1)

Workspaces persist. The sandbox keeps running between commands, so a second
run is immediate; state lives in the workspace on the host either way.

Any Linux CLI in an OCI image runs under brig, if the image also carries the
utilities brig uses to set the sandbox up and deliver the credential: a shell
(sh and bash), plus cat, stat, chown, mkdir, mount and /bin/true. A stock
distro image has them; a scratch image with only your binary does not. A
profile just saves you spelling out the image and its credential variables
every time: export the closest one, edit it, import it back. Building an image
for one is documented at
  https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md

settings (BRIG_<AGENT>_<KEY> wins over BRIG_<KEY>; see the README for all):
  BRIG_WORKSPACE       host directory mounted as the guest home
  BRIG_IMAGE           guest image
  BRIG_PULL            missing (default) | always | never
  BRIG_SKILLS          1 to project your ~/.claude skills and plugins read-only
  BRIG_FORWARD_ENV     replaces the env-sourced bindings, space-separated
  BRIG_CREDENTIALS_CMD deprecated, goes next release: declare the credential
                       under secrets:, then brig secret import <profile> <name>
                       --from-command does the same job once instead of per run
  BRIG_GIT_CONFIG      1 to write the guest git-over-HTTPS files
  BRIG_VERIFY          warn (default) | require | off -- guest image signature
  BRIG_PROFILE_DIR     where your own profiles live (BRIG_TEMPLATE_DIR still works)
  BRIG_RUNTIME         hull | nerdctl
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
		// A mistake in how the command was typed exits 2, the conventional
		// usage-error code, kept apart from 1 so a script can tell "you asked
		// for the wrong thing" from "it ran and failed". A run that stops and
		// removes nothing because it was refused must not read as success.
		var ue *usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError is a command that was typed wrong: an unknown flag, a stray
// argument, a token in a place nothing consumes it. It carries the exit code
// apart from an ordinary failure, and its message names the token at fault so
// the reader knows which word to fix.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// usagef builds a usageError the way fmt.Errorf builds an ordinary one.
func usagef(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	verb, rest := args[0], args[1:]

	// Profiles are read before anything looks a name up, so a file can stand
	// in for a built-in. A broken file is reported and skipped rather than
	// taking down the profile you were actually asking for.
	if err := profile.Load(profile.Dir()); err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
	}
	// Files left in the pre-profile directory are read by nothing and look
	// exactly like files that work: the profile they were pinning silently
	// reverts to brig's own. Nothing is moved for you -- these name credential
	// variables, and there is no safe guess about which you still want.
	if hint := profile.LegacyHint(); hint != "" {
		fmt.Fprintln(os.Stderr, "brig: "+hint)
	}
	warnDeprecatedProfileKeys()

	switch verb {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Printf("brig %s\n", version)
		return nil
	case "profiles":
		return listProfiles()
	case "profile":
		return profileCmd(rest)
	case "secret":
		return secretCmd(os.Stdout, rest)
	case "import":
		return importProfile(rest)
	case "export":
		return exportProfile(rest)
	// Deprecated spellings, absent from the usage text. There is deliberately
	// no `brig template edit`: the old group carries only the verbs it already
	// had, so a command that never existed under that name does not gain one.
	case "agents":
		deprecated("brig agents", "brig profiles")
		return listProfiles()
	case "template":
		deprecated("brig template", "brig profile")
		if len(rest) > 0 && rest[0] == "edit" {
			return fmt.Errorf("there is no `brig template edit`; use `brig profile edit`")
		}
		return profileCmd(rest)
	case "ls":
		return listSandboxes(rest)
	case "reset":
		return reset(rest)
	case "run", "create", "exec", "shell", "stop", "rm", "env":
	default:
		return fmt.Errorf("unknown command %q (try `brig help`)", verb)
	}

	opts, profileName, tail, err := parse(rest)
	if err != nil {
		return err
	}
	if err := rejectTail(verb, tail); err != nil {
		return err
	}
	t, ok := profile.Lookup(profileName)
	if !ok {
		if profileName == "" {
			return fmt.Errorf("%s needs a profile, for example `brig %s claude`. "+
				"`brig profiles` lists them", verb, verb)
		}
		return fmt.Errorf("unknown profile %q. `brig profiles` lists them", profileName)
	}
	// Say why up front. Without this the pull fails against the registry
	// with a 404 that reads like an outage rather than a decision.
	if t.Unpublished && opts.load.Image == "" {
		return fmt.Errorf("we do not publish an image for %q, so there is nothing to boot.\n"+
			"Build it yourself and point brig at it:\n"+
			"  brig %s %s --image <your-image>\n"+
			"%s explains how to build one", t.Name, verb, t.Name, profile.BringYourOwnImageDoc)
	}

	rt, err := runtime.DetectFor(runtime.Preference{Bin: t.RuntimeBin})
	if err != nil {
		return err
	}
	cfg, err := wrap.Load(t, opts.load, rt)
	if err != nil {
		return err
	}
	// A ten-character slug is a tight budget, so two long names can slug the
	// same and share one sandbox. Say which directory a run actually uses
	// whenever the slug differs from the name, so that sharing stays visible.
	if cfg.RawName != "" && cfg.RawName != cfg.Slug {
		fmt.Fprintf(os.Stderr, "brig: session %q uses %s (sandbox %s)\n",
			cfg.RawName, cfg.Workspace, cfg.VMName)
	}

	// Stopping and removing need nothing but the instance name, and are
	// handled ahead of any credential resolution so they cannot raise the
	// keychain prompt that reading the host login may bring up.
	switch verb {
	case "stop":
		return cfg.Stop()
	case "rm":
		return cfg.Remove()
	}

	set, err := cfg.BuildEnv()
	if err != nil {
		return err
	}

	switch verb {
	case "env":
		cfg.Status(set)
		return nil
	case "create":
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		fmt.Println(cfg.VMName)
		return nil
	case "shell":
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		return cfg.Shell(set, tail)
	case "exec":
		if len(tail) == 0 {
			return errors.New("exec needs a command, for example `brig exec claude -- ls`")
		}
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		return cfg.Exec(set, tail, isTerminal())
	}
	return runAgent(cfg, set, t, tail, opts.detach)
}

func runAgent(cfg *wrap.Config, set creds.Set, t profile.Profile, tail []string, detach bool) error {
	// A windowed agent owns its own console, so there is nothing to pass
	// through and nothing to exec into: starting it IS the command.
	if t.IsGUI() && len(tail) > 0 {
		return fmt.Errorf("%s is a graphical agent, so it takes no arguments "+
			"(use `brig shell %s` or `brig stop %s`)", t.Name, t.Name, t.Name)
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return err
	}
	switch {
	case t.IsGUI():
		fmt.Fprintf(os.Stderr, "brig: sandbox %s is running; the %s window should be visible.\n",
			cfg.VMName, t.GUITitle)
		return nil
	case detach:
		// The sandbox is up and will stay up; print the name so a script can
		// exec into it.
		fmt.Println(cfg.VMName)
		return nil
	case t.IsShell():
		// The "agent" is the guest shell itself, so a bare run is a login
		// shell and trailing words are one command.
		return cfg.Shell(set, tail)
	}
	argv := append([]string{t.Binary}, agentArgs(cfg, t, tail)...)
	return cfg.Exec(set, argv, isTerminal())
}

// agentArgs adds the agent's own session-name flag, so the name you typed
// travels in unchanged as the display name while only the paths use the slug.
func agentArgs(cfg *wrap.Config, t profile.Profile, tail []string) []string {
	if cfg.RawName != "" && t.Name == "claude-code" {
		return append([]string{"--name", cfg.RawName}, tail...)
	}
	return tail
}

// options are brig's own flags for one invocation.
type options struct {
	load      wrap.Options
	nameGiven bool
	detach    bool
}

// brigFlags is what brig owns on a run line. Everything else on that line
// belongs to the agent, so this table is also the boundary: split consults it
// to find where brig's arguments stop, which is why it records whether a flag
// takes a value. Adding a flag here and registering it in parse are the two
// halves of adding a flag at all.
var brigFlags = []struct {
	long  string
	short string
	value bool // takes a value, so the next argument may belong to it
}{
	{long: "name", short: "n", value: true},
	{long: "image", short: "t", value: true},
	{long: "workspace", short: "w", value: true},
	{long: "memory", short: "m", value: true},
	{long: "cpus", value: true},
	{long: "detach", short: "d"},
	{long: "skills"},
}

// ours reports whether a token is one of brig's flags, and whether that flag
// consumes the argument after it. A token carrying its own value (--name=foo)
// consumes nothing further.
// The two documented spellings and no others. The flag package would answer to
// -name and --n as well, but brig forwards everything it does not own, so
// being greedier here silently eats an agent's own flag: `brig run claude
// -image x` has to stay the agent's -image.
func ours(arg string) (mine bool, takesValue bool) {
	name, _, inline := strings.Cut(arg, "=")
	for _, f := range brigFlags {
		if name == "--"+f.long || (f.short != "" && name == "-"+f.short) {
			return true, f.value && !inline
		}
	}
	return false, false
}

// split divides a run line into brig's own arguments, the profile name, and
// the agent's tail.
//
// This exists because the flag package stops at the first non-flag argument
// and treats an unknown flag as an error, and brig's line is the opposite on
// both counts: the profile name sits in the middle of brig's flags, and an
// unrecognised flag is not a mistake but the agent's -- `brig run claude -p hi`
// runs the agent with -p hi. So the boundary is decided here, from the table,
// and the flag package parses what is left of it.
func split(args []string) (mine []string, profileName string, tail []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			// An explicit end to brig's parsing, so an agent argument spelled
			// like one of brig's flags still reaches the agent.
			return mine, profileName, args[i+1:], nil
		case !strings.HasPrefix(a, "-"):
			if profileName == "" {
				profileName = a
				continue
			}
			// A second bare word is the agent's command, not a second profile.
			return mine, profileName, args[i:], nil
		default:
			isOurs, takesValue := ours(a)
			if !isOurs {
				// An unknown flag once the profile is named is the agent's:
				// `brig run claude --resume` runs the agent with --resume, and
				// brig forwards everything from here on. Before the profile is
				// named there is no agent yet to own it, so a flag brig does not
				// recognise in that position is not passed through -- it is a
				// brig flag that does not exist. Forwarding it would make the
				// profile look like the flag's value and leave the line blaming
				// the one word on it that was right, so refuse it and name it.
				if profileName == "" {
					return nil, "", nil, usagef("unknown flag %q before the profile name. "+
						"brig's own flags come before the profile and the agent's after it; "+
						"put %q after the profile to pass it through, or -- to end brig's flags", a, a)
				}
				return mine, profileName, args[i:], nil
			}
			mine = append(mine, a)
			if takesValue && i+1 < len(args) {
				// Taken without inspection: `--name -p` means a session
				// called -p, and second-guessing that would make the
				// value of a flag depend on what it happens to look like.
				i++
				mine = append(mine, args[i])
			}
		}
	}
	return mine, profileName, nil, nil
}

// rejectTail refuses a token a verb has no use for, rather than dropping it.
//
// run forwards the tail to the agent, and shell and exec turn it into the guest
// command, so those keep whatever follows the profile. create, stop, rm and env
// take a profile and nothing after it: a word left there is a mistake to name,
// not an operand to swallow. create is grouped with the others rather than with
// run because it does not attach -- split hands it the same agent tail run gets,
// but there is no agent here to receive it, so a tail is still a mistake.
func rejectTail(verb string, tail []string) error {
	switch verb {
	case "run", "shell", "exec":
		return nil
	}
	if len(tail) > 0 {
		return usagef("unexpected argument %q; `brig %s` takes a profile and nothing more", tail[0], verb)
	}
	return nil
}

// parse reads brig's own flags off the run line in any order and leaves
// whatever remains for the agent. split decides where brig's arguments end;
// the flag package turns them into values.
func parse(args []string) (o options, profileName string, tail []string, err error) {
	mine, profileName, tail, err := split(args)
	if err != nil {
		return options{}, "", nil, err
	}

	fs := flag.NewFlagSet("brig", flag.ContinueOnError)
	// The flag package's own output is a usage dump on every error. brig's
	// errors name the flag and what it wanted instead, and main prints them.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	var mem, cpus number
	for _, f := range []struct {
		long, short string
		fn          func(name string)
	}{
		{"name", "n", func(n string) { fs.StringVar(&o.load.Name, n, "", "") }},
		{"image", "t", func(n string) { fs.StringVar(&o.load.Image, n, "", "") }},
		{"workspace", "w", func(n string) { fs.StringVar(&o.load.Workspace, n, "", "") }},
		{"memory", "m", func(n string) { fs.Var(numberFlag{spell(n), &mem}, n, "") }},
		{"cpus", "", func(n string) { fs.Var(numberFlag{spell(n), &cpus}, n, "") }},
		{"detach", "d", func(n string) { fs.BoolVar(&o.detach, n, false, "") }},
		// Opt-in, and only ever opt-in: this hands the guest the user's real
		// skills and plugins, read-only.
		{"skills", "", func(n string) { fs.BoolVar(&o.load.Skills, n, false, "") }},
	} {
		// Both spellings write the same variable, so whichever the user typed
		// lands in one place and the last one on the line wins.
		f.fn(f.long)
		if f.short != "" {
			f.fn(f.short)
		}
	}

	if err := fs.Parse(mine); err != nil {
		return options{}, "", nil, rewriteFlagError(err)
	}
	// Positive, not merely numeric: a guest with zero CPUs is a boot failure
	// several layers down, where the number no longer appears in the message.
	// Decimal, too: --memory 010 asks for ten, and a guest a fifth of the size
	// boots without a word about why.
	for _, c := range []struct {
		got *number
		dst *int
	}{{&mem, &o.load.Mem}, {&cpus, &o.load.CPUs}} {
		if !c.got.set {
			continue
		}
		n, convErr := strconv.Atoi(c.got.raw)
		if convErr != nil || n <= 0 {
			// One message for both, quoting the argument as written: a value
			// that is not a number and one that is not a useful number are the
			// same mistake to whoever has to spot their own input in it.
			return options{}, "", nil, fmt.Errorf("%s needs a positive number, not %q",
				c.got.as, c.got.raw)
		}
		*c.dst = n
	}
	// An empty name is tracked apart from the value, because `--name ''`
	// otherwise reads exactly like passing no flag and would quietly run the
	// unnamed sandbox.
	o.nameGiven = seen(fs, "name", "n")
	if o.nameGiven && o.load.Name == "" {
		return options{}, "", nil, errors.New("--name needs a session name, for example `--name foo`")
	}
	return o, profileName, tail, nil
}

// number is one of brig's numeric flags as it was given: the text typed, and
// the spelling it was typed under. The flag package keeps neither -- IntVar
// hands back a parsed int, in which -0, 0x0 and 0 are all indistinguishable,
// and it names a flag by whichever spelling it was registered under rather
// than the one on the line. Both are what the error about a bad value has to
// quote back, so the parsing waits until parse has them.
type number struct {
	set     bool
	raw, as string
}

// numberFlag binds one spelling of a numeric flag to the value behind it. The
// two spellings share a number, so the last one on the line still wins, and it
// is the one the error names.
type numberFlag struct {
	as  string
	dst *number
}

func (f numberFlag) String() string {
	if f.dst == nil {
		return ""
	}
	return f.dst.raw
}

func (f numberFlag) Set(raw string) error {
	*f.dst = number{set: true, raw: raw, as: f.as}
	return nil
}

// seen reports whether a flag was given under any of its spellings.
//
// Visit walks only what was set, which is the distinction a zero value cannot
// carry: an empty --name and no --name at all are different lines that mean
// different things.
func seen(fs *flag.FlagSet, spellings ...string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		for _, s := range spellings {
			if f.Name == s {
				given = true
			}
		}
	})
	return given
}

// rewriteFlagError puts brig's wording on the flag package's errors, which
// name the flag with one dash however it was typed.
func rewriteFlagError(err error) error {
	msg := err.Error()
	// "flag needs an argument: -image"
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: "); ok {
		return fmt.Errorf("%s needs a value", spell(name))
	}
	// `invalid boolean value "garbage" for -detach: parse error`. The flags
	// this can name are the ones written bare, so say that rather than leave
	// the reader to guess what a valid one looks like.
	if rest, ok := strings.CutPrefix(msg, "invalid boolean value "); ok {
		// From the right: the value is quoted into the message ahead of the
		// separator, so a value that contains " for " itself would tear the
		// message in half at its own text.
		raw, tail := rest, ""
		if i := strings.LastIndex(rest, " for "); i >= 0 {
			raw, tail = rest[:i], rest[i+len(" for "):]
		}
		name, _, _ := strings.Cut(tail, ":")
		return fmt.Errorf("%s is either given or not, so it takes true or false, not %s",
			spell(name), raw)
	}
	return err
}

// spell writes a flag the way the usage text does: one dash for the short
// form, two for the long one. The flag package prints a single dash whichever
// was typed, which turns --memory into -memory in the error for it.
func spell(name string) string {
	name = strings.TrimLeft(name, "-")
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// listSandboxes shows what is running, and what is merely holding a name. A
// stopped sandbox still owns its name, which is exactly the thing to see
// before wondering why a name is taken.
func listSandboxes(args []string) error {
	// ls names no profile and takes no flags, so anything here is a token it
	// would otherwise read and discard -- `brig ls claude` looks like it filters
	// to one profile and does not. Refuse it rather than answer a question that
	// was not asked.
	if len(args) > 0 {
		return usagef("unexpected argument %q; `brig ls` takes no arguments", args[0])
	}
	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	list, err := rt.List()
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	fmt.Printf("%-28s %-10s %s\n", "SANDBOX", "STATE", "WORKSPACE")
	shown := 0
	for _, inst := range list {
		if !strings.HasPrefix(inst.Name, sandboxPrefix) {
			continue
		}
		shown++
		fmt.Printf("%-28s %-10s %s\n", inst.Name, inst.State, workspaceOf(inst.Name, rt))
	}
	if shown == 0 {
		fmt.Println("(none -- `brig run claude` starts one)")
	}
	return nil
}

// workspaceOf recovers the workspace for a sandbox by asking the profile it
// was named after. A sandbox brig did not name has none to report.
func workspaceOf(vmName string, rt runtime.Runtime) string {
	rest := strings.TrimPrefix(vmName, sandboxPrefix)
	// Longest profile name first, so claude-code wins over a hypothetical
	// claude when both could prefix-match.
	names := profile.Names()
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		if rest != name && !strings.HasPrefix(rest, name+"-") {
			continue
		}
		t, _ := profile.Lookup(name)
		// An unnamed sandbox is named exactly after its profile, so there is
		// no session name to recover. TrimPrefix would leave the profile
		// name intact here and we would report the workspace of a session
		// called "claude-code", which is not the one running.
		session := ""
		if rest != name {
			session = strings.TrimPrefix(rest, name+"-")
		}
		cfg, err := wrap.Load(t, wrap.Options{Name: session}, rt)
		if err != nil {
			return ""
		}
		return cfg.Workspace
	}
	return ""
}

// reset stops and removes every sandbox brig started. Workspaces are left
// alone: they are on the host, they hold your work, and this is a command
// about sandboxes.
func reset(args []string) error {
	// reset stops and removes every brig sandbox, so a flag typed to make it
	// safer -- `brig reset --dry-run` -- must not be read past and ignored,
	// which is exactly how a command meant to preview ends up removing
	// everything. It has no flags and no operands today; refuse anything here so
	// a flag that gains a meaning later is one this release declined rather than
	// silently swallowed.
	if len(args) > 0 {
		return usagef("unexpected argument %q; `brig reset` takes no arguments and removes "+
			"every brig sandbox", args[0])
	}
	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	list, err := rt.List()
	if err != nil {
		return err
	}
	removed := 0
	for _, inst := range list {
		if !strings.HasPrefix(inst.Name, sandboxPrefix) {
			continue
		}
		_ = rt.Stop(inst.Name)
		if err := rt.Remove(inst.Name); err != nil {
			fmt.Fprintf(os.Stderr, "brig: could not remove %s: %v\n", inst.Name, err)
			continue
		}
		fmt.Println(inst.Name)
		removed++
	}
	fmt.Fprintf(os.Stderr, "brig: removed %d sandbox(es). Workspaces are untouched.\n", removed)
	return nil
}

// listProfiles prints the merged set: embedded and file-backed together, one
// namespace. It is a view of what brig will actually run, not a directory
// listing -- so it says where each profile came from, because "the image is
// not what I expected" and "there is a file I forgot about" are the same
// question.
func listProfiles() error {
	for _, p := range profile.All() {
		suffix := ""
		switch {
		case profile.OverridesBuiltIn(p.Name):
			suffix = "  (file, overrides built-in)"
		case profile.IsCustom(p.Name):
			suffix = "  (file)"
		}
		if p.Unpublished {
			suffix += "  (no published image)"
		}
		fmt.Printf("%-15s %s%s\n", p.Name, p.Desc, suffix)
		fmt.Printf("%-15s image %s, home %s\n", "", p.Image, p.GuestHome)
		if len(p.Env) > 0 {
			names := make([]string, 0, len(p.Env))
			for _, b := range p.Env {
				names = append(names, b.Name)
			}
			fmt.Printf("%-15s environment: %s\n", "", strings.Join(names, " "))
		}
		// Listed separately from the bindings above because these are the ones
		// you have to create before the sandbox will start at all.
		if len(p.Secrets) > 0 {
			fmt.Printf("%-15s secrets: %s\n", "", strings.Join(profile.SecretNames(p.Secrets), " "))
			// Which ones brig can fill from your host and which ones only you
			// can: the old single line said "create them" for both, which for
			// an importable secret is the long way round.
			//
			// Both lines are the command plus the names it covers, which is
			// what makes them comparable at a glance. Import takes the PROFILE
			// -- one command fills every importable name on this line -- while
			// create takes a name at a time.
			if importable := importableNames(p); len(importable) > 0 {
				fmt.Printf("%-15s   from your host: brig secret import %s (%s)\n", "",
					p.Name, strings.Join(importable, " "))
			}
			if hand := handCreatedNames(p); len(hand) > 0 {
				fmt.Printf("%-15s   by hand: brig secret create <name> (%s)\n", "",
					strings.Join(hand, " "))
			}
		}
		if len(p.Deny) > 0 {
			fmt.Printf("%-15s never forwarded: %s\n", "", strings.Join(p.Deny, " "))
		}
	}
	fmt.Printf("\nan unmarked profile is built in; your own live in %s\n", profile.Dir())
	fmt.Printf("to override one:  brig profile export claude-code claude-code, then brig profile edit claude-code\n")
	fmt.Printf("to add your own:  brig profile export claude-code mytool, then brig profile edit mytool (change name:)\n")
	fmt.Printf("to build an image for one: %s\n", profile.BringYourOwnImageDoc)
	return nil
}

// importableNames and handCreatedNames split a profile's requirement list the
// way the two commands that fill it do. A secret with no sources is
// hand-created by definition, so the listing points at the command that can
// actually supply it rather than at whichever one the reader tries first.
func importableNames(p profile.Profile) []string {
	var names []string
	for _, d := range p.Secrets {
		if d.Importable() {
			names = append(names, d.Name)
		}
	}
	return names
}

func handCreatedNames(p profile.Profile) []string {
	var names []string
	for _, d := range p.Secrets {
		if !d.Importable() {
			names = append(names, d.Name)
		}
	}
	return names
}

// profileCmd groups the profile verbs, which is where someone coming from
// another sandbox tool will look for them.
func profileCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("profile needs a subcommand: ls, export, import, edit or rm")
	}
	switch args[0] {
	case "ls", "list":
		return listProfiles()
	case "export", "save":
		return exportProfile(args[1:])
	case "import", "load":
		return importProfile(args[1:])
	case "edit":
		return editProfile(args[1:])
	case "rm":
		return removeProfile(args[1:])
	default:
		return fmt.Errorf("unknown profile subcommand %q (ls, export, import, edit, rm)", args[0])
	}
}

// editProfile opens a file-backed profile in your editor.
//
// It creates nothing: a profile that is still embedded has no file, so edit
// says so and names the command that would make one. Materialising a copy
// instead would mean quitting the editor unchanged left a file identical to
// the built-in -- an override in every mechanical sense, frozen at that
// version and no longer picking up a deny entry added in a later release.
func editProfile(args []string) error {
	if len(args) == 0 {
		return errors.New("profile edit needs a name, for example `brig profile edit mine`")
	}
	name := args[0]
	p, ok := profile.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown profile %q. `brig profiles` lists them", name)
	}
	path, ok := profile.Path(p.Name)
	if !ok {
		return fmt.Errorf("%s is built in, so there is no file to edit.\n"+
			"To override it:\n"+
			"  brig profile export %s %s\n"+
			"  brig profile edit %s",
			p.Name, p.Name, p.Name, p.Name)
	}

	editor := editorCommand()
	cmd := exec.Command(editor[0], append(editor[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", editor[0], err)
	}

	// Check what was saved, and leave it alone whatever the answer. brig never
	// discards someone's edits to make its own state tidy: a non-zero exit and
	// the parser's own message are enough for a person to fix it, and the next
	// invocation reports it as an unusable profile and carries on with the
	// others.
	if _, err := profile.Read(path); err != nil {
		return fmt.Errorf("%s no longer parses, and has been left as you saved it:\n  %w",
			path, err)
	}
	fmt.Printf("%s updated\n", path)
	return nil
}

// editorCommand is the editor to run, and any arguments it was given: VISUAL,
// then EDITOR, then vi. VISUAL first is the convention, and honouring the
// arguments is what makes `code -w` work -- without the wait flag the editor
// returns immediately and brig would validate a file nobody had edited yet.
func editorCommand() []string {
	for _, v := range []string{os.Getenv("VISUAL"), os.Getenv("EDITOR")} {
		if fields := strings.Fields(v); len(fields) > 0 {
			return fields
		}
	}
	return []string{"vi"}
}

// importProfile adds a profile of your own. Reading from - lets it come out of
// a pipe, which is what makes `brig export x | edit | brig import -` work.
func importProfile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("import needs a file, for example `brig profile import mine.yaml` "+
			"(or - to read stdin). See %s", profile.BringYourOwnImageDoc)
	}
	var blob []byte
	var err error
	if args[0] == "-" {
		blob, err = io.ReadAll(os.Stdin)
	} else {
		blob, err = os.ReadFile(args[0])
		// `brig import claude` is someone reaching for `brig secret import
		// claude`: this verb takes a file, that one takes a profile, and the
		// word they typed is a profile that already exists. Saying so costs
		// one branch, and the alternative is "no such file or directory" for
		// a command that was nearly right.
		if os.IsNotExist(err) {
			if p, ok := profile.Lookup(args[0]); ok {
				return fmt.Errorf("there is no file %q, and %s is a profile brig already has. "+
					"To fill its secrets from your host: brig secret import %s",
					args[0], p.Name, p.Name)
			}
		}
	}
	if err != nil {
		return err
	}
	dir := profile.Dir()
	p, path, err := profile.Import(blob, dir)
	if err != nil {
		return fmt.Errorf("%w\n\nA profile needs at least a name, an image, a guest home, "+
			"a binary, mem and cpus. `brig profile export claude-code` prints a working one "+
			"to start from, and %s explains how to build the image", err, profile.BringYourOwnImageDoc)
	}
	fmt.Printf("imported %s -> %s\n", p.Name, path)
	fmt.Printf("run it with: brig run %s\n", p.Name)
	if !strings.HasPrefix(p.Image, "ghcr.io/brig-sh/") {
		fmt.Printf("note: %s is not one of our images, so brig cannot verify its "+
			"signature. It will say so on every boot.\n", p.Image)
	}
	return nil
}

// exportProfile prints a profile, or writes it to a file. YAML by default,
// because the result is meant to be edited; --json for anything consuming it
// programmatically.
//
// A destination is a name, never a path: `brig profile export codex mine`
// writes mine.yaml into the profile directory, because that is the only place
// a profile file does anything. See profile.ExportPath, which owns the rule.
//
// With no destination it prints, which is what keeps
// `brig profile export x | brig profile import -` working, what stops the
// command writing anything you did not ask for, and how you get a copy
// anywhere else: redirect it.
// Unlike the run line, nothing here passes through to another program, so an
// unrecognised flag is a mistake rather than someone else's argument and the
// flag package can reject it outright. Without that, a mistyped flag falls
// through to the destination and export writes a file called --jsonn.
func exportProfile(args []string) error {
	asJSON, force := false, false
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&asJSON, "json", false, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")

	// Parse stops at the first bare word, so a flag written after the
	// destination -- `export codex mine --json`, the order the docs use --
	// would otherwise be left sitting in Args. Lift the word and parse on.
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			// The unknown flag is export's own case: on the run line one never
			// reaches the flag package, because it belongs to the agent. Every
			// other error the parser can raise here is one the run line already
			// says in brig's voice, so say it the same way.
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(name)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return fmt.Errorf("%s (export takes --json and --force)", msg)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	var name, dest string
	switch len(words) {
	case 0:
	case 1:
		name = words[0]
	case 2:
		name, dest = words[0], words[1]
	default:
		return fmt.Errorf("export takes a profile and at most one destination, not %q", words[2])
	}
	if name == "" {
		return errors.New("export needs a profile, for example `brig profile export claude-code`")
	}
	p, ok := profile.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown profile %q. `brig profiles` lists them", name)
	}
	render := profile.Export
	if asJSON {
		render = profile.ExportJSON
	}
	blob, err := render(p)
	if err != nil {
		return err
	}
	if dest == "" {
		_, err = os.Stdout.Write(blob)
		return err
	}
	path, err := profile.ExportPath(dest, asJSON)
	if err != nil {
		return err
	}
	// An export is generated bytes: it has none of the edits that are the
	// whole reason the file exists, so overwriting one silently is how an
	// afternoon of tuning a deny list disappears. --force is the way to say
	// you meant it.
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists. Edit it where it is, or pass --force "+
				"to replace it with a fresh export of %s", path, p.Name)
		}
	}
	// The directory may not exist yet: `brig profile edit` points a first-time
	// user straight here, and on a fresh install nothing has created it. 0700
	// per the XDG spec, and the right mode for files naming credentials.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s -> %s\n", p.Name, path)
	if profile.Embedded(p.Name) {
		fmt.Printf("it is in your profile directory, so it now overrides the built-in %s\n", p.Name)
	}
	// brig keys on the name field, not the file name, so a file called
	// mine.yaml that still says "name: codex" is an override of codex however
	// it is spelled. Say so where it is fixable, which is before the edit
	// rather than after two of them fight over one name.
	if stem := stemOf(path); stem != p.Name {
		fmt.Printf("brig reads the name: field, not the file name, so this is still "+
			"the %s profile. Change name: to %s if you meant a profile of your own\n",
			p.Name, stem)
	}
	return nil
}

// stemOf is a file's base name without its extension, which is what a profile
// file would be called if it were named after the profile it declares.
func stemOf(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// removeProfile deletes a profile of your own. A built-in is compiled in and
// cannot be removed, only shadowed -- say so rather than reporting a missing
// file.
func removeProfile(args []string) error {
	if len(args) == 0 {
		return errors.New("profile rm needs a name")
	}
	// Resolve through the registry rather than trusting the argument: it takes
	// aliases, and it names the file actually loaded, which need not be
	// <name>.yaml -- `brig profile export claude-code pinned.yaml` is an
	// override whose basename says nothing about which profile it serves.
	p, ok := profile.Lookup(args[0])
	if !ok {
		return fmt.Errorf("no profile of your own named %q", args[0])
	}
	if _, ok := profile.Path(p.Name); !ok {
		return fmt.Errorf("%s is a built-in profile, so there is nothing to remove. "+
			"Import a profile of the same name to shadow it", p.Name)
	}
	// Every file that declares the name, not just the one that loaded: see
	// profile.Remove. Two of them is a mistake brig reports at load time and
	// does not otherwise prevent, and an rm that leaves the profile listed is
	// worse than the mistake.
	removed, err := profile.Remove(p.Name)
	for _, f := range removed {
		fmt.Printf("removed %s\n", f)
	}
	return err
}

// deprecated notes an old spelling once, on stderr so it never lands in
// something being piped. Kept for one release: one commit of published history
// is not long enough to have broken anyone's muscle memory on purpose.
func deprecated(old, replacement string) {
	fmt.Fprintf(os.Stderr, "brig: `%s` is now `%s`\n", old, replacement)
}

// warnDeprecatedProfileKeys says that a profile FILE still carries
// hostCredential:, which reads another application's keychain on every run and
// goes in the next release.
//
// Only for file-backed profiles, and that scoping is the whole point: no
// built-in carries the key any more, so an unscoped check would have brig warn
// about its own shipped spec on every command -- a warning the reader cannot
// act on, which is how people learn to ignore the ones they can.
//
// Emitted here, beside LegacyHint, because this is where profiles are loaded
// and it is the same class of thing: a file that still parses and no longer
// means what it did. A run that never names the profile still hears it once,
// which is right -- the file is what needs editing, not the run.
func warnDeprecatedProfileKeys() {
	for _, p := range profile.All() {
		if p.HostCredential == nil || !profile.IsCustom(p.Name) {
			continue
		}
		where := p.Name
		if path, ok := profile.Path(p.Name); ok {
			where = path
		}
		fmt.Fprintf(os.Stderr, "brig: hostCredential: in %s is deprecated and goes in the "+
			"next release -- it reads another application's keychain on every run. "+
			"Declare the credential under secrets: with sources: instead, then: "+
			"brig secret import %s\n", where, p.Name)
	}
}

// isTerminal reports whether stdin is a tty, which decides whether the guest
// exec allocates one. A headless `-p` run piped from a script must not get a
// pseudo-terminal.
func isTerminal() bool { return wrap.IsTerminal(os.Stdin) }
