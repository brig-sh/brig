package main

import (
	"embed"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/wrap"
)

// Shell completion, in two commands.
//
// `brig completion <shell>` writes a script to stdout and installs nothing.
// Where that script belongs differs per shell and per host, and a command that
// edits a startup file has a blast radius nothing else here has; the docs say
// the line to add.
//
// The scripts carry none of brig's vocabulary. Each one collects the words left
// of the cursor, hands them to `brig __complete`, and renders what comes back.
// So the verbs, the flags and where each flag is legal are declared once, in
// Go, beside the table that already decides them -- and the three shells cannot
// come to disagree about what brig accepts, because none of them knows.
//
// __complete answers on stdout and says nothing anywhere else. It is an ABI
// between a script and the binary that generated it, so it is in no help text.

//go:embed completions/brig.bash completions/brig.zsh completions/brig.fish
var completionScripts embed.FS

// completeVerb is the hidden engine's name. Two underscores, because it is not
// a word anyone types and it must not read like one.
const completeVerb = "__complete"

const completionUsage = `brig completion -- print a shell completion script

usage:
  brig completion bash    print the bash script
  brig completion zsh     print the zsh script
  brig completion fish    print the fish script

Nothing is installed for you. Write the script where your shell reads
completions from, or source it from your startup file:

  bash    brig completion bash >/usr/local/etc/bash_completion.d/brig
          (or: eval "$(brig completion bash)" in ~/.bashrc)
  zsh     brig completion zsh >"${fpath[1]}/_brig"
          then start a new shell, or run: compinit
  fish    brig completion fish >~/.config/fish/completions/brig.fish

Completion offers the verbs, the refs ` + "`brig ls`" + ` prints, and the flags that are
legal where the cursor is. brig's own flags stand on either side of the ref,
and are offered on both. What brig does not own it does not complete: once a
word or a flag it does not recognise has begun the agent's own arguments,
completion stops. On run the first word after the ref is the project directory
brig mounts, and directories are offered for it.
`

// completionCmd prints the script for one shell.
func completionCmd(out io.Writer, args []string) error {
	if len(args) == 0 {
		// An agent can be called completion, and the verb wins: dispatch reads
		// a verb before it reads a ref, deliberately, so that brig's own
		// vocabulary does not depend on which agents a host happens to have.
		// The agent is still reachable under run, so name that reading rather
		// than answer with a list of shells the reader did not ask about.
		if _, known := profile.Lookup("completion"); known {
			return usagef("completion needs a shell: bash, zsh or fish. " +
				"The agent of that name is reached as `brig run completion`")
		}
		return usagef("completion needs a shell: bash, zsh or fish")
	}
	switch args[0] {
	case "-h", "--help", "help":
		_, err := io.WriteString(out, completionUsage)
		return err
	}
	if len(args) > 1 {
		return usagef("unexpected argument %q; `brig completion` takes one shell", args[1])
	}
	shell := args[0]
	switch shell {
	case "bash", "zsh", "fish":
	default:
		return usagef("no completion script for %q; brig has bash, zsh and fish", shell)
	}
	script, err := completionScripts.ReadFile("completions/brig." + shell)
	if err != nil {
		return err
	}
	_, err = out.Write(script)
	return err
}

// The directive is the first line of an answer, and it says what the lines
// under it are -- or, where a candidate list cannot say it, what the shell
// should do instead.
const (
	// dirNames: candidates follow, already matched against the current word.
	dirNames = ":names"
	// dirDirs: complete directories. No candidates follow.
	dirDirs = ":dirs"
	// dirFiles: complete paths. No candidates follow.
	dirFiles = ":files"
	// dirNone: brig owns nothing at this position, so offer nothing. It has to
	// be distinguishable from a failure: a shell that reads "brig had no
	// answer" falls back to filenames, and the tail right of the ref is the one
	// place brig has to stay silent rather than guess.
	dirNone = ":none"
)

