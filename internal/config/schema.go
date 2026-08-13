// Package config holds brig's global user configuration: the schema that
// declares every setting, and the loader over $XDG_CONFIG_HOME/brig/config.yaml.
//
// The schema is a table, not a struct. A declared Key carries its own type, its
// default and its one-line documentation, which makes the `brig config` CLI
// generic over it: adding a section later is adding rows, never adding CLI code.
// It also means the documentation and the validation cannot drift apart,
// because they are the same declaration.
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
	// Bool is parsed strictly: true/false/1/0 and the other spellings
	// strconv.ParseBool accepts, nothing else.
	//
	// Deliberately unlike wrap.Env.Bool (internal/wrap/env.go:51), which reads
	// anything but "0" as true. That looseness is right for a shell variable
	// and wrong for a typed file: BRIG_SKILLS=maybe is a shrug, but
	// skills.import.auto: maybe is a mistake worth reporting.
	Bool Kind = iota
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
	// Doc is one line, and is the only description of this setting anywhere:
	// `brig config list` prints it and the shipped docs are generated from it.
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
func (s Schema) Lookup(path string) (Key, bool) {
	if path == "" {
		return Key{}, false
	}
	segs := strings.Split(path, ".")
	for _, k := range s {
		if matchPath(strings.Split(k.Path, "."), segs) {
			return k, true
		}
	}
	return Key{}, false
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
			return nil, fmt.Errorf("%q is not a variable name; this field takes a "+
				"name such as ACME_TOKEN, never the value", raw)
		}
		return raw, nil
	}
	return nil, fmt.Errorf("unknown kind %s for %s", k.Kind, k.Path)
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
//
// Writers want ParseValue instead. A value being written may reference a
// variable that is unset in the shell doing the writing but set when the agent
// runs, so `brig config set` checks the expression's syntax rather than
// resolving it.
func (k Key) Resolve(raw string, lookup func(string) (string, bool)) (any, error) {
	if k.Kind == EnvVar {
		return k.ParseValue(raw)
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
