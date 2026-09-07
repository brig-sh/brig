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
// `brig completion <shell>` prints a script to stdout. It installs nothing:
// the install path differs per shell and per host, and the docs give the line.
//
// The scripts hold no brig vocabulary. Each collects the words left of the
// cursor, passes them to `brig __complete`, and renders the reply. Verbs, flags
// and flag positions are therefore declared once, in Go, next to brigFlags.
//
// __complete is an ABI between a script and the binary that printed it, so it
// is in no help text.

//go:embed completions/brig.bash completions/brig.zsh completions/brig.fish
var completionScripts embed.FS

// completeVerb is the engine's name. Underscore-prefixed: it is not a command
// anyone types.
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
		// Verb dispatch precedes the ref fallback, so an agent named
		// "completion" is not reachable as `brig completion`. That applies to
		// every verb and is intentional: the command set must not depend on
		// which agents are installed. Point at the spelling that still works.
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

// The directive is the first line of a reply. It says what the lines below it
// are, or what the shell should do when a candidate list cannot express it.
const (
	// dirNames: candidates follow, already matched against the current word.
	dirNames = ":names"
	// dirDirs: complete directories. No candidates follow.
	dirDirs = ":dirs"
	// dirFiles: complete paths. No candidates follow.
	dirFiles = ":files"
	// dirNone: offer nothing. Distinct from a failed call, which the scripts
	// treat as "fall back to filenames" -- wrong inside the agent's argv.
	dirNone = ":none"
)

// completeCmd answers one completion request. It writes only candidates to out,
// never to stderr, and returns no error. Anything on stderr would print over
// the line the user is typing.
func completeCmd(out io.Writer, words []string) {
	// Loaded here rather than by the dispatcher, which warns about files that
	// will not parse. Profiles that load are completed; the rest are skipped,
	// and the next real command reports them.
	_ = profile.Load(profile.Dir())
	// Suppresses any warning added later in a path the engine reaches.
	verbosity = wrap.Quiet

	directive, candidates := complete(words)
	fmt.Fprintln(out, directive)
	for _, c := range candidates {
		fmt.Fprintln(out, c)
	}
}

// verbs are the commands completion offers: the documented set only.
//
// The retired spellings -- exec, shell, env, create, reset, profiles, policies
// -- are excluded. They still work and print what replaced them, but they are
// removed in v0.3, so completion does not teach them.
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

// complete decides what may stand where the cursor is.
//
// words is the line with `brig` dropped; the last element is the word under the
// cursor, empty when the cursor is on fresh whitespace. Always passing that
// element, even empty, is what lets one function serve both "finish this token"
// and "what comes next".
func complete(words []string) (string, []string) {
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]
	before := words[:len(words)-1]
	// bash splits on '=', so `--network=<cursor>` arrives with "=" as the
	// current word. Treat it as an empty value prefix, as _init_completion
	// does.
	if cur == "=" {
		cur = ""
	}

	// Find the verb and the tokens between it and the cursor. Tokens left of
	// the verb are global flags, or typos; both are skipped, so a typo does not
	// make the rest of the line uncompletable.
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
		// Verbs only. A bare ref with no verb also works but is undocumented,
		// and `brig run <ref>` already covers it.
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

