package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// hostDir is where brig keeps state of its own on the host: ~/.brig/host, or
// host under BRIG_STATE_DIR.
func hostDir() (string, error) {
	if dir := os.Getenv("BRIG_STATE_DIR"); dir != "" {
		return filepath.Join(dir, "host"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep host state in: %w", err)
	}
	return filepath.Join(home, ".brig", "host"), nil
}

// HostDir is where brig keeps state of its own on the host: ~/.brig/host, or
// host under BRIG_STATE_DIR. The credential broker keeps its files in
// HostBrokerDir below it.
func HostDir() (string, error) { return hostDir() }

// CheckHostDirLinks refuses a host directory that is a link, or whose state
// directory above it is one. The broker keeps its keys below it, and a link
// could be swapped for another.
func CheckHostDirLinks(dir string) error {
	if info, err := os.Lstat(filepath.Dir(dir)); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a link, and brig keeps its "+
			"keys there. Point BRIG_STATE_DIR at the real directory", filepath.Dir(dir))
	}
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a link, and brig keeps its "+
			"keys there. Remove the link, and brig creates a directory there", dir)
	}
	return nil
}

// HostBrokerDir is the name of the broker's directory under HostDir.
const HostBrokerDir = "broker"
