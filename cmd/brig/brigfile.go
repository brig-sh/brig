package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/brig-sh/brig/internal/brigfile"
	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/profile"
)

// The verbs that read a repository's Brigfile: target ls, config and init.
//
// They only read. A Brigfile is the repository's, not the reader's, so these
// are how a reader sees what it declares before anything runs it. They look
// for no runtime, open no secret store, pull nothing and start no telemetry,
// so they answer on a machine with no runtime installed. init writes the one
// file it creates, and nothing else.

const targetUsage = `brig target -- the targets a repository's Brigfile declares

usage:
  brig target ls [-f <path>] [--json]   list the targets

A target pairs one profile with policies, a size and default arguments. The
default target is marked with *.

flags:
  -f, --file PATH  read this Brigfile instead of looking for one
      --json       print the listing as JSON

brig looks for the Brigfile at the top of the Git worktree, or in the current
directory outside Git. brig run --target runs a target, once you approve it.
`

const configUsage = `brig config -- what a repository's Brigfile declares, and where each part comes from

usage:
  brig config [-f <path>] [--json]
  brig config --target <name> [-f <path>] [--json]

With no target it prints the project: its Brigfile, its root, the default
target, and every profile, policy and target with its source. With --target
it prints that target resolved: its profile, the cpus, mem and image it gets
and where each value comes from, its policies in order, and its arguments.
Settings in your environment and flags on a run line are not applied here.

flags:
      --target NAME  resolve one target
  -f, --file PATH    read this Brigfile instead of looking for one
      --json         print the result as JSON

brig help lists every command, every flag and every setting.
`

const initUsage = `brig init -- write a starter Brigfile

usage:
  brig init [-f <path>]

Writes a Brigfile at the top of the Git worktree, or in the current directory
outside Git. It declares one target, dev, that runs the claude-code profile
brig ships. brig init never replaces a file that is already there.

flags:
  -f, --file PATH  write the Brigfile at this path instead

brig help lists every command, every flag and every setting.
`

// brigfileLine is what the Brigfile verbs read after the verb.
type brigfileLine struct {
	file   string
	target string
	json   bool
}

// parseBrigfileLine reads one Brigfile verb's flags. Every one takes -f; config
// also takes --target; the two read verbs take --json. A bare word is refused:
// none of them takes an operand, and config's target is named by its flag so
// that a later `brig run --target` reads the same way.
func parseBrigfileLine(command string, args []string, takesTarget, takesJSON bool) (brigfileLine, error) {
	var l brigfileLine
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.StringVar(&l.file, "f", "", "")
	fs.StringVar(&l.file, "file", "", "")
	takes := "-f/--file"
	if takesTarget {
		fs.StringVar(&l.target, "target", "", "")
		takes += ", --target"
	}
	if takesJSON {
		fs.BoolVar(&l.json, "json", false, "")
		takes += ", --json"
	}
	words, err := parseWords(command, takes, fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return l, err
		}
		return l, usagef("%s", err)
	}
	if len(words) > 0 {
		if takesTarget {
			return l, usagef("unexpected argument %q; `%s` names a target with --target", words[0], command)
		}
		return l, usagef("unexpected argument %q; `%s` takes no arguments", words[0], command)
	}
	if takesTarget && seen(fs, "target") && l.target == "" {
		return l, usagef("--target needs a target name")
	}
	l.json = l.json || globalJSON
	return l, nil
}

// loadBrigfile loads the Brigfile -f names, or the one for the working
// directory's project.
//
// Only a Brigfile that is not there is "no such thing". A file the Brigfile
// names that is missing is a broken Brigfile, and the loader's error, which
// names the path and the field, is returned as it is.
func loadBrigfile(file string) (*brigfile.Loaded, error) {
	path := file
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root, err := brigfile.Root(wd)
		if err != nil {
			return nil, err
		}
		path, err = brigfile.Discover(wd)
		if errors.Is(err, brigfile.ErrNotFound) {
			return nil, notFoundf("no Brigfile in %s. brig looks at the top of the Git worktree, "+
				"or in the current directory outside Git. `brig init` writes a starter there, "+
				"or name a Brigfile with -f <path>", root)
		}
		if err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, notFoundf("no Brigfile at %s. `brig init -f %s` writes a starter there", path, shellWord(path))
	}
	return brigfile.Load(path)
}

