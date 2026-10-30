package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// nodeWidePrefix is where the bundle's install.sh unpacks as root. A var for
// tests.
var nodeWidePrefix = "/var/lib/brig/data"

// BundleEnv is the environment of a runtime bundle's tree, and where the
// launchers that load it are.
type BundleEnv struct {
	// File is the tree's etc/brig-env.sh.
	File string
	// Launchers is the directory the bundle's installer writes them to.
	Launchers string
	// Program is the binary that was run: brig, or brigd, which has a launcher
	// too and reaches the same warning.
	Program string
}

// Missing says what MissingBundleEnv found. doctor and a run's warning both open
// with it.
func (e BundleEnv) Missing() string {
	return fmt.Sprintf("this %s is inside the runtime bundle, and its launcher did not run", e.Program)
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
// /opt/cni/bin, and keeps networks in a configuration of its own, which collide
// with the launcher's on the bridge device. BRIG_BOOT_ASSETS is unset, so brig
// boots whatever boot bundle it has fetched. Up to 0.1.10 that init cannot mount
// the virtiofs root urunc hands it, and the guest fails with "exec sleep failed".
// None of these errors names the launcher. The bundle's own bin directory is an
// easy thing to put on PATH, because its installer names brig-ctl there.
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
	if !InBundleTree(exe) {
		return BundleEnv{}, false
	}
	prefix := filepath.Dir(filepath.Dir(exe))
	return BundleEnv{File: filepath.Join(prefix, "etc", "brig-env.sh"), Launchers: bundleLaunchers(prefix), Program: filepath.Base(exe)}, true
}

// InBundleTree says whether a binary sits in the bin directory of a runtime
// bundle's tree.
func InBundleTree(bin string) bool {
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(bin)), "etc", "brig-env.sh"))
	return err == nil && info.Mode().IsRegular()
}

// bundleLaunchers is where the bundle's install.sh puts them: /usr/local/bin for
// the one tree it unpacks as root, and the home's .local/bin for any other. A
// user's tree can be anywhere, since it follows XDG_DATA_HOME, and the launchers
// do not follow it.
func bundleLaunchers(prefix string) string {
	nodeWide := nodeWidePrefix
	if resolved, err := filepath.EvalSymlinks(nodeWide); err == nil {
		nodeWide = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil || prefix == nodeWide {
		return "/usr/local/bin"
	}
	return filepath.Join(home, ".local", "bin")
}
