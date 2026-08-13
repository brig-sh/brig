// Package config declares brig's global user configuration.
//
// What is here today is the schema and the value handling over it: the table of
// declared settings, the types they hold, and the expansion of ${VAR}
// expressions found in them. The loader over $XDG_CONFIG_HOME/brig/config.yaml
// and the `brig config` command are not written yet; this package is the layer
// they will both read, and it is deliberately usable and testable without them.
//
// The schema is a table, not a struct. A declared Key carries its own type, its
// default and its one-line documentation, which is what will let one generic set
// of CLI verbs serve every section: adding a section is adding rows. It also
// keeps the documentation and the validation from drifting apart, because they
// are the same declaration.
//
// Nothing here reads a file. A Key turns text into a typed value; where that
// text came from -- a YAML node or a command line -- is the caller's problem.
package config

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kind is the type a declared setting holds.
type Kind int

const (
	// invalid is the zero value, so a Key that forgot to declare its Kind is a
	// schema error rather than a silent Bool. The schema is hand-written data
	// and an omitted field is the easiest mistake to make in it; Schema.Validate
	// is what turns that mistake into a message.
	invalid Kind = iota
	// Bool is parsed strictly: true/false/1/0 and the other spellings
	// strconv.ParseBool accepts, nothing else.
	//
	// Deliberately unlike wrap.Env.Bool (internal/wrap/env.go:51), which reads
	// anything but "0" as true. That looseness is right for a shell variable
	// and wrong for a typed file: BRIG_SKILLS=maybe is a shrug, but
	// skills.import.auto: maybe is a mistake worth reporting.
	Bool
	Int
	String
	// Enum is a string constrained to Key.Enum.
	Enum
	// StringList is a list of strings. From a command line it is written
	// comma-separated; from YAML it arrives as a sequence.
	StringList
	// EnvVar holds the NAME of an environment variable, never its value. The
	// shape check is what stops a token being pasted where a name belongs, so
	// it is a type rather than a convention.
	EnvVar
)

