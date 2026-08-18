package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// On macOS hull is what ships the kernel and initrd, so they live in its
// directory rather than in one of brig's.
func defaultBootAssetsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to find the boot artifacts in: %w", err)
	}
	return filepath.Join(home, ".hull", "assets"), nil
}
