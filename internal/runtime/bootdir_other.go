//go:build !darwin

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// Everywhere else urunc is the runtime and nothing installs these yet, so this
// is a location rather than a promise: put them here, or point
// BRIG_BOOT_ASSETS somewhere else. It follows the XDG Base Directory
// Specification's data directory, the same reasoning as the profile directory.
func defaultBootAssetsDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "brig", "assets"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to find the boot artifacts in: %w", err)
	}
	return filepath.Join(home, ".local", "share", "brig", "assets"), nil
}