// completeCmd answers one completion request. It writes candidates to out and
// nothing to stderr, ever, and returns no error: a notice or a failure printed
// on a keystroke lands in the middle of the line the reader is typing.
func completeCmd(out io.Writer, words []string) {
	// The profile directory is read here rather than by the dispatcher, so that
	// a file that will not parse costs a candidate rather than a line of
	// diagnostics across the prompt. What loaded is completed; what did not is
	// not, and the next real command says why.
	_ = profile.Load(profile.Dir())
	// Nothing below prints a notice, and this is the belt to that braces: a
	// warning added anywhere the engine reaches stays silent on a keystroke.
	verbosity = wrap.Quiet

	directive, candidates := complete(words)
	fmt.Fprintln(out, directive)
	for _, c := range candidates {
		fmt.Fprintln(out, c)
	}
}

// verbs are the commands completion offers: the taught vocabulary, and only it.
//
// The retired spellings brig still answers to -- exec, shell, env, create,
// reset, profiles, policies -- are deliberately absent. They keep working and
// they say what replaced them; completing them would teach a word that is
// leaving, to the one reader who has not typed it yet.
var verbs = []string{
	"agent",
	"completion",
	"doctor",
	"info",
	"ls",
	"policy",
	"rm",
	"run",
	"secret",
	"sh",
	"stop",
	"telemetry",
	"version",
}

// refVerbs are the verbs whose operand is a session ref.
var refVerbs = map[string]bool{
	"run":  true,
	"sh":   true,
	"stop": true,
	"rm":   true,
	"info": true,
}

// complete is the whole decision: which words are already on the line, and what
// may stand where the cursor is.
//
// words is the line with `brig` dropped, and the word under the cursor is the
// last of them -- empty when the cursor sits on fresh whitespace. That the
// current word is always present, even empty, is what lets one function serve
// both "finish this token" and "what can come next".
func complete(words []string) (string, []string) {
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]
	before := words[:len(words)-1]
	// bash breaks a word on '=', so `--network=<cursor>` arrives with the
	// separator as the current word rather than as part of the flag. It is not
	// something the reader is trying to complete, so it reads as the empty
	// start of the value -- the normalisation _init_completion makes, for the
	// same reason.
	if cur == "=" {
		cur = ""
	}

	// The verb, and what stands between it and the cursor. Everything left of
	// the verb is a global flag or a mistake, and both are skipped: a token brig
	// would refuse is not a verb, so reading on is what keeps the rest of the
	// line completable.
	verb := ""
	var rest []string
	for i := 0; i < len(before); i++ {
		a := before[i]
		if strings.HasPrefix(a, "-") {
			if mine, takesValue := ours(a, posGlobal); mine && takesValue && !strings.Contains(a, "=") {
				i++
			}
			continue
		}
		verb, rest = a, before[i+1:]
		break
	}

	if verb == "" {
		if strings.HasPrefix(cur, "-") {
			return names(cur, flagSpellings(posGlobal, ""))
		}
		// Only the verbs. `brig claude@refactor` with no verb works, and is in
		// no help text on purpose; completing it would teach the second
		// spelling that the taught line already covers.
		return names(cur, verbs)
	}

	switch {
	case verb == "completion":
		if len(rest) > 0 {
			return dirNone, nil
		}
		return names(cur, []string{"bash", "fish", "zsh"})
	case verb == "ls":
		// One flag, no operand.
		if strings.HasPrefix(cur, "-") {
			return names(cur, []string{"--quiet", "-q"})
		}
		return dirNone, nil
	case verb == "doctor":
		if strings.HasPrefix(cur, "-") {
			return names(cur, []string{"--json"})
		}
		if len(bareWords(rest, posRun)) > 0 {
			return dirNone, nil
		}
		return names(cur, agentNames())
	case groups[verb] != nil:
		return completeGroup(verb, rest, cur)
	case refVerbs[verb]:
		return completeRunLine(verb, rest, cur)
	}
	return dirNone, nil
}

