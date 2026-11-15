// Package memkey holds the rules for the names of agent memory stores: the
// alphabet, the project- prefix, the words a switch is written with, and the
// key of a project's store. It is separate from internal/memory so profile
// and Brigfile loading can read a memory setting without linking the MCP
// handler.
package memkey

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalidName is returned for a name or key that breaks the rules here.
var ErrInvalidName = errors.New("invalid memory name")

// MaxName is the longest memory or store name.
const MaxName = 64

// ProjectPrefix starts the key of every project store.
const ProjectPrefix = "project-"

// Project is the setting that asks for the store of the project folder.
const Project = "project"

// ValidChars reports whether name is made of the alphabet a memory or store
// name may use, and is not empty, too long, a dot name or hidden. The
// alphabet has no separator, so a name that passes is one path component.
func ValidChars(name string) error {
	if name == "" || len(name) > MaxName {
		return fmt.Errorf("%w: length must be 1 to %d", ErrInvalidName, MaxName)
	}
	if name == "." || name == ".." || name[0] == '.' {
		return fmt.Errorf("%w: %q may not start with a dot", ErrInvalidName, name)
	}
	for _, c := range []byte(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: %q may use only a-z, 0-9, '.', '_' and '-'", ErrInvalidName, name)
		}
	}
	return nil
}

// ValidStoreName checks the name of a store a caller picks. The project-
// prefix is ProjectKey's alone, so a named store can never land on a
// project's directory. The words a switch is written with are refused too,
// so memory off can never be read as a store called off.
func ValidStoreName(name string) error {
	if err := ValidChars(name); err != nil {
		return err
	}
	if strings.HasPrefix(name, ProjectPrefix) {
		return fmt.Errorf("%w: %q uses the project- prefix, which is reserved", ErrInvalidName, name)
	}
	if _, ok := switchWords[strings.ToLower(name)]; ok {
		return fmt.Errorf("%w: %q is reserved: it reads as on or off", ErrInvalidName, name)
	}
	return nil
}

// switchWords are the spellings of a switch, and what each means for
// memory: off, or on, which is the project's store. YAML 1.1 reads off, no
// and false in a profile as "false", and on, yes and true as "true", so
// both the raw and the parsed forms are here.
var switchWords = map[string]string{
	"false": "", "no": "", "off": "", "0": "", "none": "",
	"true": Project, "yes": Project, "on": Project, "1": Project,
}

// ParseSetting reads a memory setting, from any source: "" for off,
// [Project] for the project's store, or a store name. A switch word is read
// in any case. Anything else is refused, and the caller names its source.
func ParseSetting(v string) (string, error) {
	if m, ok := switchWords[strings.ToLower(v)]; ok {
		return m, nil
	}
	if v == Project {
		return Project, nil
	}
	if err := ValidStoreName(v); err != nil {
		return "", err
	}
	return v, nil
}

// ProjectKey is the store key for the project at absPath. The path is
// cleaned first, so /a/b/ and /a/./b share one store.
func ProjectKey(absPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(absPath)))
	return ProjectPrefix + hex.EncodeToString(sum[:8])
}