// completeRunLine answers for a lifecycle verb: flags, then the ref, then
// run's project directory. Past that the tokens are the agent's.
func completeRunLine(verb string, rest []string, cur string) (string, []string) {
	// takeAll strips --all before the run line is parsed, and removeAll then
	// rejects every remaining argument. Nothing else can appear on this line.
	if hasSpelling(rest, "--all") {
		return dirNone, nil
	}

	line := walkRunLine(verb, rest)
	if line.tailBegun {
		// Inside the agent's argv. brig has no list of another program's flags,
		// so it offers neither names nor values here.
		return dirNone, nil
	}

	if line.pending != "" && !strings.HasPrefix(cur, "-") {
		return operandCandidates(flagValue(line.pending), cur)
	}

	// `--flag=value` with the cursor inside the value. Only bash splits on
	// "=", so in zsh and fish the whole thing is the current word and would
	// otherwise be matched against flag names, which it cannot match.
	if flag, prefix, ok := inlineValue(cur); ok {
		mine, takesValue := ours(flag, posRun)
		if !mine || !takesValue {
			return dirNone, nil
		}
		directive, candidates := operandCandidates(flagValue(flag), prefix)
		return qualify(flag, directive, candidates)
	}

	if strings.HasPrefix(cur, "-") {
		// split() reads brig's own flags on both sides of the ref, stopping
		// only at a flag brig does not own, so offer them on both sides too.
		// `brig run claude --mem 4096` is a line brig parses.
		flags := flagSpellings(posRun, verb)
		if verb == "rm" && !line.refGiven {
			// --all is not in brigFlags: it is read before the run line, and it
			// replaces the ref rather than accompanying it.
			flags = append(flags, "--all")
			sort.Strings(flags)
		}
		return names(cur, flags)
	}

	if !line.refGiven {
		return names(cur, refsFor(verb))
	}
	// On run the first bare word after the ref is the project directory. On
	// every other verb, and for later words, the agent's argv starts here.
	if verb == "run" && !line.projectTaken {
		return dirDirs, nil
	}
	return dirNone, nil
}

// runLine is how far a lifecycle line has been parsed at the cursor.
//
// It resolves the same boundary split() resolves, in the same order, because
// the two must agree on where brig's arguments end. If completion stops early
// it withholds a flag brig accepts; if it stops late it offers brig's flags
// inside the agent's argv.
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
				// Before the ref, an unknown flag is a typo, which the run
				// itself reports. Skip it so the rest of the line still
				// completes.
				continue
			}
			if !takesValue || strings.Contains(a, "=") {
				continue
			}
			// Consume the value. A shell that splits on '=' delivers it as two
			// tokens ("=" then the value); consuming only the "=" would leave
			// the value to be counted as a positional, i.e. as the ref.
			i++
			if i < len(args) && args[i] == "=" {
				i++
			}
		}
	}
	line.pending = pendingFlag(args)
	return line
}

// pendingFlag returns the flag whose value the cursor is on: the last token,
// when it is a brig flag that takes a value. A trailing "=" is skipped, so
// `--network=<cursor>` completes like `--network <cursor>`.
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

// hasSpelling reports whether args contains this exact token.
func hasSpelling(args []string, spelling string) bool {
	for _, a := range args {
		if a == spelling {
			return true
		}
	}
	return false
}

// sessionVerbs require an existing sandbox, so they complete from the session
// index rather than from the profile registry.
//
// run and sh create one, and info reads a profile without booting, so those
// three accept an agent that has never run. stop and rm do not: offering every
// agent would offer sessions that do not exist.
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
	// opNothing: no candidate list. A new agent's name, a session label, a
	// secret name -- values brig cannot enumerate, or must not.
	opNothing operand = iota
	opAgent
	// opFileAgent is an agent backed by a file. `agent edit` opens that file
	// and `agent rm` deletes it; both reject a built-in.
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

// sub is one subcommand of a noun group: its boolean flags, its value-taking
// flags, and what its positional arguments name, in order.
//
// Each noun subcommand builds a flag.FlagSet inside the function that runs it,
// so there is nothing for completion to read. This table duplicates that, for
// completion only; TestGroupsTableCoversEveryGroupFlag guards the drift. A
// subcommand missing here completes nothing rather than something wrong.
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

