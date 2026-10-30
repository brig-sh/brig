package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BundleEnv is the environment of a runtime bundle's tree, and where the
// launchers that load it are.
type BundleEnv struct {
	// File is the tree's etc/brig-env.sh.
	File string
	// Launchers is the directory the bundle's installer writes them to.
	Launchers string
}

// Fix is what to type, in the words both the doctor row and the run's warning
// use.
func (e BundleEnv) Fix() string {
	return fmt.Sprintf("export PATH=\"%s:$PATH\"", e.Launchers)
}

// MissingBundleEnv says whether exe is the brig inside a runtime bundle's tree,
// run without the environment that tree's launcher sets.
//
// The binary works without it, far enough to fail somewhere else. nerdctl then
// has no configuration of the bundle's, so it looks for CNI plugins in
// /opt/cni/bin. BRIG_BOOT_ASSETS is unset, so brig fetches the boot bundle, whose
// init mounts a block root and cannot mount the virtiofs root the bundle's urunc
// hands it. The guest then fails with "exec sleep failed", which says nothing
// about any of this. The bundle's own bin directory is an easy thing to put on
// PATH, because its installer names brig-ctl there.
//
// The launcher exports both variables, so one that is set means the launcher
// ran or someone took charge of this by hand.
func MissingBundleEnv(exe string) (BundleEnv, bool) {
	if os.Getenv("BRIG_RUNTIME_BIN") != "" || os.Getenv("BRIG_BOOT_ASSETS") != "" {
		return BundleEnv{}, false
	}
	// A symlink on PATH reaches the same binary and skips the same launcher.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	prefix := filepath.Dir(filepath.Dir(exe))
	file := filepath.Join(prefix, "etc", "brig-env.sh")
	if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
		return BundleEnv{}, false
	}
	return BundleEnv{File: file, Launchers: bundleLaunchers(prefix)}, true
}

// bundleLaunchers is where the bundle's install.sh puts them: a user install is
// under the home, and a node-wide one is not.
func bundleLaunchers(prefix string) string {
	home, err := os.UserHomeDir()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(home); rerr == nil {
			home = resolved
		}
		if strings.HasPrefix(prefix, home+string(filepath.Separator)) {
			return filepath.Join(home, ".local", "bin")
		}
	}
	return "/usr/local/bin"
}
