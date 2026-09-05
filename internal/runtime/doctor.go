package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// versionTimeout bounds the one question Version asks the binary, the same 5s
// PinsDigest gives `--version`: the binary is on the host and normally answers
// at once, so a bound this generous only ever fires on one that has wedged.
const versionTimeout = 5 * time.Second

// Version runs `<bin> --version` and returns what it prints, trimmed.
//
// A package-level helper rather than a Runtime method on purpose. The Runtime
// interface is being changed by another issue in parallel, and brig doctor is
// the only caller that wants the version string, so a free function keeps the
// two from colliding -- and PinsDigest already shells the same command for a
// bool, so nothing new is learned about the binary that was not learned before.
func Version(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = mergeEnv(telemetryEnv(false))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// BootAssetsDir reports the directory the boot bundle for this host lives in
// and whether the kernel and initrd are already there, so brig doctor can say
// "present" or name the directory that is empty. It honours BRIG_BOOT_ASSETS,
// the same override the boot path reads, so the two agree about where to look.
//
// It reports presence rather than fetching: doctor names what is missing and
// how to get it, and downloading a bundle is a side effect a diagnostic has no
// business having.
func BootAssetsDir() (dir string, present bool, err error) {
	dir = os.Getenv("BRIG_BOOT_ASSETS")
	if dir == "" {
		dir, err = defaultBootAssetsDir()
		if err != nil {
			return "", false, err
		}
	}
	kernel := filepath.Join(dir, bootKernelName())
	initrd := filepath.Join(dir, bootInitrdName)
	return dir, bootArtifactsPresent(kernel, initrd), nil
}