// groups is the noun commands and their subcommands.
//
// Secret names are deliberately absent: listing them opens the store, which on
// macOS runs `security dump-keychain`, and on Linux can raise an unlock prompt
// from the secret-service backend. Subcommands complete; their name operands
// do not.
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
				// Consume the value, plus the "=" when a shell split the
				// inline spelling into tokens. Consuming only the "=" leaves
				// the value to be counted as a positional, shifting every
				// operand index by one.
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
			// No subcommand yet, so there is no flag set to draw from.
			return dirNone, nil
		}
		return names(cur, subNames(subs))
	}
	s, ok := findSub(subs, name)
	if !ok {
		return dirNone, nil
	}

	// The cursor is on a value-taking flag's value.
	if len(rest) > 0 && !strings.HasPrefix(cur, "-") {
		if kind, ok := s.values[lastFlag(rest)]; ok {
			return operandCandidates(kind, cur)
		}
	}
	if flag, prefix, ok := inlineValue(cur); ok {
		kind, takesValue := s.values[flag]
		if !takesValue {
			return dirNone, nil
		}
		directive, candidates := operandCandidates(kind, prefix)
		return qualify(flag, directive, candidates)
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

// takesGroupValue reports whether arg is a flag of this subcommand that
// consumes the next token, so the walk does not count a value as a
// positional.
func takesGroupValue(subs []sub, name, arg string) bool {
	s, ok := findSub(subs, name)
	if !ok {
		return false
	}
	spelling, _, _ := strings.Cut(arg, "=")
	_, takes := s.values[spelling]
	return takes
}

// lastFlag returns the token left of the cursor, skipping a trailing "=" so
// that `--file=<cursor>` resolves to `--file`. bash splits on "=", so the
// inline spelling arrives as three tokens.
func lastFlag(rest []string) string {
	last := rest[len(rest)-1]
	if last == "=" && len(rest) > 1 {
		last = rest[len(rest)-2]
	}
	return last
}

// inlineValue splits a `--flag=value` token: the flag, and the part of the
// value typed so far.
//
// bash has "=" in COMP_WORDBREAKS and sends the two halves as separate tokens,
// which lastFlag handles. zsh and fish send one token, which arrives as the
// current word and reaches here.
func inlineValue(cur string) (flag, prefix string, ok bool) {
	if !strings.HasPrefix(cur, "-") {
		return "", "", false
	}
	flag, prefix, found := strings.Cut(cur, "=")
	if !found {
		return "", "", false
	}
	return flag, prefix, true
}

// qualify rewrites value candidates as `--flag=value`.
//
// The shell replaces the whole current word, and that word includes the flag,
// so a bare value would replace `--network=is` with `isolated`. Directives that
// hand the slot to the shell cannot be qualified this way -- the shell would
// complete a path against the flag text -- so they become dirNone. That leaves
// an inline path uncompleted in zsh and fish, which is what it already was.
func qualify(flag string, directive string, candidates []string) (string, []string) {
	if directive != dirNames {
		return dirNone, nil
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, flag+"="+c)
	}
	return dirNames, out
}

// flagValue maps a run-line flag to what its value names. --network is the
// only closed set; the rest take a host path or a number.
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

// networkModes is the value set --network accepts. Duplicated from
// wrap.ParseNetwork, which maps strings to values without exposing the list.
var networkModes = []string{"isolated", "offline", "shared"}

// flagSpellings returns brig's own flags legal at one position, as typed.
//
// Read from brigFlags, so a flag added there needs no change here. Excluded:
// deprecated spellings, positions being retired (retiredAs), and undocumented
// spellings -- all still accepted, none of them taught.
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

// bareWords returns the positional arguments at one position: tokens that are
// neither a flag nor a flag's value.
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
			// A "=" with no brig flag before it. Skip it and its value.
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

// fileAgentNames returns the spellings of file-backed agents only. A built-in
// has no file to open or delete.
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

// refCandidates returns every agent plus every existing session, in one list.
// They are one grammar: `claude` is that agent's default session,
// `claude@refactor` is a labelled one.
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

// policyNames returns the policies that load. One that does not load is
// omitted: every command that takes a policy name would reject it.
func policyNames() []string {
	entries, _ := policy.LoadAll(policy.Dir())
	out := make([]string, 0, len(entries))
	for name := range entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// names filters candidates by the word being typed.
//
// Filtered here rather than in the shells so the Go tests cover exactly what a
// keystroke offers; the shells filter again, harmlessly.
//
// A "--" prefix drops the short forms, which cannot match it anyway.
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