// completeRunLine answers between a lifecycle verb and the end of the line.
//
// The three positions a brig line has are the whole of the logic here: brig's
// own flags up to the ref, the ref, and then the agent's own vocabulary, which
// brig does not complete because it does not own it.
func completeRunLine(verb string, rest []string, cur string) (string, []string) {
	// --all is read before the run line, removes every sandbox and refuses any
	// argument at all, so a line carrying it names no session and there is
	// nothing further to offer. See removeAll.
	if hasSpelling(rest, "--all") {
		return dirNone, nil
	}

	line := walkRunLine(verb, rest)
	if line.tailBegun {
		// The vocabulary from here is the agent's. brig has no list of another
		// program's flags and must not guess at one -- not for a flag name, and
		// not for a flag's value either.
		return dirNone, nil
	}

	// The value of one of brig's own flags, when that is what the cursor is on.
	if line.pending != "" && !strings.HasPrefix(cur, "-") {
		return operandCandidates(flagValue(line.pending), cur)
	}

	if strings.HasPrefix(cur, "-") {
		// Brig's own flags stand on either side of the ref: split reads them
		// wherever they are on the run line, and stops only at a flag brig does
		// not own. Offering them only up to the ref would leave completion
		// silent about `brig run claude --mem`, a line brig reads.
		flags := flagSpellings(posRun, verb)
		if verb == "rm" && !line.refGiven {
			// --all is not on the run line and not in the table it is built
			// from. It replaces the ref rather than joining it, so it is
			// offered only where the ref would have gone.
			flags = append(flags, "--all")
			sort.Strings(flags)
		}
		return names(cur, flags)
	}

	if !line.refGiven {
		return names(cur, refsFor(verb))
	}
	// On run the first bare word after the ref is the project brig mounts, and
	// a project is a directory. Every other verb, and every word after that
	// one, begins the agent's own arguments.
	if verb == "run" && !line.projectTaken {
		return dirDirs, nil
	}
	return dirNone, nil
}

// runLine is how far a lifecycle line has got by the time the cursor is
// reached.
//
// It answers the same question split answers, and deliberately in the same
// order, because the two must agree about where brig's arguments stop. Where
// they disagree, completion either withholds a flag brig would have read or
// offers one into the agent's argv -- and the second is the worse of the two,
// since it puts brig's vocabulary in front of a reader who is typing another
// program's.
type runLine struct {
	// refGiven: the session ref has been named.
	refGiven bool
	// projectTaken: run's one project word has been given.
	projectTaken bool
	// tailBegun: brig's parsing has ended and the rest is the agent's -- `--`,
	// a flag brig does not own once the ref is named, or a bare word past the
	// project.
	tailBegun bool
	// pending is the brig flag whose value the cursor's word would be, when the
	// line ends on one. Empty when it does not.
	pending string
}

func walkRunLine(verb string, args []string) runLine {
	var line runLine
	takesProject := verb == "run"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			line.tailBegun = true
			return line
		case !strings.HasPrefix(a, "-"):
			if !line.refGiven {
				line.refGiven = true
				continue
			}
			if takesProject && !line.projectTaken {
				line.projectTaken = true
				continue
			}
			line.tailBegun = true
			return line
		default:
			mine, takesValue := ours(a, posRun)
			if !mine {
				if line.refGiven {
					line.tailBegun = true
					return line
				}
				// Before the ref, a flag brig does not have is a mistake the
				// run itself names. Reading past it keeps the rest of the line
				// completable, which is what someone fixing the typo wants.
				continue
			}
			if !takesValue || strings.Contains(a, "=") {
				continue
			}
			// The flag's value, which a shell that breaks words on '=' hands
			// over as a separator token and then the value itself. Consuming
			// only one of the two would leave the value standing where a bare
			// word goes, and be read as the ref.
			i++
			if i < len(args) && args[i] == "=" {
				i++
			}
		}
	}
	line.pending = pendingFlag(args)
	return line
}

// pendingFlag is the brig flag the cursor's word would be the value of: the
// last token on the line, when that is a flag of brig's that takes one.
//
// The `=` a shell may have split out of an inline value is stepped over, so
// `--network=<cursor>` completes the postures the spaced spelling does.
func pendingFlag(args []string) string {
	if len(args) == 0 {
		return ""
	}
	last := args[len(args)-1]
	if last == "=" && len(args) > 1 {
		last = args[len(args)-2]
	}
	mine, takesValue := ours(last, posRun)
	if !mine || !takesValue || strings.Contains(last, "=") {
		return ""
	}
	return last
}