// resolveTarget is Loaded.Target with the unknown name made a not-found error
// that lists what the Brigfile has, so the reader does not need a second
// command to find the right word.
func resolveTarget(l *brigfile.Loaded, name string) (brigfile.ResolvedTarget, error) {
	if _, ok := l.Project.Targets[name]; !ok {
		return brigfile.ResolvedTarget{}, notFoundf("%s has no target %q; its targets are %s",
			l.Path, name, strings.Join(targetNamesOf(l), ", "))
	}
	return l.Target(name)
}

func targetNamesOf(l *brigfile.Loaded) []string {
	names := make([]string, 0, len(l.Project.Targets))
	for name := range l.Project.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// shown is a string the repository wrote, made safe to print. One holding a
// control character is quoted, so an escape sequence in a Brigfile reaches
// the reader's terminal as text and cannot move the cursor or clear the
// screen. Names are checked by the loader; paths, images and arguments are
// not limited to printable characters there.
func shown(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

// shellWord is a path as the reader would type it after -f.
func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '~' ||
			(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sourceJSON is one alias and where it was declared.
type sourceJSON struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	// IgnoredUserFile is set on a builtin: profile when the reader has a
	// file of their own that overrides that built-in. The Brigfile does not
	// read it.
	IgnoredUserFile string `json:"ignoredUserFile,omitempty"`
}

// targetRowJSON is one target as target ls lists it.
type targetRowJSON struct {
	Name     string       `json:"name"`
	Default  bool         `json:"default"`
	Profile  sourceJSON   `json:"profile"`
	Policies []sourceJSON `json:"policies"`
}

type intSettingJSON struct {
	Value int `json:"value"`
	// From is "target" for a value the target's overrides set, and "profile"
	// for one the target's profile set.
	From string `json:"from"`
}

type stringSettingJSON struct {
	Value string `json:"value"`
	From  string `json:"from"`
}

// resolvedTargetJSON is one target as config --target resolves it.
type resolvedTargetJSON struct {
	Name     string            `json:"name"`
	Default  bool              `json:"default"`
	Profile  sourceJSON        `json:"profile"`
	CPUs     intSettingJSON    `json:"cpus"`
	Mem      intSettingJSON    `json:"mem"`
	Image    stringSettingJSON `json:"image"`
	Policies []sourceJSON      `json:"policies"`
	Args     []string          `json:"args"`
}

// configJSON is the Config payload: the project, and with --target the one
// target resolved.
type configJSON struct {
	Name          string              `json:"name,omitempty"`
	Path          string              `json:"path"`
	Root          string              `json:"root"`
	DefaultTarget string              `json:"defaultTarget,omitempty"`
	Profiles      []sourceJSON        `json:"profiles"`
	Policies      []sourceJSON        `json:"policies"`
	Targets       []targetRowJSON     `json:"targets"`
	Target        *resolvedTargetJSON `json:"target,omitempty"`
}

// ignoredUserFile names the reader's own file that overrides a built-in
// profile, when the Brigfile asks for that built-in. brigfile resolves
// builtin:<name> from what brig ships so that one Brigfile means one profile
// on every machine; the reader's file still decides `brig run <name>`, and
// two answers to "which claude-code" are worth naming.
func ignoredUserFile(rp brigfile.ResolvedProfile) string {
	if rp.Source.Kind != brigfile.SourceBuiltin || !profile.OverridesBuiltIn(rp.Source.Ref) {
		return ""
	}
	path, _ := profile.Path(rp.Source.Ref)
	return path
}

func profileSource(rp brigfile.ResolvedProfile) sourceJSON {
	return sourceJSON{Alias: rp.Alias, Source: rp.Source.String(), IgnoredUserFile: ignoredUserFile(rp)}
}

// targetRows lists every target in name order. Each is resolved, so the
// policies are the ones a run would attach: the target's own, then those a
// repository profile lists.
func targetRows(l *brigfile.Loaded) ([]targetRowJSON, error) {
	rows := []targetRowJSON{}
	for _, name := range targetNamesOf(l) {
		rt, err := l.Target(name)
		if err != nil {
			return nil, err
		}
		row := targetRowJSON{
			Name:     name,
			Default:  name == l.Project.DefaultTarget,
			Profile:  profileSource(rt.Profile),
			Policies: []sourceJSON{},
		}
		for _, p := range rt.Policies {
			row.Policies = append(row.Policies, sourceJSON{Alias: p.Alias, Source: p.Source.String()})
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func aliasesOf(rows []sourceJSON) string {
	if len(rows) == 0 {
		return "-"
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Alias)
	}
	return strings.Join(names, ", ")
}

// targetCmd groups the target verbs. ls is the only one; the group is a noun
// so that what acts on one target later has a place to go.
func targetCmd(args []string) error {
	if len(args) == 0 {
		return usagef("target needs a subcommand: ls")
	}
	switch args[0] {
	case "--help", "-h", "help":
		fmt.Print(targetUsage)
		return nil
	case "ls":
	default:
		return usagef("unknown target subcommand %q (ls)", args[0])
	}
	line, err := parseBrigfileLine("brig target ls", args[1:], false, true)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(targetUsage)
		return nil
	}
	if err != nil {
		return err
	}
	l, err := loadBrigfile(line.file)
	if err != nil {
		return err
	}
	rows, err := targetRows(l)
	if err != nil {
		return err
	}
	if line.json {
		return writeJSONDocument(os.Stdout, "TargetList", rows)
	}
	printTargets(os.Stdout, l, rows)
	return nil
}

func printTargets(w io.Writer, l *brigfile.Loaded, rows []targetRowJSON) {
	name, alias, source := len("TARGET"), len("PROFILE"), len("SOURCE")
	for _, r := range rows {
		name = max(name, len(r.Name))
		alias = max(alias, len(r.Profile.Alias))
		source = max(source, len(shown(r.Profile.Source)))
	}
	fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %s\n", name, "TARGET", alias, "PROFILE", source, "SOURCE", "POLICIES")
	for _, r := range rows {
		mark := " "
		if r.Default {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %-*s  %-*s  %-*s  %s\n", mark, name, r.Name, alias, r.Profile.Alias,
			source, shown(r.Profile.Source), aliasesOf(r.Policies))
	}
	fmt.Fprintln(w)
	if l.Project.DefaultTarget != "" {
		fmt.Fprintln(w, "* the default target")
	}
	fmt.Fprintf(w, "read from %s\n", l.Path)
}

// configCmd prints what the Brigfile declares, or one target resolved.
func configCmd(args []string) error {
	line, err := parseBrigfileLine("brig config", args, true, true)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(configUsage)
		return nil
	}
	if err != nil {
		return err
	}
	l, err := loadBrigfile(line.file)
	if err != nil {
		return err
	}
	rows, err := targetRows(l)
	if err != nil {
		return err
	}
	data := configJSON{
		Name:          l.Project.Name,
		Path:          l.Path,
		Root:          l.Root,
		DefaultTarget: l.Project.DefaultTarget,
		Profiles:      []sourceJSON{},
		Policies:      []sourceJSON{},
		Targets:       rows,
	}
	for _, alias := range sortedAliases(l.Profiles) {
		data.Profiles = append(data.Profiles, profileSource(l.Profiles[alias]))
	}
	for _, alias := range sortedAliases(l.Policies) {
		data.Policies = append(data.Policies, sourceJSON{Alias: alias, Source: l.Policies[alias].Source.String()})
	}
	if line.target != "" {
		rt, err := resolveTarget(l, line.target)
		if err != nil {
			return err
		}
		data.Target = resolvedTarget(l, rt)
	}
	if line.json {
		return writeJSONDocument(os.Stdout, "Config", data)
	}
	if data.Target != nil {
		printResolvedTarget(os.Stdout, l, data.Target)
		return nil
	}
	printProject(os.Stdout, data)
	return nil
}

func sortedAliases[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolvedTarget applies a target's overrides to its profile's size and
// image, and records which of the two each value came from. It applies
// nothing else: BRIG_* settings and run-line flags are a run's to apply.
func resolvedTarget(l *brigfile.Loaded, rt brigfile.ResolvedTarget) *resolvedTargetJSON {
	p := rt.Profile.Profile
	out := &resolvedTargetJSON{
		Name:     rt.Name,
		Default:  rt.Name == l.Project.DefaultTarget,
		Profile:  profileSource(rt.Profile),
		CPUs:     intSettingJSON{Value: p.CPUs, From: "profile"},
		Mem:      intSettingJSON{Value: p.Mem, From: "profile"},
		Image:    stringSettingJSON{Value: p.Image, From: "profile"},
		Policies: []sourceJSON{},
		Args:     []string{},
	}
	if o := rt.Overrides; o.CPUs != nil {
		out.CPUs = intSettingJSON{Value: *o.CPUs, From: "target"}
	}
	if o := rt.Overrides; o.Mem != nil {
		out.Mem = intSettingJSON{Value: *o.Mem, From: "target"}
	}
	if o := rt.Overrides; o.Image != nil {
		out.Image = stringSettingJSON{Value: *o.Image, From: "target"}
	}
	for _, pol := range rt.Policies {
		out.Policies = append(out.Policies, sourceJSON{Alias: pol.Alias, Source: pol.Source.String()})
	}
	out.Args = append(out.Args, rt.Args...)
	return out
}

func printProject(w io.Writer, d configJSON) {
	const label = 16
	if d.Name != "" {
		fmt.Fprintf(w, "%-*s%s\n", label, "project", d.Name)
	}
	fmt.Fprintf(w, "%-*s%s\n", label, "brigfile", d.Path)
	fmt.Fprintf(w, "%-*s%s\n", label, "root", d.Root)
	if d.DefaultTarget != "" {
		fmt.Fprintf(w, "%-*s%s\n", label, "default target", d.DefaultTarget)
	}
	section := func(title string, rows []sourceJSON) {
		fmt.Fprintf(w, "\n%s\n", title)
		if len(rows) == 0 {
			fmt.Fprintln(w, "  none")
			return
		}
		width := 0
		for _, r := range rows {
			width = max(width, len(r.Alias))
		}
		for _, r := range rows {
			fmt.Fprintf(w, "  %-*s  %s\n", width, r.Alias, shown(r.Source))
		}
	}
	section("profiles", d.Profiles)
	section("policies", d.Policies)
	fmt.Fprintf(w, "\ntargets\n")
	width := 0
	for _, r := range d.Targets {
		width = max(width, len(r.Name))
	}
	for _, r := range d.Targets {
		s := fmt.Sprintf("  %-*s  profile %s", width, r.Name, r.Profile.Alias)
		if len(r.Policies) > 0 {
			s += ", policies " + aliasesOf(r.Policies)
		}
		if r.Default {
			s += "  (default)"
		}
		fmt.Fprintln(w, s)
	}
	for _, p := range d.Profiles {
		printIgnoredNote(w, p)
	}
}

func printResolvedTarget(w io.Writer, l *brigfile.Loaded, t *resolvedTargetJSON) {
	const label = 10
	head := t.Name
	if t.Default {
		head += " (default)"
	}
	fmt.Fprintf(w, "%-*s%s\n", label, "target", head)
	fmt.Fprintf(w, "%-*s%s\n", label, "brigfile", l.Path)
	fmt.Fprintf(w, "%-*s%s  %s\n", label, "profile", t.Profile.Alias, shown(t.Profile.Source))

	from := func(origin string) string {
		if origin == "target" {
			return "from target " + t.Name
		}
		return "from profile " + t.Profile.Alias
	}
	values := [][3]string{
		{"cpus", strconv.Itoa(t.CPUs.Value), from(t.CPUs.From)},
		{"mem", strconv.Itoa(t.Mem.Value), from(t.Mem.From)},
		{"image", shown(t.Image.Value), from(t.Image.From)},
	}
	width := 0
	for _, v := range values {
		width = max(width, len(v[1]))
	}
	for _, v := range values {
		fmt.Fprintf(w, "%-*s%-*s  %s\n", label, v[0], width, v[1], v[2])
	}

	if len(t.Policies) == 0 {
		fmt.Fprintf(w, "%-*snone\n", label, "policies")
	}
	pw := 0
	for _, p := range t.Policies {
		pw = max(pw, len(p.Alias))
	}
	for i, p := range t.Policies {
		name := ""
		if i == 0 {
			name = "policies"
		}
		fmt.Fprintf(w, "%-*s%-*s  %s\n", label, name, pw, p.Alias, shown(p.Source))
	}

	// Each argument quoted, so where one ends and the next begins is plain,
	// and a control character in one is escaped.
	args := "none"
	if len(t.Args) > 0 {
		quoted := make([]string, 0, len(t.Args))
		for _, a := range t.Args {
			quoted = append(quoted, strconv.Quote(a))
		}
		args = strings.Join(quoted, " ")
	}
	fmt.Fprintf(w, "%-*s%s\n", label, "args", args)
	printIgnoredNote(w, t.Profile)
}

func printIgnoredNote(w io.Writer, p sourceJSON) {
	if p.IgnoredUserFile == "" {
		return
	}
	name := strings.TrimPrefix(p.Source, "builtin:")
	fmt.Fprintf(w, "\nnote: %s overrides the built-in %s for `brig run %s`.\n"+
		"      This Brigfile says %s, so brig ignores that file for profile %s.\n",
		p.IgnoredUserFile, name, name, p.Source, p.Alias)
}

// starterSchemaURL is where an editor finds the Brigfile schema. It resolves
// once the schema is on the default branch.
const starterSchemaURL = "https://raw.githubusercontent.com/brig-sh/brig/main/api/project/v1alpha1/brigfile.schema.json"

// starterBrigfile is the Brigfile init writes. It must itself load: a starter
// that brig refuses is the worst first impression the command could make, and
// a test holds it to that.
//
// The name is the project directory's. It is written as a JSON string, which
// YAML reads as a double-quoted scalar, so that a directory called 2026, yes
// or a: b is still read as that string. A name with a control character is
// one the loader refuses, so it is left out.
func starterBrigfile(name string) []byte {
	var b strings.Builder
	// The modeline is the first line because yaml-language-server reads it
	// from there, and the file is named Brigfile, which no editor maps to
	// YAML or to this schema by itself.
	b.WriteString("# yaml-language-server: $schema=" + starterSchemaURL + "\n")
	b.WriteString("# How this repository runs under brig. Each field is described at\n" +
		"# https://github.com/brig-sh/brig/blob/main/docs/brigfile.md\n")
	b.WriteString("apiVersion: " + brigfile.APIVersion + "\n")
	if name != "" && strings.IndexFunc(name, unicode.IsControl) < 0 {
		quoted, err := json.Marshal(name)
		if err == nil {
			b.WriteString("name: " + string(quoted) + "\n")
		}
	}
	b.WriteString(`defaultTarget: dev
profiles:
  claude:
    from: builtin:claude-code
targets:
  dev:
    profile: claude
    # overrides:
    #   cpus: 4
    #   mem: 8192
`)
	return []byte(b.String())
}

// initCmd writes a starter Brigfile.
//
// The file is created with O_EXCL, through a root opened on its directory.
// O_EXCL fails on any entry already at the path, a link included, and does
// not follow a link to create the file it points at. A check for the file
// followed by a create would follow a dangling link planted in a checkout,
// and write wherever it points.
func initCmd(args []string) error {
	line, err := parseBrigfileLine("brig init", args, false, false)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(initUsage)
		return nil
	}
	if err != nil {
		return err
	}
	var dir, base string
	if line.file != "" {
		abs, err := filepath.Abs(line.file)
		if err != nil {
			return err
		}
		dir, base = filepath.Dir(abs), filepath.Base(abs)
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if dir, err = brigfile.Root(wd); err != nil {
			return err
		}
		base = brigfile.FileName
	}
	path := filepath.Join(dir, base)
	blob := starterBrigfile(filepath.Base(dir))
	// The starter is checked before anything is created, so a starter brig
	// would refuse is never left behind.
	if _, err := brigfile.Parse(blob); err != nil {
		return fmt.Errorf("the starter Brigfile does not parse: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(base, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		what := "a file"
		if fi, lerr := root.Lstat(base); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			what = "a symlink"
		}
		return fmt.Errorf("%s is already there (%s); brig init does not replace it or write through it", path, what)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dashF := ""
	if line.file != "" {
		dashF = " -f " + shellWord(line.file)
	}
	b := notice.New("wrote "+path).
		Do("to list its targets", "brig target ls"+dashF).
		Do("to see what dev resolves to", "brig config --target dev"+dashF)
	fmt.Println(b.String())
	return nil
}
