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