// hasSpelling reports whether a line carries one exact flag spelling.
func hasSpelling(args []string, spelling string) bool {
	for _, a := range args {
		if a == spelling {
			return true
		}
	}
	return false
}

// sessionVerbs act on a sandbox that already exists, so they are completed
// from the sessions there are rather than from the agents there could be.
//
// run and sh both start one, and info reads a profile without booting
// anything, so all three take an agent that has never run. stop and rm do not:
// offering every agent there would offer a session that is not there, and on a
// host with nothing running the honest answer is nothing.
var sessionVerbs = map[string]bool{
	"stop": true,
	"rm":   true,
}

func refsFor(verb string) []string {
	if sessionVerbs[verb] {
		return wrap.SessionRefs()
	}
	return refCandidates()
}

// operand is what a word or a flag's value names.
type operand int

const (
	// opNothing: brig has no list for this. A session label, a new agent's
	// name, a secret's value -- words brig cannot enumerate, or should not.
	opNothing operand = iota
	opAgent
	// opFileAgent is an agent with a file of its own. edit opens that file and
	// rm removes it, so a built-in is a candidate both commands refuse.
	opFileAgent
	opPolicy
	opRef
	opDir
	opFile
	// opNetwork is the posture set --network takes.
	opNetwork
)

func operandCandidates(kind operand, cur string) (string, []string) {
	switch kind {
	case opAgent:
		return names(cur, agentNames())
	case opFileAgent:
		return names(cur, fileAgentNames())
	case opPolicy:
		return names(cur, policyNames())
	case opRef:
		return names(cur, refCandidates())
	case opDir:
		return dirDirs, nil
	case opFile:
		return dirFiles, nil
	case opNetwork:
		return names(cur, networkModes)
	}
	return dirNone, nil
}

// sub is one subcommand of a noun group: its flags, the flags that take a
// value, and what its bare words name, in order.
//
// The noun groups build their arguments with a flag.FlagSet of their own,
// inside the function that runs them, so there is no table to read them off.
// This is that table, for completion only. A subcommand missing from here
// completes nothing, which is the safe direction to be wrong in.
type sub struct {
	name string
	// flags take no value.
	flags []string
	// values are the flags that do, and what the value names.
	values map[string]operand
	// operands is what the bare words after the subcommand name are. A word
	// past the last slot completes nothing.
	operands []operand
}

// groups is the noun commands and what stands under each.
//
// Secret names are absent on purpose. Listing them opens the store -- on macOS
// that is `security dump-keychain`, and a secret-service backend can put an
// unlock prompt on the screen -- and a keystroke must not do either. The
// subcommands complete; their operands do not.
var groups = map[string][]sub{
	"agent": {
		{name: "ls"},
		{name: "show", flags: []string{"--json"}, operands: []operand{opAgent}},
		{
			name:     "new",
			flags:    []string{"--json", "--force", "-f"},
			values:   map[string]operand{"--from": opAgent},
			operands: []operand{opNothing},
		},
		{name: "edit", operands: []operand{opFileAgent}},
		{name: "rm", flags: []string{"--yes", "-y"}, operands: []operand{opFileAgent}},
		{name: "import", operands: []operand{opFile}},
		{
			name:     "export",
			flags:    []string{"--json", "--force", "-f"},
			operands: []operand{opAgent, opFile},
		},
	},
	"policy": {
		{name: "ls"},
		{name: "create", operands: []operand{opNothing}},
		{name: "edit", operands: []operand{opPolicy}},
		{name: "show", flags: []string{"--json"}, operands: []operand{opPolicy}},
		{name: "rm", flags: []string{"--force", "-f"}, operands: []operand{opPolicy}},
		{
			name:     "attach",
			values:   map[string]operand{"--name": opNothing, "-n": opNothing},
			operands: []operand{opPolicy, opAgent},
		},
		{
			name:     "detach",
			values:   map[string]operand{"--name": opNothing, "-n": opNothing},
			operands: []operand{opPolicy, opAgent},
		},
		{
			name:     "check",
			values:   map[string]operand{"--name": opNothing, "-n": opNothing},
			operands: []operand{opAgent},
		},
	},
	"secret": {
		{
			name:     "create",
			flags:    []string{"--stdin"},
			values:   map[string]operand{"--file": opFile, "-f": opFile},
			operands: []operand{opNothing},
		},
		{name: "read", operands: []operand{opNothing}},
		{
			name:     "update",
			flags:    []string{"--stdin"},
			values:   map[string]operand{"--file": opFile, "-f": opFile},
			operands: []operand{opNothing},
		},
		{name: "delete", flags: []string{"--yes", "-y"}, operands: []operand{opNothing}},
		{name: "ls"},
		{
			name:     "import",
			flags:    []string{"--dry-run", "--yes", "-y"},
			values:   map[string]operand{"--from-command": opNothing},
			operands: []operand{opAgent},
		},
	},
	"telemetry": {
		{name: "status"},
		{name: "on"},
		{name: "off"},
	},
}

