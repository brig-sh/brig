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
	"github.com/brig-sh/brig/internal/session"
	"github.com/brig-sh/brig/internal/wrap"
)

// version is stamped at build time by goreleaser.
var version = "dev"

// sandboxPrefix is how brig recognises its own sandboxes in a runtime that
// may be running other things. It is the same mark wrap stamps onto every
// sandbox name, taken from there so the two cannot drift: ls and `rm --all`
// select on exactly what Load refuses to let BRIG_NAME drop.
const sandboxPrefix = wrap.NamePrefix

const usage = `brig -- run a coding agent in a sandbox

usage:
  brig run  <ref> [project] [args...]            start the sandbox and run it. A
                                                 project is mounted at
                                                 /work/<name>, and the agent
                                                 starts there
  brig sh   <ref> [command...]                   a login shell inside the sandbox,
                                                 or one command in it
  brig stop <ref>                                stop the sandbox, keep it
  brig rm   <ref>                                stop and remove the sandbox
  brig rm   --all                                stop and remove every brig sandbox
  brig ls   [-q]                                 list sandboxes; -q prints the refs
  brig logs <ref> [--follow] [--tail N] [--raw]  stream the sandbox log (--gateway: that sandbox's gateway log; alone: the shared one)
  brig info <ref>                                print the execution envelope and the
                                                 full environment, by name -- fails
                                                 if a declared secret is missing
  brig agent ls|show|new|edit|rm|import|export   the agents you can run
  brig policy ls|create|edit|show|rm             manage policies
  brig policy attach|detach <policy> <profile>   bind or unbind a policy,
                                                 [-n NAME] for one session
                                                 instead of every run
  brig policy check <profile> [-n NAME]          what is bound to a run, and
                                                 whether brig can enforce it
  brig secret create|read|update|delete|ls       keep secrets in your keyring
  brig secret import <profile>                   fill a profile's secrets from
                                                 your host, once
  brig telemetry status|on|off                   report what is counted, or
                                                 turn the counting on or off
  brig version

A <ref> is the session. claude is that agent's default session, and
claude@refactor is a session of its own -- its own workspace, its own sandbox,
and the label reaching the agent as its display name. A label brig would have
to rewrite is refused rather than rewritten. brig ls prints the ref of every
sandbox, and every verb above takes one.

global flags (left of the command, as in: brig -q run claude):
      --verbose          the execution envelope, brig's own progress and the
                         runtime's own output. No short form: -v belongs to
                         the agents
  -q, --quiet            identifiers and errors only, for a script. It drops
                         brig's warnings; with ls it prints the refs, one per
                         line. A verification that did not hold is printed
                         even here
                         (-q after the verb still works this release)

flags (before the agent's own arguments; -- ends brig's parsing):
      --image IMAGE      guest image to boot
      --home PATH        host directory to mount as the guest home
                         (-w and --workspace still work, with a note)
      --no-project       with run: this session's project is not mounted,
                         whatever it ran with last
      --mem MB           guest memory
      --cpus N           guest vCPUs
  -d, --detach           with run: start the sandbox and exit
      --skills           project your own ~/.claude skills and plugins into
                         the guest, read-only (or BRIG_SKILLS=1)
      --network MODE     shared, isolated or offline (or BRIG_NETWORK)
      --offline          shorthand for --network offline: the agent runs, the
                         workspace is there, nothing leaves

By default a run prints what you have to act on, then the agent: warnings,
errors, and one line saying verification held. The execution envelope, brig's
own progress and the runtime's output wait for --verbose -- and a boot that
fails quotes what the runtime said whether or not you asked. brig info prints
the envelope on demand, without booting anything.

Workspaces persist. The sandbox keeps running between commands, so a second
run is immediate; state lives in the workspace on the host either way.

Any Linux CLI in an OCI image runs under brig, if the image also carries the
utilities brig uses to set the sandbox up and deliver the credential: a shell
(sh and bash), plus cat, stat, chown, mkdir, mount and /bin/true. A stock
distro image has them; a scratch image with only your binary does not. An
agent entry just saves you spelling out the image and its credential variables
every time: copy the closest one and edit it, with
  brig agent new mine --from claude, then brig agent edit mine
Building an image for one is documented at
  https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md

settings (BRIG_<AGENT>_<KEY> wins over BRIG_<KEY>; see the README for all):
  BRIG_WORKSPACE       host directory mounted as the guest home
  BRIG_IMAGE           guest image
  BRIG_PULL            missing (default) | always | never
  BRIG_SKILLS          1 to project your ~/.claude skills and plugins read-only
  BRIG_FORWARD_ENV     replaces the env-sourced bindings, space-separated
  BRIG_GIT_CONFIG      1 to write the guest git-over-HTTPS files
  BRIG_VERIFY          warn (default) | require | off -- guest image signature
  BRIG_PROFILE_DIR     where your own profiles live (BRIG_TEMPLATE_DIR still works)
  BRIG_RUNTIME         hull | nerdctl
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
		// The exit status is a stable, documented set: a script can tell "you
		// asked for the wrong thing" from "it ran and failed" from "the sandbox
		// could not be verified" without parsing the message. exitCode owns the
		// mapping; the README documents it. A run refused for any reason still
		// exits non-zero, so a stop or a boot that removed or started nothing
		// never reads as success.
		os.Exit(exitCode(err))
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

// notFoundError is a name that resolves to nothing: a profile brig does not
// have, or a sandbox that is not there. It carries its own exit code so a
// script can tell "no such thing" apart from a run that started and failed.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

// notFoundf builds a notFoundError the way fmt.Errorf builds an ordinary one.
func notFoundf(format string, a ...any) error { return &notFoundError{fmt.Sprintf(format, a...)} }

func run(args []string) error {
	// The global position first, so a flag written left of the verb is named
	// rather than read as a command.
	g, verbLine, err := parseGlobal(args)
	if err != nil {
		return err
	}
	// And how much this invocation says, before anything says it. That is what
	// the global position buys: `brig -q run claude` is quiet from the first
	// line, the notices about profiles below included. The run-line spelling of
	// -q is read much later -- it sits on the far side of the ref -- so those
	// notices have already printed by the time it is known, which is one more
	// reason the flag has moved.
	verbosity = g.verbosity()
	if len(verbLine) == 0 {
		// No verb: a bare `brig`, or global flags and nothing to do with them.
		fmt.Print(usage)
		return nil
	}
	verb, rest := verbLine[0], verbLine[1:]

	// Profiles are read before anything looks a name up, so a file can stand
	// in for a built-in. A broken file is reported and skipped rather than
	// taking down the profile you were actually asking for.
	if err := profile.Load(profile.Dir()); err != nil {
		warnf("%s", err)
	}
	// Files left in the pre-profile directory are read by nothing and look
	// exactly like files that work: the profile they were pinning silently
	// reverts to brig's own. Nothing is moved for you -- these name credential
	// variables, and there is no safe guess about which you still want.
	if hint := profile.LegacyHint(); hint != "" {
		warnf("%s", hint)
	}
	warnDeprecatedProfileKeys()

	switch verb {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Printf("brig %s\n", version)
		return nil
	case "agent":
		return agentCmd(rest)
	case "policy":
		return policyCmd(rest)
	case "secret":
		return secretCmd(os.Stdout, rest)
	case "telemetry":
		return telemetryCmd(os.Stdout, rest)
	case "logs":
		return logsCmd(rest)
	// Deprecated spellings, absent from the usage text.
	//
	// The three grammars this release settles are all here: a plural noun that
	// is a command of its own, a noun command spelled again at the top level,
	// and a group renamed. Each keeps working and says the one word that
	// replaces it, because a spelling has to survive the commit that renames it
	// or that commit breaks every script anyone has written.
	case "profiles":
		deprecated("brig profiles", "brig agent ls")
		return listProfiles()
	case "profile":
		return deprecatedProfileCmd(rest)
	case "policies":
		deprecated("brig policies", "brig policy ls")
		return listPolicies()
	case "import":
		deprecated("brig import", "brig agent import")
		return importProfile(rest)
	case "export":
		// Named onto `brig agent export` rather than onto show or new, because
		// this is the same command: it prints with no destination and writes
		// with one, and the reader's line keeps working word for word. show and
		// new are the taught spellings of those two halves, and the usage text
		// is where someone meets them.
		deprecated("brig export", "brig agent export")
		return exportProfile(rest)
	// There is deliberately no `brig template edit`: the old group carries only
	// the verbs it already had, so a command that never existed under that name
	// does not gain one.
	case "agents":
		deprecated("brig agents", "brig agent ls")
		return listProfiles()
	case "template":
		deprecated("brig template", "brig agent")
		if len(rest) > 0 && rest[0] == "edit" {
			return fmt.Errorf("there is no `brig template edit`; use `brig agent edit`")
		}
		return agentCmd(rest)
	case "ls":
		// The global -q reaches ls as its own meaning: refs and nothing else.
		// The two readings agree -- bare refs ARE identifiers only -- and ls
		// still reads -q after the verb itself, which is the spelling the
		// round-trip test consumes.
		return listSandboxes(rest, verbosity <= wrap.Quiet)
	case "reset":
		// reset was the one verb whose name did not say what it acts on, which
		// is the whole of its problem: it removes every sandbox brig has, and it
		// was spelled as if it were a setting being restored. --all is that
		// word, on the verb that already removes one.
		deprecated("brig reset", "brig rm --all")
		return removeAll("brig reset", rest)
	case "rm":
		// --all is read here rather than on the run line, because it names no
		// session: split would refuse it as a brig flag standing where brig's
		// own flags go, and it is not one. Without --all this falls through to
		// the run line and removes the one sandbox the ref names.
		if others, all := takeAll(rest); all {
			return removeAll("brig rm --all", others)
		}
	case "run", "sh", "stop", "info":
		// The taught lifecycle spellings. They fall through to the run line
		// below, which is where the ref and the flags are read.
	case "create":
		// create is `run -d`: start the sandbox, print its name, attach to
		// nothing. It keeps a branch of its own below rather than being
		// rewritten into run here, because run hands a trailing word to the
		// agent and create has no agent to hand one to -- a word after the ref
		// is still a mistake, and translating the verb would have swallowed it.
		deprecated("brig create", "brig run -d")
	case "exec", "shell":
		// Two verbs for one question: exec ran a command, shell opened a login
		// shell or ran one. sh is both, because which of the two you want is
		// said by whether you typed a command, not by which word you reached
		// for.
		//
		// Both keep their own branch below rather than being translated to sh.
		// exec runs its argv directly where sh runs it through `bash -lc`, so a
		// script that relies on its own quoting keeps it -- a rename must not
		// change what a working line does.
		deprecated("brig "+verb, "brig sh")
	case "env":
		// Kept for one release as a spelling of `brig info`. The bug report
		// template used to send reporters to `brig status`, which was never a
		// command; info is the name that work settled on.
		//
		// Said here rather than beside the report it prints, so the notice
		// arrives whether or not the profile behind it resolves. What is retired
		// is the word, and the word is known before anything is looked up.
		deprecated("brig env", "brig info")
	default:
		// A bare ref with no verb, and it is tried last on purpose. A token
		// becomes a ref only once it has matched no verb, so a verb is always a
		// verb: install an agent called `ls` and `brig ls` is still the listing.
		// Read the other way round, brig's own vocabulary would depend on which
		// agents happen to be on the host, and the same command line would mean
		// two things on two machines.
		//
		// Deliberately in no help text and no example. Two spellings of one
		// thing is what the rest of this change is undoing; this one exists
		// because `brig claude@refactor` is what people type, not because brig
		// is teaching it.
		ref, refErr := session.ParseRef(verb)
		_, known := profile.Lookup(ref.Agent)
		if (refErr != nil || !known) && !session.IsRefShaped(verb) {
			// A bare word that failed is reported as a command and not as a
			// ref. The reader typed it where a command goes, and it is a
			// mistyped verb as readily as a mistyped agent -- so the ref
			// parser's complaint would answer a question nobody asked.
			return fmt.Errorf("unknown command %q (try `brig help`)", verb)
		}
		// A token carrying the separator is a ref that did not work, because
		// nothing else it could be has an '@' in it. Answering that with the
		// vocabulary of commands would send the reader looking for a verb they
		// did not type, past a diagnosis that already names what is wrong with
		// the ref.
		if refErr != nil {
			return refErr
		}
		// An agent brig does not have falls through to the run line rather
		// than being reported here, which is where the verbed form reports it
		// too. One message, not two spellings of one that have to be kept in
		// step.
		verb, rest = "run", verbLine
	}

	opts, profileName, tail, err := parse(verb, rest)
	if err != nil {
		return err
	}
	// Both at once asks for two contradictory things, and either winner would
	// be silent about the other: the directory would be mounted with
	// --no-project ignored, or dropped with the word the line names ignored.
	if opts.load.Project != "" && opts.load.NoProject {
		return usagef("--no-project and the project %q ask for different things; "+
			"drop one", opts.load.Project)
	}
	// --no-project is run's, like the positional it answers. Elsewhere it is a
	// flag that does nothing, which is worse than one that is refused.
	if opts.load.NoProject && verb != "run" {
		return usagef("--no-project belongs to `brig run`, not `brig %s`. "+
			"A session runs without its project from the next `brig run --no-project` onwards", verb)
	}
	// The run-line spelling of -q, for the one release in which both work. It
	// is read here rather than in the global position, so everything above has
	// already printed: that asymmetry is the notice's point, not a gap in it.
	if opts.quiet {
		verbosity = wrap.Quiet
	}
	if opts.load.Project != "" {
		warnPositionalMeaning(opts.load.Project)
	}
	if err := rejectTail(verb, tail); err != nil {
		return err
	}
	t, ok := profile.Lookup(profileName)
	if !ok {
		if profileName == "" {
			return fmt.Errorf("%s needs a profile, for example `brig %s claude`. "+
				"`brig agent ls` lists them", verb, verb)
		}
		return notFoundf("unknown profile %q. `brig agent ls` lists them", profileName)
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
		// brig info is the report worth giving when the runtime is the thing
		// that is broken: only one of its lines comes from the runtime, and the
		// person most likely to run it is the one whose runtime is not on PATH.
		// So it carries on without one, and the report marks that single line
		// unavailable. env is the old spelling of the same command and gets the
		// same treatment, or the spelling brig recommends would be the one that
		// fails. Every other verb needs the runtime to do its work, so they
		// still fail here, naming what is missing.
		//
		// But only "no runtime on PATH" is a state the report should paper
		// over. An unknown BRIG_RUNTIME or a runtimeBin that is not there is a
		// mistake to fix, and this is the verb people run to find it -- so those
		// surface here as they do for run, naming the real cause. Match the
		// sentinel, not any error, or a future error type silently rejoins the
		// swallow.
		reports := verb == "info" || verb == "env"
		if !reports || !errors.Is(err, runtime.ErrNoRuntime) {
			return err
		}
		rt = nil
	}
	opts.load.Verbosity = verbosity
	cfg, err := wrap.Load(t, opts.load, rt)
	if err != nil {
		return err
	}
	// The slug is sanitised, so the directory a named session gets does not
	// always read back as the name typed. Say which directory it actually is
	// whenever the two differ. A name that sanitises onto another session's
	// sandbox is refused later, at EnsureRunning; this only names the directory.
	if cfg.RawName != "" && cfg.RawName != cfg.Slug {
		warnf("session %q uses %s (sandbox %s)", cfg.RawName, cfg.Workspace, cfg.VMName)
	}

	// Stopping and removing need nothing but the instance name, and are
	// handled ahead of any credential resolution so they cannot raise the
	// keychain prompt that reading the host login may bring up.
	switch verb {
	case "stop":
		// Deliberately not gated on the sandbox existing: stopping one that was
		// never running is the end state stop asks for, not a failure. See
		// Config.Stop. rm and logs below are the opposite -- there is nothing to
		// remove and nothing to read -- which is why only they check first.
		return cfg.Stop()
	case "rm":
		return removeSandbox(cfg, session.Ref{Agent: profileName, Label: cfg.RawName}.String())
	}

	set, err := cfg.BuildEnv()
	if err != nil {
		return err
	}

	// The execution envelope: the boundary this run is about to trust, printed
	// before the sandbox boots so the user sees it before it matters.
	//
	// Behind --verbose. Nine rows of boundary before the agent says anything is
	// the machinery a quiet default is meant to remove. It is not
	// gone and it is not harder to reach -- `brig info` is the command whose
	// whole output it is, it answers without booting anything, and --verbose
	// puts it back on a run. Do not restore it to the default: that is the
	// user's decision, not an oversight.
	//
	// What a default run still says about the boundary is the part nobody can
	// be asked to go and look up: a verification that did not hold prints at
	// every level, and one that did prints above -q. See alertf and
	// sayVerified.
	//
	// Only run and create, as before. sh continues an existing session, and
	// when there is no sandbox yet it lets EnsureRunning start one without
	// printing the block, so a scripted `brig sh` is not interrupted.
	if showsEnvelope(verb, verbosity) {
		cfg.PrintPreRunEnvelope(set)
	}

	switch verb {
	case "info":
		cfg.Info(set)
		return nil
	case "env":
		// The retired spelling of the same report; the notice for it is printed
		// by the dispatch above.
		cfg.Info(set)
		return nil
	case "create":
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		fmt.Println(cfg.VMName)
		return nil
	case "sh", "shell":
		// One command or none, which is the whole of sh's grammar: Shell runs
		// the trailing words through a login shell and opens one when there are
		// no trailing words.
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

// showsEnvelope reports whether this invocation prints the execution envelope
// before it boots. Two rules, and they are independent: which verbs have a
// boundary worth naming, and whether the reader asked to see it.
//
// A function rather than a condition at the call site because both rules have
// caught something: a scripted `brig sh` on a cold sandbox printed a block
// nobody was reading, and the level moved after that.
func showsEnvelope(verb string, v wrap.Verbosity) bool {
	switch verb {
	case "run", "create":
		return v >= wrap.Verbose
	}
	return false
}

func runAgent(cfg *wrap.Config, set creds.Set, t profile.Profile, tail []string, detach bool) error {
	// A windowed agent owns its own console, so there is nothing to pass
	// through and nothing to exec into: starting it IS the command.
	if t.IsGUI() && len(tail) > 0 {
		return fmt.Errorf("%s is a graphical agent, so it takes no arguments "+
			"(use `brig sh %s` or `brig stop %s`)", t.Name, t.Name, t.Name)
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return err
	}
	switch {
	case t.IsGUI():
		warnf("sandbox %s is running; the %s window should be visible.", cfg.VMName, t.GUITitle)
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
	quiet     bool
	offline   bool
}

// position is where on a brig command line a flag is legal.
//
// A brig line has three places a token can stand -- left of the verb, between
// the verb and the session ref, and right of the ref -- and they are not the
// same kind of place. In the first two brig owns the vocabulary, so a token it
// does not recognise is a mistake to name. In the third the vocabulary is the
// agent's, so the same token is a word to forward untouched. Which set a flag
// belongs to is therefore what decides how an unknown neighbour of it is read,
// and that is why the table below carries it rather than leaving the boundary
// to a single "have I seen the profile yet".
//
// The zero value is no position at all, so an entry has to say which one it
// means. Defaulting would put a new flag in whichever set the zero value
// happened to be, and both wrong answers are silent: a flag that is
// accidentally global is refused everywhere it is documented to work, and one
// that is accidentally on the run line is handed to the agent, which does not
// have it.
type position int

const (
	// posGlobal is left of the verb: `brig --json run claude`. Closed and
	// empty today; see splitGlobal.
	posGlobal position = iota + 1
	// posRun is the run line, from the verb to the ref. Every flag brig has
	// today is here.
	posRun
	// posAny matches a flag wherever brig would have read it. Only the tail
	// warning uses it: right of the ref every token is the agent's whatever
	// position brig would otherwise read it in, so what matters there is that
	// the token is brig's at all, not where.
	posAny
)

// brigFlags is what brig owns, and where. Everything else on a run line
// belongs to the agent, so this table is also the boundary: split consults it
// to find where brig's arguments stop, which is why it records whether a flag
// takes a value. Adding a flag here and registering it in parse -- or in
// parseGlobal, for a global one -- are the two halves of adding a flag at all.
var brigFlags = []struct {
	long     string
	short    string
	value    bool // takes a value, so the next argument may belong to it
	position position
	// retiredAs and retiredTo are the notice for a flag whose POSITION is
	// going, rather than its spelling: what to stop writing, and what to write
	// instead. Empty on a flag that is not retiring here.
	//
	// On the entry rather than in deprecatedFlags because that map is keyed by
	// spelling and cannot say this: -q is not going anywhere, its place on the
	// line is, and the same spelling in the other position is the replacement.
	// The table already carries a position per flag, so this is a field on the
	// row rather than a case in split.
	retiredAs, retiredTo string
}{
	{long: "name", short: "n", value: true, position: posRun},
	{long: "image", short: "t", value: true, position: posRun},
	// --home is the guest home; -w and --workspace are the spellings it
	// replaces, and all three write one value. The home and the project are
	// two directories on one line now, and "workspace" could not say which of
	// them it meant.
	{long: "home", value: true, position: posRun},
	{long: "workspace", short: "w", value: true, position: posRun},
	// --mem is the spelling; --memory is what it was called first and still
	// answers, because a line that works today keeps working. Both write one
	// value in parse, so the last one on the line wins.
	{long: "mem", value: true, position: posRun},
	{long: "memory", short: "m", value: true, position: posRun},
	{long: "cpus", value: true, position: posRun},
	// --no-project detaches the project a session is carrying. It takes no
	// value: a directory is what the positional says, and this says the
	// absence of one.
	{long: "no-project", position: posRun},
	{long: "detach", short: "d", position: posRun},
	{long: "skills", position: posRun},
	{long: "network", value: true, position: posRun},
	{long: "offline", position: posRun},
	// --verbose is brig's own progress and the runtime's own output, and it is
	// global because it is a fact about the whole invocation rather than about
	// one run line.
	//
	// It has NO short form, on purpose and permanently. -v is Claude Code's
	// version flag, codex's verbose flag and Docker's volume flag: brig owns
	// none of those readings, and taking the letter would make `brig run
	// claude -v` mean one thing to brig and another to everything else the
	// reader has ever typed it at. Do not add it back as an obvious
	// convenience.
	{long: "verbose", position: posGlobal},
	// -q is global from this release. It was on the run line, and `brig run
	// claude -q` is a line people have written, so the run-line spelling below
	// keeps working for one release and says where the flag went -- the same
	// two-release window every other retiring spelling took. Global-only would
	// have sent that -q to the agent, silently changing what a working command
	// does.
	{long: "quiet", short: "q", position: posGlobal},
	{long: "quiet", short: "q", position: posRun,
		retiredAs: "brig <verb> <ref> -q", retiredTo: "brig -q <verb> <ref>"},
}

