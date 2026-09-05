package policy

import (
	"os"
	"path/filepath"
)

// Dir is where a user's own policies live: $XDG_CONFIG_HOME/brig/policies,
// or $HOME/.config/brig/policies when $XDG_CONFIG_HOME is unset, empty, or
// relative, per the XDG Base Directory Specification.
//
// BRIG_POLICY_DIR overrides it, taken as given: an explicit override is a
// deliberate act and is not second-guessed for absoluteness the way
// $XDG_CONFIG_HOME is.
func Dir() string {
	if dir := os.Getenv("BRIG_POLICY_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(configHome(), "brig", "policies")
}

// configHome resolves $XDG_CONFIG_HOME per the XDG Base Directory
// Specification, version 0.8: https://specifications.freedesktop.org/basedir/latest/
func configHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Returned deliberately, even though Dir treats a relative override
		// as invalid: there is no home directory to anchor to, and failing
		// outright would make brig unusable rather than merely misdirected.
		// os.UserHomeDir failing means $HOME is unset, which is close to
		// impossible on the platforms brig supports.
		return ".config"
	}
	return filepath.Join(home, ".config")
}