// completeGroup answers under a noun command: the subcommand, then whatever
// that subcommand takes.
func completeGroup(verb string, rest []string, cur string) (string, []string) {
	subs := groups[verb]
	name := ""
	var words []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if strings.HasPrefix(a, "-") {
			if takesGroupValue(subs, name, a) && !strings.Contains(a, "=") {
				// The value, and the separator before it when a shell has
				// broken the inline spelling into tokens. Consuming only the
				// separator would leave the value standing where an operand
				// goes and shift every slot after it by one.
				i++
				if i < len(rest) && rest[i] == "=" {
					i++
				}
			}
			continue
		}
		if name == "" {
			name = a
			continue
		}
		words = append(words, a)
	}

	if name == "" {
		if strings.HasPrefix(cur, "-") {
			// A flag before the subcommand belongs to no subcommand yet.
			return dirNone, nil
		}
		return names(cur, subNames(subs))
	}
	s, ok := findSub(subs, name)
	if !ok {
		return dirNone, nil
	}

	// The value of a flag that takes one.
	if len(rest) > 0 && !strings.HasPrefix(cur, "-") {
		if kind, ok := s.values[lastFlag(rest)]; ok {
			return operandCandidates(kind, cur)
		}
	}
	if strings.HasPrefix(cur, "-") {
		spellings := append([]string{}, s.flags...)
		for f := range s.values {
			spellings = append(spellings, f)
		}
		sort.Strings(spellings)
		return names(cur, spellings)
	}
	if len(words) >= len(s.operands) {
		return dirNone, nil
	}
	return operandCandidates(s.operands[len(words)], cur)
}

func findSub(subs []sub, name string) (sub, bool) {
	for _, s := range subs {
		if s.name == name {
			return s, true
		}
	}
	return sub{}, false
}

func subNames(subs []sub) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.name)
	}
	sort.Strings(out)
	return out
}

// takesGroupValue reports whether a token is a flag of this subcommand that
// consumes the argument after it, so that walking a group's line does not read
// a flag's value as one of its bare words.
func takesGroupValue(subs []sub, name, arg string) bool {
	s, ok := findSub(subs, name)
	if !ok {
		return false
	}
	spelling, _, _ := strings.Cut(arg, "=")
	_, takes := s.values[spelling]
	return takes
}

// lastFlag is the flag spelling the cursor may be completing the value of: the
// token immediately left of the cursor, with the `=` a shell may have split out
// of an inline value stepped over.
//
// bash breaks words on `=`, so `--network=sh` reaches here as three tokens.
// Reading past the separator is what makes the inline spelling of a flag
// complete the same way the spaced one does.
func lastFlag(rest []string) string {
	last := rest[len(rest)-1]
	if last == "=" && len(rest) > 1 {
		last = rest[len(rest)-2]
	}
	return last
}