// deprecatedFlags are the spellings on their way out, and what replaces each.
// #47 ships both grammars in v0.2 and removes these in v0.3, so they keep
// working and say so -- the same contract the retiring verbs have.
//
// -n and --name retire onto the label rather than onto another flag: the ref is
// what names a session now, and `brig run claude@refactor` is what `brig run
// claude --name refactor` was. Both spellings are mapped and both are named,
// rather than the long one being aliased quietly, because --name is also Claude
// Code's own flag -- `brig run claude -- --name x` is the agent's --name today
// -- and silently reinterpreting a flag two programs share is worse than saying
// which of them read it.
//
// -w and --workspace retire onto --home, both spellings named. The long one is
// mapped and said out loud rather than aliased quietly because the word itself
// is what is being retired: with a project on the same line there are two host
// directories to talk about, and "workspace" never said which one it was.
var deprecatedFlags = map[string]string{
	"-t":          "--image",
	"-m":          "--mem",
	"-n":          "<agent>@<label>",
	"--name":      "<agent>@<label>",
	"-w":          "--home",
	"--workspace": "--home",
}

// ours reports whether a token is one of brig's flags in the position it was
// written, and whether that flag consumes the argument after it. A token
// carrying its own value (--name=foo) consumes nothing further.
//
// The two documented spellings and no others. The flag package would answer to
// -name and --n as well, but brig forwards everything it does not own, so
// being greedier here silently eats an agent's own flag: `brig run claude
// -image x` has to stay the agent's -image.
func ours(arg string, at position) (mine bool, takesValue bool) {
	mine, takesValue, _ = oursAt(arg, at)
	return mine, takesValue
}