// String names the kind the way an error message needs it: "expected bool".
func (k Kind) String() string {
	switch k {
	case invalid:
		return "no kind"
	case Bool:
		return "bool"
	case Int:
		return "int"
	case String:
		return "string"
	case Enum:
		return "enum"
	case StringList:
		return "list"
	case EnvVar:
		return "env var name"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Key is one declared setting.
type Key struct {
	// Path is the dotted address. A "*" segment is a name the user chooses:
	// mcp.servers.*.command is set as mcp.servers.github.command.
	Path string
	Kind Kind
	// Enum is the permitted set when Kind is Enum.
	Enum []string
	// Default applies when the key is ABSENT from the file. It is not the same
	// idea as ${VAR:-fallback}, which applies when the variable is unset; a key
	// can be present and still resolve through a fallback.
	Default any
	// Doc is one line, and is the only description of this setting anywhere.
	// Validate requires it, because a setting that cannot be explained in one
	// line is a setting nobody will be able to use. It is what a `config list`
	// verb and the generated reference will print once those exist.
	Doc string
}

// Schema is a set of declared keys.
//
// A value rather than a package global so tests can declare their own: the
// promise that a new section needs no CLI change is only worth something if a
// test can invent a section and drive it.
type Schema []Key

// Lookup returns the key declaring path. The returned Key keeps its Path as
// declared, wildcards intact, so a caller can tell mcp.servers.github.command
// from the mcp.servers.*.command row that permits it.
//
// An exact declaration wins over a wildcard that also admits the path, whatever
// order the two appear in. Without that rule the answer depends on slice
// position, so moving two rows apart would silently change a key's type.
// Ambiguity between two *wildcards* is a schema bug rather than something to
// resolve here; Validate rejects duplicate paths, and overlapping wildcards
// resolve first-declared.
func (s Schema) Lookup(path string) (Key, bool) {
	if path == "" {
		return Key{}, false
	}
	segs := strings.Split(path, ".")
	var wildcard Key
	var viaWildcard bool
	for _, k := range s {
		if !matchPath(strings.Split(k.Path, "."), segs) {
			continue
		}
		if k.Path == path {
			return k, true
		}
		if !viaWildcard {
			wildcard, viaWildcard = k, true
		}
	}
	return wildcard, viaWildcard
}

// Validate checks that a schema is usable before anything reads it.
//
// The schema is hand-written data, so its mistakes are the mistakes people make
// in data: a forgotten field, a duplicated row, a default that does not match
// the type beside it. Each one is silent at runtime and confusing at the point
// it surfaces, which is why they are caught here instead.
//
// Same role as agent.Template.Validate (internal/agent/custom.go:127), and for
// the same reason: data that configures behaviour should refuse to be wrong.
func (s Schema) Validate() error {
	seen := map[string]bool{}
	for _, k := range s {
		if k.Path == "" {
			return fmt.Errorf("a key has an empty path")
		}
		if slices.Contains(strings.Split(k.Path, "."), "") {
			return fmt.Errorf("%s: a path segment is empty", k.Path)
		}
		if seen[k.Path] {
			return fmt.Errorf("%s: declared twice", k.Path)
		}
		seen[k.Path] = true

		if k.Kind == invalid {
			return fmt.Errorf("%s: no kind declared", k.Path)
		}
		if k.Kind == Enum && len(k.Enum) == 0 {
			// Otherwise every value fails against an empty set, with an error
			// that trails off after "is not one of".
			return fmt.Errorf("%s: an enum key needs its enum values", k.Path)
		}
		if k.Kind != Enum && len(k.Enum) > 0 {
			return fmt.Errorf("%s: enum values on a %s key", k.Path, k.Kind)
		}
		if k.Doc == "" {
			// The only description of this setting anywhere, so an empty one is
			// a setting nobody can explain.
			return fmt.Errorf("%s: no doc line", k.Path)
		}
		if err := k.validateDefault(); err != nil {
			return fmt.Errorf("%s: %w", k.Path, err)
		}
	}
	return nil
}

// validateDefault checks Default against Kind. Default is `any`, so nothing
// else stops a string default on an Int key reaching a consumer that asserts an
// int and panicking there instead of here.
func (k Key) validateDefault() error {
	if k.Default == nil {
		return nil // no default is a valid state: the key is simply unset
	}
	var ok bool
	switch k.Kind {
	case Bool:
		_, ok = k.Default.(bool)
	case Int:
		_, ok = k.Default.(int)
	case String, EnvVar:
		_, ok = k.Default.(string)
	case Enum:
		var s string
		if s, ok = k.Default.(string); ok && !slices.Contains(k.Enum, s) {
			return fmt.Errorf("default %q is not one of %s", s, strings.Join(k.Enum, ", "))
		}
	case StringList:
		_, ok = k.Default.([]string)
	}
	if !ok {
		return fmt.Errorf("default is %T, want %s", k.Default, k.Kind)
	}
	return nil
}

// matchPath reports whether a declared path admits a concrete one. A "*"
// segment matches exactly one non-empty segment -- never zero, never several,
// which is what keeps features.a.b from passing as features.*.
func matchPath(declared, actual []string) bool {
	if len(declared) != len(actual) {
		return false
	}
	for i, d := range declared {
		if actual[i] == "" {
			return false
		}
		if d == "*" {
			continue
		}
		if d != actual[i] {
			return false
		}
	}
	return true
}

// Sections returns the declared top-level section names, sorted.
//
// A section exists because a key declares it. That is what makes security and
// introspection reserved for free: nothing declares them, so they are unknown
// sections, and the loader's warn-and-ignore path already covers them.
func (s Schema) Sections() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range s {
		name := k.Path
		if i := strings.IndexByte(name, '.'); i >= 0 {
			name = name[:i]
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Keys returns the keys declared under a section, sorted by path.
func (s Schema) Keys(section string) []Key {
	var out []Key
	for _, k := range s {
		if k.Path == section || strings.HasPrefix(k.Path, section+".") {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ParseValue converts raw text to the key's type.
//
// Callers that may be holding an env expression want Resolve instead, which
// expands first -- ParseValue on "${BRIG_MEM:-4096}" is a type error, and
// correctly so.
func (k Key) ParseValue(raw string) (any, error) {
	switch k.Kind {
	case Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a bool (try true or false)", raw)
		}
		return b, nil
	case Int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not an int", raw)
		}
		return n, nil
	case String:
		return raw, nil
	case Enum:
		if slices.Contains(k.Enum, raw) {
			return raw, nil
		}
		return nil, fmt.Errorf("%q is not one of %s", raw, strings.Join(k.Enum, ", "))
	case StringList:
		// A blank entry is dropped rather than reported: "a,,b" is a stray
		// comma, and a list with an empty member is never what was meant.
		out := []string{}
		for part := range strings.SplitSeq(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	case EnvVar:
		if !validVarName(raw) {
			// The value is described, never repeated. This is the error that
			// fires when somebody pastes a credential into a field that wants a
			// variable name, so echoing the input would print the credential
			// into a terminal, a CI log or a bug report. Same rule as
			// creds.go:102-106, which reports a reference's scheme and never
			// its value.
			return nil, fmt.Errorf("this field takes a variable NAME such as "+
				"ACME_TOKEN, not a value (got %s)", shapeOf(raw))
		}
		return raw, nil
	}
	return nil, fmt.Errorf("no value kind declared for %s", k.Path)
}

// shapeOf describes a rejected value without reproducing any of it: the length,
// and which rule it broke. Length alone cannot reconstruct a secret, and a
// character class is enough to spot a typo.
func shapeOf(s string) string {
	if s == "" {
		return "an empty value"
	}
	for _, r := range s {
		switch {
		case r == '_',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		default:
			return fmt.Sprintf("%d characters, including one that cannot appear "+
				"in a variable name", len(s))
		}
	}
	// Every character is legal, so validVarName can only have failed on the
	// leading digit.
	return fmt.Sprintf("%d characters, starting with a digit", len(s))
}

// Resolve expands env expressions in raw text and then type-checks the result.
//
// The order is the point. mem: ${BRIG_MEM:-4096} has to expand to "4096" before
// anything can call it an int, so expansion cannot be a String-only
// convenience -- it belongs before the type check for every kind.
//
// EnvVar is the exception: it holds a variable name, and expanding it would
// turn a declared credential reference into the credential. ParseValue then
// rejects an expression outright, because "$" and "{" cannot appear in a name.
// Note what that guard is worth: a token whose text happens to be letters,
// digits and underscores would otherwise expand, pass as a name, and be stored
// where a reference belongs.
//
// A writer wants ParseValue instead. A value being written may reference a
// variable that is unset in the shell doing the writing but set when the agent
// runs, so a writer should check the expression's syntax rather than resolve it.
func (k Key) Resolve(raw string, lookup func(string) (string, bool)) (any, error) {
	if k.Kind == EnvVar {
		v, err := k.ParseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k.Path, err)
		}
		return v, nil
	}
	expanded, err := Expand(raw, lookup)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.Path, err)
	}
	v, err := k.ParseValue(expanded)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.Path, err)
	}
	return v, nil
}

// validVarName reports whether s is shaped like an environment variable name.
func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