// flagValue is what a run-line flag's value names.
//
// A closed set is the payoff here: --network has three postures and nothing
// else, and a reader who cannot remember them is exactly who completion is for.
// The rest name a host path, or a number brig cannot guess.
func flagValue(flag string) operand {
	switch flag {
	case "--home", "-w", "--workspace":
		return opDir
	case "--image", "-t":
		return opFile
	case "--network":
		return opNetwork
	}
	return opNothing
}

// networkModes is the posture set --network takes. Spelled here rather than
// read from wrap because the parser there maps words onto values and does not
// enumerate them.
var networkModes = []string{"isolated", "offline", "shared"}

// flagSpellings is what brig owns at one position, as a reader types it.
//
// Read off the position table, so a flag added there is completed without
// anything here changing. What is left out is what the help text leaves out: a
// spelling on its way out, and a position on its way out, are not words to
// teach a reader who has not typed them yet.
func flagSpellings(at position, verb string) []string {
	var out []string
	for _, f := range brigFlags {
		if f.position != at || f.retiredAs != "" || f.undocumented {
			continue
		}
		if deprecatedFlags["--"+f.long] != "" {
			continue
		}
		if at == posRun && verb != "" && !honorsRunLine(verb, f.long) {
			continue
		}
		out = append(out, "--"+f.long)
		if f.short != "" && deprecatedFlags["-"+f.short] == "" {
			out = append(out, "-"+f.short)
		}
	}
	sort.Strings(out)
	return out
}

// bareWords are the words at one position that are not flags and not a flag's
// value. On a run line the first of them is the ref.
func bareWords(args []string, at position) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if mine, takesValue := ours(a, at); mine && takesValue && !strings.Contains(a, "=") {
				i++
				if i < len(args) && args[i] == "=" {
					i++
				}
			}
			continue
		}
		if a == "=" {
			// A separator with no flag of brig's before it. Neither it nor the
			// value after it is a word of brig's own.
			i++
			continue
		}
		out = append(out, a)
	}
	return out
}

// agentNames is every spelling that reaches an agent: the profile names and the
// short forms that stand for them.
func agentNames() []string {
	var out []string
	for _, name := range profile.Names() {
		out = append(out, name)
		out = append(out, profile.Aliases(name)...)
	}
	sort.Strings(out)
	return out
}

// fileAgentNames is every spelling that reaches an agent brig loaded from a
// file. A built-in has no file to open or remove.
func fileAgentNames() []string {
	var out []string
	for _, name := range profile.Names() {
		if _, ok := profile.Path(name); !ok {
			continue
		}
		out = append(out, name)
		out = append(out, profile.Aliases(name)...)
	}
	sort.Strings(out)
	return out
}

// refCandidates is what a lifecycle verb takes: every agent, and every session
// that exists under one.
//
// Both, and in one list, because they are one grammar: `claude` is that agent's
// default session and `claude@refactor` is a session of its own. Offering the
// agents alone would answer half the question, and offering the labelled refs
// alone would hide the session most lines mean.
func refCandidates() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, name := range agentNames() {
		add(name)
	}
	for _, ref := range wrap.SessionRefs() {
		add(ref)
	}
	sort.Strings(out)
	return out
}

// policyNames is every policy that loads, and nothing about the ones that do
// not: a name completion offers has to be a name the next command accepts.
func policyNames() []string {
	entries, _ := policy.LoadAll(policy.Dir())
	out := make([]string, 0, len(entries))
	for name := range entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// names matches candidates against the word being typed.
//
// Matched here rather than left to the shell so that one Go test is the whole
// truth about what a keystroke offers. The shells filter again; agreeing with
// them costs nothing.
//
// A long spelling under the cursor drops the short forms: someone who has typed
// two dashes has said which spelling they want, and offering `-q` against `--`
// is a candidate that cannot be completed.
func names(cur string, candidates []string) (string, []string) {
	long := strings.HasPrefix(cur, "--")
	var out []string
	for _, c := range candidates {
		if long && !strings.HasPrefix(c, "--") {
			continue
		}
		if strings.HasPrefix(c, cur) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return dirNone, nil
	}
	return dirNames, out
}