// oursAt is ours with the position the match was found in, which only matters
// when the search was posAny: a flag brig read nowhere on this line still has a
// place it belongs, and that is what the tail notice has to name.
func oursAt(arg string, at position) (mine bool, takesValue bool, found position) {
	name, _, inline := strings.Cut(arg, "=")
	for _, f := range brigFlags {
		if at != posAny && f.position != at {
			continue
		}
		if name == "--"+f.long || (f.short != "" && name == "-"+f.short) {
			return true, f.value && !inline, f.position
		}
	}
	return false, false, 0
}

// retiredAt reports the notice for a flag written in a position it is leaving:
// the spelling to stop writing and the one that replaces it, or two empty
// strings when the flag is not retiring here. See brigFlags.
func retiredAt(arg string, at position) (was, now string) {
	name, _, _ := strings.Cut(arg, "=")
	for _, f := range brigFlags {
		if f.position != at || f.retiredTo == "" {
			continue
		}
		if name == "--"+f.long || (f.short != "" && name == "-"+f.short) {
			return f.retiredAs, f.retiredTo
		}
	}
	return "", ""
}

// splitGlobal finds the verb and refuses anything standing left of it.
//
// The global position is closed. --verbose and -q live there, both facts about
// the whole invocation rather than about one run line, and a token brig does
// not own is refused rather than read as something else. `brig --json run
// claude` is a line someone will type, and the two other readings are "the
// command is --json" and "the agent wants it": one reports a command that does
// not exist, the other hands a word to an agent that does not have it. Naming
// the token is the reading that is true.
//
// A flag-shaped verb -- -h, --help, --version -- is a verb, not a token in this
// position: those are the spellings brig has always answered to.
func splitGlobal(args []string) (mine []string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || verbSpelling(a) {
			return mine, args[i:], nil
		}
		isOurs, takesValue := ours(a, posGlobal)
		if !isOurs {
			return nil, nil, usagef("unknown flag %q before the command. "+
				"brig takes a command first: `brig run claude`, `brig ls`. "+
				"If %q is the agent's, it goes after the profile", a, a)
		}
		mine = append(mine, a)
		if takesValue && i+1 < len(args) {
			i++
			mine = append(mine, args[i])
		}
	}
	return mine, nil, nil
}

// verbSpelling reports whether a token is one of brig's verbs written as a
// flag. Only these three: they are read by the verb switch in run, and a
// fourth would be a flag.
func verbSpelling(arg string) bool {
	switch arg {
	case "-h", "--help", "--version":
		return true
	}
	return false
}

// globals are brig's flags in the global position: how much this invocation
// says about itself, which is a property of the whole command rather than of
// the run line inside it.
type globals struct {
	quiet   bool
	verbose bool
}

// verbosity is the level these two flags ask for. Neither given is the default,
// which is what a person reads.
func (g globals) verbosity() wrap.Verbosity {
	switch {
	case g.quiet:
		return wrap.Quiet
	case g.verbose:
		return wrap.Verbose
	}
	return wrap.Normal
}

// parseGlobal reads brig's global flags -- the ones legal left of the verb --
// and returns them with the verb and everything after it.
//
// Registering a flag here is the second half of adding a global one; the table
// entry in brigFlags is the first. Doing only the first half fails here with
// the flag package's "not defined" rather than accepting the flag and dropping
// it on the floor.
func parseGlobal(args []string) (g globals, rest []string, err error) {
	mine, rest, err := splitGlobal(args)
	if err != nil {
		return globals{}, nil, err
	}
	fs := flag.NewFlagSet("brig", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	// No short form for --verbose, deliberately and permanently. See brigFlags.
	fs.BoolVar(&g.verbose, "verbose", false, "")
	for _, spelling := range []string{"quiet", "q"} {
		fs.BoolVar(&g.quiet, spelling, false, "")
	}
	if err := fs.Parse(mine); err != nil {
		return globals{}, nil, rewriteFlagError(err)
	}
	// Both at once asks to be told more and less about the same run, and either
	// winner would leave the line reading like it asked for the opposite.
	if g.quiet && g.verbose {
		return globals{}, nil, usagef("--quiet and --verbose ask for different things; drop one")
	}
	return g, rest, nil
}

// split divides a run line into brig's own arguments, the session ref, the
// project positional, and the agent's tail.
//
// The verb is a parameter because only run takes a positional: on run the
// second bare word is a directory to mount, and on sh it is the start of the
// guest command. Every other verb gets that word back at the head of its tail,
// where rejectTail names it.
//
// This exists because the flag package stops at the first non-flag argument
// and treats an unknown flag as an error, and brig's line is the opposite on
// both counts: the ref sits in the middle of brig's flags, and an unrecognised
// flag after the ref is not a mistake but the agent's -- `brig run claude -p hi`
// runs the agent with -p hi. So the boundary is decided here, from the table,
// and the flag package parses what is left of it.
//
// The rule is that the first token brig does not own ends its parsing and
// everything from there is the agent's, warnings included. Before the ref brig
// owns every token, so an unknown flag there is a brig flag that does not
// exist rather than something to forward. After the ref brig still answers to
// its own flags -- `brig run claude -q` -- and on run it owns the positional
// too, so that does not end the parsing either: `brig run claude ~/proj --mem
// 4096 -d` is read by brig throughout. The next bare word after the positional
// does end it, which is where the agent's argv starts.
func split(verb string, args []string) (mine []string, ref session.Ref, word string, tail []string, err error) {
	takesProject := verb == "run"
	passed := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			// An explicit end to brig's parsing, so an agent argument spelled
			// like one of brig's flags still reaches the agent. Nothing is
			// said about what follows: the line already said it -- which is
			// also why no word after it is ever read as a project.
			//
			// A project already read stands. `brig run claude ~/proj -- pwd`
			// named one before the marker, and the marker speaks for what
			// comes after it rather than undoing what came before.
			return mine, ref, word, args[i+1:], nil
		case !strings.HasPrefix(a, "-"):
			if !passed {
				if ref, err = session.ParseRef(a); err != nil {
					// A ref brig cannot read is a mistyped command, not a
					// profile to go and look up: reporting it as a missing
					// profile would name the whole token and say nothing about
					// the part that is wrong.
					return nil, session.Ref{}, "", nil, usagef("%s", err)
				}
				passed = true
				continue
			}
			// A bare word after the ref. On run the first one is the
			// project, which brig owns and so reads past; anywhere else, and
			// for the next one on run, it is where the agent's argv starts.
			if takesProject && word == "" {
				word = a
				continue
			}
			return mine, ref, word, agentTail(args[i:]), nil
		default:
			isOurs, takesValue := ours(a, posRun)
			if !isOurs {
				// An unknown flag once the ref is named is the agent's:
				// `brig run claude --resume` runs the agent with --resume, and
				// brig forwards everything from here on. Before the ref there is
				// no agent yet to own it, so a flag brig does not recognise in
				// that position is not passed through -- it is a brig flag that
				// does not exist. Forwarding it would make the profile look like
				// the flag's value and leave the line blaming the one word on it
				// that was right, so refuse it and name it.
				if !passed {
					return nil, session.Ref{}, "", nil, usagef("unknown flag %q before the profile name. "+
						"brig's own flags come before the profile and the agent's after it; "+
						"put %q after the profile to pass it through, or -- to end brig's flags", a, a)
				}
				return mine, ref, word, agentTail(args[i:]), nil
			}
			// Read off the spelling rather than the whole token, so the
			// inline form is the same flag here as it is everywhere else, and
			// named before the value is taken, so the notice is never about a
			// value that reads like a flag: `--name -t` is a session called -t
			// and reaches this loop as a value, not as a token.
			if spelling, _, _ := strings.Cut(a, "="); deprecatedFlags[spelling] != "" {
				deprecated(spelling, deprecatedFlags[spelling])
			} else if was, now := retiredAt(a, posRun); now != "" {
				// A flag whose place on the line is what retires, not its
				// spelling. Said here, where the position is known, rather than
				// after the flag package has read it and forgotten where it
				// stood.
				deprecated(was, now)
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
	return mine, ref, word, nil, nil
}

// warnPositionalMeaning names both readings of a second bare word, for the one
// release in which it changes meaning.
//
// Until now that word ended brig's parsing and reached the AGENT: `brig run
// claude .` passed "." to claude. It is the project directory brig mounts from
// here on, so anyone who was passing a positional through has a line that means
// something else now -- and this is a breaking change however additive the
// feature looks. Naming the reading it lost, beside the one it gained, is what
// lets somebody pick the one they meant.
//
// Only a word brig read itself. A tail after -- was declared the agent's by the
// person typing it, so there is nothing to point out -- the same rule agentTail
// follows.
func warnPositionalMeaning(word string) {
	warnf("`%s` is now the project directory this run mounts, "+
		"and brig starts the agent in it. It used to be the agent's first argument "+
		"instead. If that is what you meant, put it after --: `brig run <ref> -- %s`. "+
		"This notice goes in the next release.", word, word)
}

// agentTail hands the tail back as the agent's, saying so for any of brig's own
// flags in it.
//
// Right of the boundary brig reads nothing, and that is the surprise worth
// naming: `brig run claude -p hi --quiet` has always run the agent with
// --quiet and left the envelope printed, and the line reads like it asked for
// the opposite. So brig says which token it did not read. It does not take it:
// a line that works today has to keep working, and capturing the token would
// change what runs rather than explain it.
//
// Only the tail brig ended itself. A tail after -- was declared the agent's by
// the person typing it, and there is nothing to point out about a decision
// already made.
func agentTail(tail []string) []string {
	for _, a := range tail {
		mine, _, at := oursAt(a, posAny)
		if !mine {
			continue
		}
		// Where the flag actually belongs, which is not the same place for
		// every flag any more: --verbose and -q stand left of the verb, and
		// telling their reader to put one before the profile would send them to
		// a position that refuses it.
		where := "before the profile"
		if at == posGlobal {
			where = "before the command"
		}
		name, _, _ := strings.Cut(a, "=")
		warnf("%s is one of brig's own flags, but here it is the "+
			"agent's: brig stopped reading the line at the argument before it. "+
			"Put %s %s for brig to read it.", name, name, where)
	}
	return tail
}

// rejectTail refuses a token a verb has no use for, rather than dropping it.
//
// run forwards the tail to the agent, and sh -- with shell and exec, the two
// spellings it replaces -- turns it into the guest command, so those keep
// whatever follows the ref. create, stop, rm and env take a ref and nothing
// after it: a word left there is a mistake to name, not an operand to swallow.
// create is grouped with the others rather than with run because it does not
// attach -- split hands it the same agent tail run gets, but there is no agent
// here to receive it, so a tail is still a mistake.
func rejectTail(verb string, tail []string) error {
	switch verb {
	case "run", "sh", "shell", "exec":
		return nil
	}
	if len(tail) > 0 {
		return usagef("unexpected argument %q; `brig %s` takes a ref and nothing more", tail[0], verb)
	}
	return nil
}

// parse reads brig's own flags off the run line in any order and leaves
// whatever remains for the agent. split decides where brig's arguments end;
// the flag package turns them into values.
func parse(verb string, args []string) (o options, profileName string, tail []string, err error) {
	mine, ref, word, tail, err := split(verb, args)
	if err != nil {
		return options{}, "", nil, err
	}
	o.load.Project = word

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
		// Three spellings of the guest home: --home is the one, -w and
		// --workspace are what it replaces. They share the value, so the last
		// one on the line wins.
		{"home", "", func(n string) { fs.StringVar(&o.load.Workspace, n, "", "") }},
		{"workspace", "w", func(n string) { fs.StringVar(&o.load.Workspace, n, "", "") }},
		// Two long spellings of one value: --mem is the one the usage text
		// gives, --memory is what it was called first. They share mem, so the
		// last one on the line wins and the error names the one that carried
		// the bad value.
		{"mem", "", func(n string) { fs.Var(numberFlag{spell(n), &mem}, n, "") }},
		{"memory", "m", func(n string) { fs.Var(numberFlag{spell(n), &mem}, n, "") }},
		{"cpus", "", func(n string) { fs.Var(numberFlag{spell(n), &cpus}, n, "") }},
		{"detach", "d", func(n string) { fs.BoolVar(&o.detach, n, false, "") }},
		// Opt-in, and only ever opt-in: this hands the guest the user's real
		// skills and plugins, read-only.
		{"skills", "", func(n string) { fs.BoolVar(&o.load.Skills, n, false, "") }},
		// Run this session with no project, whatever it ran with last.
		{"no-project", "", func(n string) { fs.BoolVar(&o.load.NoProject, n, false, "") }},
		// The posture this run takes: shared or offline. Refused by name later
		// if it is neither, in the one place every source of it is resolved.
		{"network", "", func(n string) { fs.StringVar(&o.load.Network, n, "", "") }},
		// Sugar for --network offline, which is the word the request is usually
		// phrased in.
		{"offline", "", func(n string) { fs.BoolVar(&o.offline, n, false, "") }},
		// Suppresses the execution envelope on run and create, for a script or
		// a returning session that has already seen it.
		{"quiet", "q", func(n string) { fs.BoolVar(&o.quiet, n, false, "") }},
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
	// --offline is shorthand for one --network value, so the two agreeing is
	// fine and the two disagreeing is a mistake worth naming: a silent winner
	// would leave the run with a posture the line does not read like.
	if o.offline {
		if o.load.Network != "" && o.load.Network != "offline" {
			return options{}, "", nil, fmt.Errorf(
				"--offline and --network %s ask for different things; --offline is --network offline",
				o.load.Network)
		}
		o.load.Network = "offline"
	}
	// The label is the session, so it lands where --name lands rather than
	// becoming a second way to hold one: `brig run claude@refactor` is `brig
	// run claude --name refactor`, and everything downstream -- the slug, the
	// workspace, the sandbox name, the display name the agent is given -- is
	// reached by the one path it always was.
	//
	// Both on one line is two different sessions asked for at once. A silent
	// winner would run one of them with the other still written on the command,
	// so name them and stop.
	if ref.Label != "" {
		if o.nameGiven {
			return options{}, "", nil, usagef("%s names the session %q and --name names %q. "+
				"Use one or the other", ref, ref.Label, o.load.Name)
		}
		o.load.Name, o.nameGiven = ref.Label, true
	}
	return o, ref.Agent, tail, nil
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
//
// It leads with the ref, which is what every other verb takes: a listing whose
// identifier no verb accepts is a listing a reader cannot act on, and copying
// what it printed used to be an error.
func listSandboxes(args []string, quiet bool) error {
	// ls names no session and -q is the only flag it has, so anything else here
	// is a token it would otherwise read and discard -- `brig ls claude` looks
	// like it filters to one agent and does not. Refuse it rather than answer a
	// question that was not asked.
	//
	// Read here rather than through the flag package because ls is dispatched
	// before the run line is parsed: it has no ref to parse, and the run line's
	// parser exists to find where brig's arguments stop, which on this verb is
	// immediately. Both spellings, because -q and --quiet are one flag
	// everywhere else brig reads them.
	for _, a := range args {
		switch a {
		case "-q", "--quiet":
			quiet = true
		default:
			return usagef("unexpected argument %q; `brig ls` takes no arguments "+
				"other than -q", a)
		}
	}
	rt, err := runtime.Detect()
	if err != nil {
		if !errors.Is(err, runtime.ErrNoRuntime) {
			// An unknown BRIG_RUNTIME is a mistake, not an empty list: reading a
			// typo as "you have no sandboxes" hides it. Fail with the value
			// named, exactly as run does, rather than answering a question that
			// was not asked.
			return err
		}
		// No runtime on PATH means nothing is running and nothing is holding a
		// name: there is nothing to list. Answer the question that was asked --
		// there are no sandboxes -- and exit 0, rather than failing a read-only
		// query with the error of the thing it would have queried.
		// Print the same header the runtime-present empty case does, so the
		// shape of `brig ls` does not change with the runtime, and surface the
		// platform-specific way to get one -- the hint run gives -- so a fresh
		// box is told how rather than only that there is nothing.
		//
		// Except under -q, which is read by a script: no sandboxes is an empty
		// list, and the note about why would be a line the loop reading this had
		// to recognise and skip.
		if quiet {
			return nil
		}
		printSandboxes(os.Stdout, nil)
		fmt.Println("(none -- no runtime found on PATH, so there are no sandboxes)")
		fmt.Printf("  %s\n", strings.TrimPrefix(err.Error(), "no runtime found on PATH: "))
		return nil
	}
	list, err := rt.List()
	if err != nil {
		return err
	}
	// The one verb that knows what the runtime actually has, so the one place
	// an entry for a sandbox that went away without going through `brig rm`
	// can be dropped. See wrap.PruneSessions.
	pruneSessionIndex(list)
	rows := sandboxRows(list, rt)
	if quiet {
		printRefs(os.Stdout, rows)
		return nil
	}
	printSandboxes(os.Stdout, rows)
	if len(rows) == 0 {
		fmt.Println("(none -- `brig run claude` starts one)")
	}
	return nil
}

// sandboxRows turns what the runtime has into the listing: brig's own
// sandboxes, in name order, each with the ref and the workspace it answers to.
//
// Separate from listSandboxes because it is the whole of what the listing
// decides, and the only part of it a test can drive without a runtime: what the
// two printers below are handed is exactly this.
func sandboxRows(list []runtime.Instance, rt runtime.Runtime) []sandboxRow {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	rows := make([]sandboxRow, 0, len(list))
	for _, inst := range list {
		if !strings.HasPrefix(inst.Name, sandboxPrefix) {
			continue
		}
		rows = append(rows, sandboxRow{
			ref:       refOf(inst.Name),
			name:      inst.Name,
			state:     inst.State,
			workspace: workspaceOf(inst.Name, rt),
		})
	}
	return rows
}

// pruneSessionIndex hands the session index every instance the runtime has, so
// that an entry naming a sandbox that is not among them goes.
//
// Every instance and not only brig's own: a sandbox brig would not recognise
// still exists, and the question the index is asking is whether the sandbox is
// there at all.
func pruneSessionIndex(list []runtime.Instance) {
	live := make([]string, 0, len(list))
	for _, inst := range list {
		live = append(live, inst.Name)
	}
	wrap.PruneSessions(live)
}

// sandboxRow is one line of the listing, gathered before anything is printed
// so the two variable-width columns can be sized to what they will hold.
//
// ref is the session this sandbox is carrying, and empty when brig has none to
// print for it -- see refOf. It is a string rather than a session.Ref because
// the listing only ever writes it out, and an empty string is the one state a
// Ref cannot hold: a Ref with an empty agent is not a ref, and it would have to
// be checked for at every use rather than once, here.
type sandboxRow struct {
	ref       string
	name      string
	state     string
	workspace string
}

// noRef is what the table shows for a sandbox with no ref to print.
//
// A dash rather than a blank, because the two say different things to whoever
// is reading: an empty cell in a column that is full on every other line reads
// as brig having lost the value, and this is brig saying it has none. Nothing
// is guessed into the gap -- see refOf for what is derived and where the
// deriving stops -- and the SANDBOX column beside it still names the thing, so
// `brig rm --all` remains the way to be rid of it.
const noRef = "-"

// refCell is the ref as the table prints it, placeholder included.
func refCell(ref string) string {
	if ref == "" {
		return noRef
	}
	return ref
}

// printSandboxes writes the listing, header included, with the ref and name
// columns each as wide as what is in it.
//
// The ref leads, because it is the column that is also the identifier: it is
// what every other verb takes, so it is the word a reader is here to copy. The
// sandbox name is beside it for recognising brig's own sandboxes in the
// runtime's output, which is the only thing that name is good for.
//
// The width was a constant, and no constant is right: a sandbox is named after
// its profile plus a session slug, so brig-claude-desktop with a ten-character
// slug is already 30 characters against the 28 that were reserved -- and a
// profile read from a file can be called anything, so a wider constant only
// moves where it breaks. Measuring the values costs a pass over a list brig has
// in hand and cannot be outgrown.
func printSandboxes(w io.Writer, rows []sandboxRow) {
	ref, name := len("REF"), len("SANDBOX")
	for _, r := range rows {
		if n := len(refCell(r.ref)); n > ref {
			ref = n
		}
		if len(r.name) > name {
			name = len(r.name)
		}
	}
	fmt.Fprintf(w, "%-*s %-*s %-10s %s\n", ref, "REF", name, "SANDBOX", "STATE", "WORKSPACE")
	for _, r := range rows {
		fmt.Fprintf(w, "%-*s %-*s %-10s %s\n",
			ref, refCell(r.ref), name, r.name, r.state, r.workspace)
	}
}

// printRefs writes the refs and nothing else, one to a line: `brig ls -q`, the
// form a script reads.
//
// A row with no ref is left out rather than printed as the table's placeholder.
// Every line of this output is meant to be a word another verb takes, and a
// loop reading it must not be handed one that is not -- which is the promise the
// round-trip test pins. The table is where a person sees that the sandbox is
// there at all.
func printRefs(w io.Writer, rows []sandboxRow) {
	for _, r := range rows {
		if r.ref == "" {
			continue
		}
		fmt.Fprintln(w, r.ref)
	}
}

// refOf is the ref of whichever session is carrying a sandbox: the one brig
// recorded, and otherwise the one the sandbox's own name decomposes into. Empty
// when neither answers, which is the case the table prints noRef for.
//
// The recorded ref is the answer whenever there is one: the index is filed by
// ref, so an entry naming this sandbox holds that sandbox's ref by
// construction. The derivation is for a sandbox with no entry -- one created
// before the index existed, or one whose entry was pruned -- and it is the same
// decomposition the workspace column has always fallen back to.
//
// Deriving beats leaving the column empty because the derived ref cannot
// address a sandbox other than the one on its row: a sandbox is named
// <prefix><agent>-<slug>, so resolving the ref it decomposes into builds that
// same name straight back. The ambiguity in a sandbox name -- claude-code plus
// the slug "refactor" reads equally as an agent called claude-code-refactor
// with no slug -- picks between two refs that reach the one sandbox either way.
// What it can get wrong is which agent, and so which workspace the session
// resolves to by default, and the cost of that is one restart.
//
// Whatever comes out is then checked rather than trusted, on both counts the
// listing promises: it has to parse as a ref, and its agent has to be one brig
// still has. A sandbox named through BRIG_NAME can decompose into a label no
// verb would accept, and an agent whose file has been deleted leaves a ref that
// every verb answers "unknown profile" to. Neither is a word to hand a script,
// so neither is printed, and the check is what makes that structural instead of
// something to remember.
func refOf(vmName string) string {
	ref := wrap.RefOfSandbox(vmName)
	if ref == "" {
		agent, slug, ok := splitSandboxName(vmName)
		if !ok {
			return ""
		}
		ref = session.Ref{Agent: agent, Label: slug}.String()
	}
	parsed, err := session.ParseRef(ref)
	if err != nil {
		return ""
	}
	if _, ok := profile.Lookup(parsed.Agent); !ok {
		return ""
	}
	return ref
}

// splitSandboxName reads a sandbox name back into the profile and the session
// slug it was built from, and reports whether any profile brig has fits.
//
// Longest profile name first, so claude-code wins over a hypothetical claude
// when both could prefix-match. An unnamed sandbox is named exactly after its
// profile, so there is no slug to recover: without the equality case here,
// TrimPrefix would leave the profile name intact and the caller would answer
// about a session called "claude-code", which is not the one running.
func splitSandboxName(vmName string) (profileName, slug string, ok bool) {
	rest := strings.TrimPrefix(vmName, sandboxPrefix)
	names := profile.Names()
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		switch {
		case rest == name:
			return name, "", true
		case strings.HasPrefix(rest, name+"-"):
			return name, strings.TrimPrefix(rest, name+"-"), true
		}
	}
	return "", "", false
}

// workspaceOf recovers the workspace for a sandbox: what it was started with
// if brig recorded that, and otherwise the path the profile it was named after
// would derive. A sandbox brig did not name has none to report.
//
// The recorded path wins over the derivation, and over an ambient
// BRIG_WORKSPACE that the derivation would otherwise pick up. This column says
// what a sandbox is mounting, not what a run started from this shell would
// mount: a session created with -w has a directory neither of those two can
// name, and reporting the derived one was reporting the wrong directory with
// no sign that it was wrong.
func workspaceOf(vmName string, rt runtime.Runtime) string {
	if ws := wrap.WorkspaceOfSandbox(vmName); ws != "" {
		return ws
	}
	name, slug, ok := splitSandboxName(vmName)
	if !ok {
		return ""
	}
	t, _ := profile.Lookup(name)
	cfg, err := wrap.Load(t, wrap.Options{Name: slug}, rt)
	if err != nil {
		return ""
	}
	return cfg.Workspace
}

// takeAll reads --all off a command line and reports whether it was there.
//
// A flag on rm rather than a verb of its own, because it is the same removal:
// `brig rm <ref>` takes one sandbox and `brig rm --all` takes every one. The
// long spelling only -- a short -a would be a second way to ask for the most
// destructive thing brig does, and #47 is retiring short flags rather than
// adding them.
//
// What is left of the line is handed back rather than dropped, so removeAll can
// refuse it by name: `brig rm claude --all` is two different requests written on
// one line, and picking either would act on something the line does not read
// like.
func takeAll(args []string) (rest []string, all bool) {
	for _, a := range args {
		if a == "--all" {
			all = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, all
}

// removeSandbox is `brig rm <ref>`: be rid of the one sandbox the ref names.
//
// It asks the runtime whether that sandbox is there before handing over, so the
// case the runtime used to answer with its own "instance not found" -- there is
// nothing to remove -- is reported as a not-found (exit 3), the code the README
// gives a name that resolves to nothing, rather than as a removal that ran and
// failed (exit 1). A List that itself fails is a runtime that could not be asked
// (exit 4), a different fact the exit table keeps apart, so it comes back
// untouched rather than being read as absence.
func removeSandbox(cfg *wrap.Config, ref string) error {
	present, err := sandboxPresent(cfg.Runtime, cfg.VMName)
	if err != nil {
		return err
	}
	if !present {
		// Not pruned here. "Not in the list" is not always "gone": hull.List
		// falls back to a plain `ps` on a hull without `ps -a`, and that listing
		// carries only the running instances, so a stopped sandbox reads as
		// absent while it is still in hull's store. Forgetting its index entry on
		// that reading would drop the workspace record of a sandbox that is
		// really there. The entry goes when a removal actually happens, in
		// Remove, or when ls prunes against a full listing; a stale entry for a
		// sandbox that is truly gone costs nothing until then.
		return noSandboxf(ref)
	}
	return cfg.Remove()
}

// sandboxPresent reports whether the runtime has a sandbox of this name at all,
// running or stopped.
//
// It is how rm and logs tell "there is nothing here" -- a not-found, exit 3 --
// from a runtime that could not be asked -- its own error, exit 4. The two are
// different facts and the README's exit table keeps them apart, so a List that
// fails is returned as is rather than folded into absence.
func sandboxPresent(rt runtime.Runtime, name string) (bool, error) {
	list, err := rt.List()
	if err != nil {
		return false, err
	}
	for _, inst := range list {
		if inst.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// noSandboxf is the not-found a ref'd verb gives when the runtime has no sandbox
// for the ref. It names the ref the reader typed -- the word `brig ls` prints
// and the one they will reach for next -- rather than the sandbox name they
// never chose.
func noSandboxf(ref string) error {
	return notFoundf("no sandbox for %s. `brig ls` lists them", ref)
}

// removeAll stops and removes every sandbox brig started. Workspaces are left
// alone: they are on the host, they hold your work, and this is a command
// about sandboxes.
//
// spelling is the command as it was typed, for the message: this is reached as
// `brig rm --all` and as the retired `brig reset`, and an error about the wrong
// one of those sends the reader to fix a line they did not write.
func removeAll(spelling string, args []string) error {
	// This stops and removes every brig sandbox, so a flag typed to make it
	// safer -- `brig rm --all --dry-run` -- must not be read past and ignored,
	// which is exactly how a command meant to preview ends up removing
	// everything. It has no flags and no operands beyond --all itself; refuse
	// anything here so a flag that gains a meaning later is one this release
	// declined rather than silently swallowed.
	if len(args) > 0 {
		return usagef("unexpected argument %q; `%s` takes no arguments and removes "+
			"every brig sandbox", args[0], spelling)
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
		err := rt.Remove(inst.Name)
		// The same pruning `brig rm` of one sandbox does, for the same reason
		// and on the same terms: this goes through the runtime directly rather
		// than through a Config, because it works from the instance list and a
		// stopped sandbox need not correspond to a profile brig can still look
		// up.
		wrap.ForgetSandbox(inst.Name)
		wrap.ForgetSlugClaim(inst.Name)
		if err != nil {
			warnf("could not remove %s: %v", inst.Name, err)
			continue
		}
		fmt.Println(inst.Name)
		removed++
	}
	warnf("removed %d sandbox(es). Workspaces are untouched.", removed)
	// A network whose sandbox was removed outside brig is not reachable
	// through Remove, because that sandbox is not in the list any more. This is
	// the one command that leaves nothing behind, so it prunes those too. Only
	// the runtimes that make a network per sandbox implement this; the rest are
	// skipped rather than asked.
	if p, ok := rt.(runtime.NetworkPruner); ok {
		// Asked again rather than reusing the list above: that one was taken
		// before the removals, so every sandbox just removed would still look
		// live and nothing would be pruned. What matters here is what survived,
		// which is a sandbox whose removal failed and whose network is
		// therefore still in use.
		var live []string
		if after, err := rt.List(); err == nil {
			for _, inst := range after {
				live = append(live, inst.Name)
			}
		}
		p.PruneNetworks(live)
	}
	return nil
}

// listProfiles prints the merged set: embedded and file-backed together, one
// namespace. It is a view of what brig will actually run, not a directory
// listing -- so it says where each profile came from, because "the image is
// not what I expected" and "there is a file I forgot about" are the same
// question.
func listProfiles() error {
	all := profile.All()
	// The short spellings, in a column of their own: `brig run claude` runs
	// claude-code, and this listing is where someone finds that out. The
	// column is as wide as the spellings in it and absent when there are none,
	// because a column that is empty on every line says nothing and costs
	// every description the width.
	//
	// The name column is measured the same way, floored at the width it has
	// always had. A profile from a file can be called anything, and a name
	// past the floor used to push its own description out; now it would push
	// the alias out with it.
	alias := make(map[string]string, len(all))
	nameWidth, aliasWidth := 15, 0
	for _, p := range all {
		alias[p.Name] = strings.Join(profile.Aliases(p.Name), " ")
		if len(p.Name) > nameWidth {
			nameWidth = len(p.Name)
		}
		if len(alias[p.Name]) > aliasWidth {
			aliasWidth = len(alias[p.Name])
		}
	}
	// A gutter inside the column: the widest spelling is desktop, and without
	// this it ends up against the description it is meant to be read apart
	// from.
	if aliasWidth > 0 {
		aliasWidth++
	}
	// What a profile's own lines are indented by, so they sit under the
	// description rather than in the alias column, where they would read as
	// more spellings of the name.
	indent := nameWidth + 1
	if aliasWidth > 0 {
		indent += aliasWidth + 1
	}
	pad := strings.Repeat(" ", indent)
	for _, p := range all {
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
		head := fmt.Sprintf("%-*s", nameWidth, p.Name)
		if aliasWidth > 0 {
			head += fmt.Sprintf(" %-*s", aliasWidth, alias[p.Name])
		}
		fmt.Printf("%s %s%s\n", head, p.Desc, suffix)
		fmt.Printf("%simage %s, home %s\n", pad, p.Image, p.GuestHome)
		if len(p.Env) > 0 {
			names := make([]string, 0, len(p.Env))
			for _, b := range p.Env {
				names = append(names, b.Name)
			}
			fmt.Printf("%senvironment: %s\n", pad, strings.Join(names, " "))
		}
		// Listed separately from the bindings above because these are the ones
		// you have to create before the sandbox will start at all.
		if len(p.Secrets) > 0 {
			fmt.Printf("%ssecrets: %s\n", pad, strings.Join(profile.SecretNames(p.Secrets), " "))
			// Which ones brig can fill from your host and which ones only you
			// can: the old single line said "create them" for both, which for
			// an importable secret is the long way round.
			//
			// Both lines are the command plus the names it covers, which is
			// what makes them comparable at a glance. Import takes the PROFILE
			// -- one command fills every importable name on this line -- while
			// create takes a name at a time.
			if importable := importableNames(p); len(importable) > 0 {
				fmt.Printf("%s  from your host: brig secret import %s (%s)\n", pad,
					p.Name, strings.Join(importable, " "))
			}
			if hand := handCreatedNames(p); len(hand) > 0 {
				fmt.Printf("%s  by hand: brig secret create <name> (%s)\n", pad,
					strings.Join(hand, " "))
			}
		}
		if len(p.Deny) > 0 {
			fmt.Printf("%snever forwarded: %s\n", pad, strings.Join(p.Deny, " "))
		}
	}
	fmt.Printf("\nan unmarked profile is built in; your own live in %s\n", profile.Dir())
	fmt.Printf("to override one:  brig agent new claude-code --from claude-code, then brig agent edit claude-code\n")
	fmt.Printf("to add your own:  brig agent new mytool --from claude-code, then brig agent edit mytool\n")
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

// agentUsage is what `brig agent --help` prints. Held here rather than in the
// top-level usage text for the same reason secretUsage is: it names the flags
// of seven verbs, which is more than the one line the command list can give
// them.
const agentUsage = `brig agent -- the agents you can run

usage:
  brig agent ls                          list the agents you can run
  brig agent show <agent> [--json]       print one, to read or to pipe
  brig agent new <name> --from <agent>   copy one under a name of your own
  brig agent edit <name>                 open yours in $VISUAL or $EDITOR
  brig agent rm <name> [-y]              delete yours, after asking
  brig agent import <file>               add one of your own (- reads stdin)
  brig agent export <agent> [name]       print one, or save it as <name>

flags:
      --from AGENT  with new: the agent to copy
      --json        with show, export and new: JSON rather than YAML
  -f, --force       with new and export: replace a file that is already there
  -y, --yes         with rm: the answer, given in advance

An agent brig ships is built in and has no file. new gives you a copy that
does, under a name of your own and with that name written into it, so
` + "`brig agent edit`" + ` and ` + "`brig run`" + ` both reach it by the name you picked.

A destination is a name and never a path: brig writes one directory of its own
and nowhere else, because that is the only place one of these files does
anything. Redirect ` + "`brig agent show`" + ` to put a copy anywhere of your own.

An agent entry saves you spelling out the image, the guest home and the
credential variables on every run. Copy the closest one and edit it. Building
an image for one is documented at
  ` + profile.BringYourOwnImageDoc + `
`

// agentCmd groups the verbs that manage the agents brig can run.
//
// One noun, one verb set: ls, show, new, edit, rm, import, export. The group
// was called profile and its listing was also a top-level `brig profiles`,
// while show and new were two shapes of one `brig export` -- three ways of
// spelling the same group, none of which told a reader which of the three the
// next command would want. deprecatedProfileCmd keeps every old spelling
// working; this is the only one the help text teaches.
//
// The noun is the CLI's alone. internal/profile, BRIG_PROFILE_DIR and
// ~/.brig/profiles/ keep their names: renaming them is a mechanical diff
// across the tree that would land on top of this one and compete with it for
// review, and it changes nothing anyone types.
func agentCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("agent needs a subcommand: ls, show, new, edit, rm, import or export")
	}
	var err error
	switch args[0] {
	case "--help", "-h", "help":
		fmt.Print(agentUsage)
		return nil
	case "ls":
		return listProfiles()
	case "show":
		err = showAgent(args[1:])
	case "new":
		err = newAgent(args[1:])
	case "export":
		err = exportProfile(args[1:])
	case "import":
		err = importProfile(args[1:])
	case "edit":
		err = editProfile(args[1:])
	case "rm":
		err = removeProfile(args[1:])
	// The undocumented second spellings, kept for one release. They were never
	// in the help text, so they were found by accident and then written into
	// scripts, which is exactly why they cannot simply disappear.
	case "list":
		deprecated("brig agent list", "brig agent ls")
		return listProfiles()
	case "save":
		deprecated("brig agent save", "brig agent export")
		err = exportProfile(args[1:])
	case "load":
		deprecated("brig agent load", "brig agent import")
		err = importProfile(args[1:])
	default:
		return fmt.Errorf("unknown agent subcommand %q "+
			"(ls, show, new, edit, rm, import, export)", args[0])
	}
	// A verb's own parser reports --help as an error, because that is how the
	// flag package says it. Asking for help is not a mistake, so it is answered
	// with the help and an exit code of zero -- the same translation the secret
	// group makes, and for the same reason.
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(agentUsage)
		return nil
	}
	return err
}

// deprecatedProfileCmd is `brig profile <verb>`, which is now
// `brig agent <verb>`.
//
// The notice names the subcommand rather than the group, because the verbs do
// not all retire onto the same word: export splits into show and new for the
// two lines people actually type, and a notice reading "`brig profile` is now
// `brig agent`" would leave the reader to work out which of the two theirs
// became. Whatever they typed still runs -- agentCmd does the work -- so the
// notice is the only difference between the old spelling and the new one.
func deprecatedProfileCmd(args []string) error {
	if len(args) == 0 {
		deprecated("brig profile", "brig agent")
		return agentCmd(args)
	}
	// The verb the old spelling maps onto, for the notice. Anything not listed
	// is a mistake rather than a retired spelling, and agentCmd names the real
	// verbs for it -- so it hears no notice, which is right: there is nothing
	// to stop typing.
	now := map[string]string{
		"ls": "ls", "list": "ls",
		"export": "export", "save": "export",
		"import": "import", "load": "import",
		"edit": "edit",
		"rm":   "rm",
	}
	verb, known := now[args[0]]
	if !known {
		if isHelp(args[0]) {
			deprecated("brig profile", "brig agent")
		}
		return agentCmd(args)
	}
	deprecated("brig profile "+args[0], "brig agent "+verb)
	// Translated to the current verb, so `brig profile save` does not go on to
	// hear agentCmd's notice for save as well: one command, one notice.
	return agentCmd(append([]string{verb}, args[1:]...))
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
		return errors.New("edit needs a name, for example `brig agent edit mine`")
	}
	name := args[0]
	p, ok := profile.Lookup(name)
	if !ok {
		return notFoundf("unknown profile %q. `brig agent ls` lists them", name)
	}
	path, ok := profile.Path(p.Name)
	if !ok {
		return fmt.Errorf("%s is built in, so there is no file to edit.\n"+
			"To override it:\n"+
			"  brig agent new %s --from %s\n"+
			"  brig agent edit %s",
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
// a pipe, which is what makes `brig agent show x | edit | brig agent import -`
// work.
func importProfile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("import needs a file, for example `brig agent import mine.yaml` "+
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
			"a binary, mem and cpus. `brig agent show claude-code` prints a working one "+
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

// exportLine is one line of show, new or export after parsing: the bare words
// in the order they were typed, and the flags that were set.
//
// The three verbs are one operation with different lines around it -- render a
// profile, to stdout or into the profile directory -- so they share a parser
// rather than keeping three copies of the loop below. Which flags a verb offers
// is passed in, because the message an unknown flag gets has to name that
// verb's own set: telling `brig agent show` it takes --force would advertise a
// flag it does not have.
type exportLine struct {
	words  []string
	asJSON bool
	force  bool
	from   string
}

// exportFlags is which of the optional flags a verb offers, and the prose that
// names them when one is mistyped.
type exportFlags struct {
	force bool
	from  bool
	takes string
}

// parseExportLine reads a render verb's arguments.
//
// Parse stops at the first bare word, so a flag written after one -- `export
// codex mine --json`, the order the docs use -- would otherwise be left
// sitting in Args. Lift the word and parse on.
//
// Unlike the run line, nothing here passes through to another program, so an
// unrecognised flag is a mistake rather than someone else's argument and the
// flag package can reject it outright. Without that, a mistyped flag falls
// through to the destination and export writes a file called --jsonn.
func parseExportLine(verb string, args []string, offers exportFlags) (exportLine, error) {
	var line exportLine
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.BoolVar(&line.asJSON, "json", false, "")
	if offers.force {
		fs.BoolVar(&line.force, "force", false, "")
		fs.BoolVar(&line.force, "f", false, "")
	}
	if offers.from {
		fs.StringVar(&line.from, "from", "", "")
	}
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return line, err
			}
			// The unknown flag is this group's own case: on the run line one
			// never reaches the flag package, because it belongs to the agent.
			// Every other error the parser can raise here is one the run line
			// already says in brig's voice, so say it the same way.
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(name)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return line, fmt.Errorf("%s (%s takes %s)", msg, verb, offers.takes)
		}
		if fs.NArg() == 0 {
			return line, nil
		}
		line.words = append(line.words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// showAgent prints one agent entry and writes nothing. It is the spelling
// `brig export <profile>` had, under the noun it belongs to.
//
// A second word is refused rather than read as a destination, which is the one
// place show and the old top-level export differ: `brig export claude mine`
// wrote a file, so anyone with that in their fingers gets the command that took
// the job over rather than a file they did not ask for.
func showAgent(args []string) error {
	line, err := parseExportLine("show", args, exportFlags{takes: "--json"})
	if err != nil {
		return err
	}
	switch len(line.words) {
	case 0:
		return errors.New("show needs an agent, for example `brig agent show claude-code`")
	case 1:
	default:
		return fmt.Errorf("show prints one agent and writes nothing, so it takes no "+
			"destination. To copy one under a name of your own: brig agent new %s --from %s",
			line.words[1], line.words[0])
	}
	return renderProfile(line.words[0], "", line.asJSON, false)
}

// newAgent copies an agent entry under a name of your own, which is the
// spelling `brig export <profile> <name>` had.
//
// --from rather than a second bare word, because the two words are not
// interchangeable and the old line gave a reader no way to tell which order it
// wanted. The name comes first here for the same reason it does in
// `brig secret create <name>`: the thing being made is the operand.
//
// The name is written into the file, not just onto it -- see profile.ExportAs.
// That is the defect this rename fixes: the recipe brig prints failed on its
// own second step, because the copy still declared the profile it came from
// and `brig agent edit <name>` had nothing of that name to open.
func newAgent(args []string) error {
	line, err := parseExportLine("new", args,
		exportFlags{force: true, from: true, takes: "--from, --json and --force"})
	if err != nil {
		return err
	}
	switch len(line.words) {
	case 0:
		return errors.New("new needs a name, for example " +
			"`brig agent new mine --from claude-code`")
	case 1:
	default:
		// Both words are named and neither is guessed at. `new mine codex` and
		// `new codex mine` are both lines someone types -- the second is the
		// old `brig export codex mine` order -- and picking one would send half
		// of them a corrected command with the two words the wrong way round.
		return fmt.Errorf("new takes one name and takes the agent it copies from --from, "+
			"so it cannot have both %q and %q: brig agent new <name> --from <agent>",
			line.words[0], line.words[1])
	}
	if line.from == "" {
		return fmt.Errorf("new copies an agent, so it needs one: "+
			"brig agent new %s --from claude-code. `brig agent ls` lists them", line.words[0])
	}
	return renderProfile(line.from, line.words[0], line.asJSON, line.force)
}

// exportProfile prints a profile, or writes it to a file: the verb `brig agent
// export` is, and the one `brig profile export` and the top-level `brig export`
// were. show and new are the taught spellings of its two halves; this is kept
// because it is the line already in everyone's scripts, word for word.
func exportProfile(args []string) error {
	line, err := parseExportLine("export", args,
		exportFlags{force: true, takes: "--json and --force"})
	if err != nil {
		return err
	}
	var name, dest string
	switch len(line.words) {
	case 0:
	case 1:
		name = line.words[0]
	case 2:
		name, dest = line.words[0], line.words[1]
	default:
		return fmt.Errorf("export takes an agent and at most one destination, not %q",
			line.words[2])
	}
	if name == "" {
		return errors.New("export needs an agent, for example `brig agent export claude-code`")
	}
	return renderProfile(name, dest, line.asJSON, line.force)
}

// renderProfile is what show, new and export all do: look one profile up and
// render it, to stdout or into the profile directory. YAML by default, because
// the result is meant to be edited; --json for anything consuming it
// programmatically.
//
// A destination is a name, never a path: `brig agent new mine --from codex`
// writes mine.yaml into the profile directory, because that is the only place
// a profile file does anything. See profile.ExportPath, which owns the rule.
//
// The destination is also the name written into the file. brig keys on the
// name: field rather than on the file name, so the two disagreeing is the
// difference between an agent of your own and a copy of someone else's under
// a misleading file name -- see profile.ExportAs.
//
// With no destination it prints, which is what keeps
// `brig agent show x | brig agent import -` working, what stops the command
// writing anything you did not ask for, and how you get a copy anywhere else:
// redirect it.
func renderProfile(name, dest string, asJSON, force bool) error {
	p, ok := profile.Lookup(name)
	if !ok {
		return notFoundf("unknown profile %q. `brig agent ls` lists them", name)
	}
	path, err := profile.ExportPath(dest, asJSON)
	if err != nil {
		return err
	}
	// The name the rendered profile carries: the file's own stem when there is
	// a file, so that `brig agent new mytool --from claude-code` writes a profile
	// called mytool and `brig agent edit mytool` can then open it. With no
	// destination there is no file to agree with, and what is printed is the
	// profile as it stands.
	as := p.Name
	if path != "" {
		as = stemOf(path)
	}
	// The same collision profile.Import refuses, refused here for the same
	// reason: a name that a reserved profile already answers to would take a
	// workspace that is not its own. Export can reach it now that it writes the
	// name rather than copying one that was already checked.
	if owner, reserved := profile.Reserved(as); reserved && as != owner {
		return fmt.Errorf("a profile named %q would collide with the %s profile, which "+
			"reserves that word. Copy it under another name", as, owner)
	}
	// An alias is the same collision one step further out: the word is already
	// how people spell a profile, and a profile of that name wins the lookup,
	// so the copy takes every run of the name it was copied from. Refused
	// rather than warned about, because the export that causes it is the first
	// step of the recipe brig prints and the shadowing shows up much later, as
	// a run booting an image nobody chose.
	if owner, ok := profile.Alias(as); ok {
		return fmt.Errorf("a profile named %q would collide with %s, which is what brig "+
			"already runs for that word. Copy it under another name", as, owner)
	}
	render := profile.ExportAs
	if asJSON {
		render = profile.ExportJSONAs
	}
	blob, err := render(p, as)
	if err != nil {
		return err
	}
	if path == "" {
		_, err = os.Stdout.Write(blob)
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
	// The directory may not exist yet: `brig agent edit` points a first-time
	// user straight here, and on a fresh install nothing has created it. 0700
	// per the XDG spec, and the right mode for files naming credentials.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		return err
	}
	if as == p.Name {
		fmt.Printf("wrote %s -> %s\n", p.Name, path)
	} else {
		fmt.Printf("wrote %s as %s -> %s\n", p.Name, as, path)
	}
	// Embedded is asked about the name the file now declares rather than the
	// one it was copied from: an export under a new name shadows nothing, and
	// saying it overrode the built-in it started from would be exactly wrong.
	if profile.Embedded(as) {
		fmt.Printf("it is in your profile directory, so it now overrides the built-in %s\n", as)
	}
	// The name agrees with the file, and nothing else does yet: the image, the
	// guest home and every comment in there still describe the profile this was
	// copied from. Say so where it is actionable, and name the command that
	// opens it, which is the next step of the recipe brig prints.
	if as != p.Name {
		fmt.Printf("its image, guest home and comments still describe %s\n", p.Name)
	}
	fmt.Printf("edit it with: brig agent edit %s\n", as)
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
//
// -y is the answer to the question below, given in advance, and it is spelled
// the way the secret verbs spell it.
func removeProfile(args []string) error {
	name, yes, err := nameAndYes("rm", "brig agent rm mine", args)
	if err != nil {
		return err
	}
	// Resolve through the registry rather than trusting the argument: it takes
	// aliases, and it names the file actually loaded, which need not be
	// <name>.yaml -- a file someone renamed by hand is an override whose
	// basename says nothing about which profile it serves.
	p, ok := profile.Lookup(name)
	if !ok {
		return notFoundf("no profile of your own named %q", name)
	}
	if _, ok := profile.Path(p.Name); !ok {
		return fmt.Errorf("%s is a built-in profile, so there is nothing to remove. "+
			"Import a profile of the same name to shadow it", p.Name)
	}
	if err := confirmRemoveProfile(name, p.Name, profile.Files(p.Name), yes); err != nil {
		return err
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

// confirmRemoveProfile names the files an rm is about to delete, and asks
// before deleting one the argument did not name.
//
// rm resolves the argument through the registry, which is what lets it work on
// an alias and on a file whose basename says nothing about the profile inside
// it. That resolution is also how `brig agent rm claude-code` could delete a
// file called mytool.yaml and exit 0 without a word: the file the person had a
// name for and the file brig found were different files, and only brig knew
// which.
//
// So the question is whether brig had to work anything out, and there are two
// ways it does. The argument may not be the profile's own name, which is the
// alias case: `brig agent rm claude` deletes whatever backs claude-code, and
// a file called claude.yaml declaring claude-code carries the stem the
// argument spells while being a profile the argument never said. And a file's
// basename may not be the argument at all, which is the renamed-by-hand case
// and the second-file-shadowing-the-first case. `brig agent rm mytool`
// against mytool.yaml declaring mytool is the one combination that is exactly
// what was typed -- what export writes, and the only case where nothing can
// surprise you.
//
// The message names every file profile.Remove will take rather than only the
// ones that raised the question: what is being agreed to is the delete, and a
// list one file short of it is the wrong thing to agree to.
//
// Without a terminal there is nobody to answer, and assuming yes would make
// the scripted case the one that cannot be stopped, so it refuses and names
// the flag that answers in advance -- the shape confirmDelete already uses,
// for the same reason.
func confirmRemoveProfile(arg, resolved string, files []string, yes bool) error {
	surprising := arg != resolved
	for _, f := range files {
		if stemOf(f) != arg {
			surprising = true
		}
	}
	if !surprising {
		return nil
	}
	list := strings.Join(files, ", ")
	// A verb that agrees with the list, because two files declaring one profile
	// is a case this message exists for rather than a curiosity.
	declares := "declares"
	if len(files) > 1 {
		declares = "declare"
	}
	// Named on stderr either way, so an rm inside a pipeline still says which
	// files it means where a person can see them. A run that answered in
	// advance is told rather than asked, because -y is an answer to this
	// question and not a reason to stop naming the files.
	if yes {
		fmt.Fprintf(os.Stderr, "brig: removing %s, which %s the %s profile\n",
			list, declares, resolved)
		return nil
	}
	if !wrap.IsTerminal(os.Stdin) {
		return fmt.Errorf("removing %q would delete %s, which brig worked out from the "+
			"name you typed rather than being told, and there is no terminal to ask on. "+
			"Pass -y to answer in advance: brig agent rm %s -y", arg, list, arg)
	}
	fmt.Fprintf(os.Stderr, "brig: removing %q deletes %s, which %s the %s profile. "+
		"Remove it? [y/N] ", arg, list, declares, resolved)
	line, err := readAnswer(os.Stdin)
	if err != nil {
		// EOF is the answer a closed stdin gives, and it is not yes.
		return fmt.Errorf("aborted: nothing was removed. To answer in advance: "+
			"brig agent rm %s -y", arg)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("aborted: nothing was removed")
}

// deprecated notes an old spelling once, on stderr so it never lands in
// something being piped. Kept for one release: one commit of published history
// is not long enough to have broken anyone's muscle memory on purpose.
func deprecated(old, replacement string) {
	warnf("`%s` is now `%s`", old, replacement)
}

// verbosity is how much this invocation says about itself, read off the global
// position before anything prints. See wrap.Verbosity for the rule.
//
// Package state rather than a value threaded through, because every notice
// below is at the top of its own call chain: there is nothing above them
// holding a writer to hand in, and they are lines brig prints about a command
// rather than values a command returns. run sets it on every invocation, so a
// test that drives run twice does not inherit the first one's level.
var verbosity = wrap.Normal

// warnf prints one of brig's own notices to stderr: a warning, a deprecation, a
// word about what a line now means.
//
// It stays in the default output -- a warning is an action -- and goes only
// under -q, which is a script asking for identifiers and errors. An error is
// never printed through here: errors are returned, and main prints them at
// every level.
func warnf(format string, a ...any) {
	if verbosity < wrap.Normal {
		return
	}
	fmt.Fprintf(os.Stderr, "brig: "+format+"\n", a...)
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
		warnf("hostCredential: in %s is deprecated and goes in the "+
			"next release -- it reads another application's keychain on every run. "+
			"Declare the credential under secrets: with sources: instead, then: "+
			"brig secret import %s", where, p.Name)
	}
}

// isTerminal reports whether stdin is a tty, which decides whether the guest
// exec allocates one. A headless `-p` run piped from a script must not get a
// pseudo-terminal.
func isTerminal() bool { return wrap.IsTerminal(os.Stdin) }
