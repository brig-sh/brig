package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
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

// versionWord is a release version, with or without its leading v: three
// dotted numbers and whatever suffix the tag or the toolchain gave it.
var versionWord = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+\S*)$`)

// VersionToken returns the version out of a runtime's `--version` line,
// without its leading v, or "" when the line carries none.
//
// The version is the first word shaped like one, not a fixed position. hull
// up to 0.1.0-rc28 printed `hull version 0.1.0-rc28`, with the version last.
// From the release that reports its build it prints `hull v0.1.0-rc29
// (4f5b4cb, 2026-09-24, go1.26.5, darwin/arm64)`, where the last word is the
// platform. nerdctl prints `nerdctl version 2.3.5`. A build from source can
// print `hull dev (...)`, which has no version word, and whose date must not
// be read as one.
func VersionToken(out string) string {
	for _, f := range strings.Fields(out) {
		if m := versionWord.FindStringSubmatch(f); m != nil {
			return m[1]
		}
	}
	return ""
}

// BootAssetsDir reports the directory the boot bundle for this host lives in
// and whether the kernel and initrd are already there, so brig doctor can say
// "present" or name the directory that is empty. It resolves the directory
// through bootAssetsDir, the same function the boot path uses, with the same
// question put to rt: a hull is asked with `hull assets dir`, and a runtime
// with no opinion gets the per-platform default. So doctor checks the directory
// a run would boot from, not a guess at it (#314).
//
// It reports presence rather than fetching: doctor names what is missing and
// how to get it, and downloading a bundle is a side effect a diagnostic has no
// business having.
//
// explicit reports that BRIG_BOOT_ASSETS chose the directory, which a run
// never downloads into, so doctor's advice for an empty one differs.
func BootAssetsDir(rt Runtime) (dir string, present, explicit bool, err error) {
	dir, chosen, err := bootAssetsDir(assetLocatorFor(rt))
	if err != nil {
		return "", false, false, err
	}
	kernel := filepath.Join(dir, bootKernelName())
	initrd := filepath.Join(dir, bootInitrdName)
	return dir, bootArtifactsPresent(kernel, initrd), chosen != "", nil
}

// BootAssetNames are the two files a boot bundle must hold, for a message
// that tells someone what to put in a directory.
func BootAssetNames() (kernel, initrd string) { return bootKernelName(), bootInitrdName }

// doctorAssetDirTimeout bounds doctor's `hull assets dir`. Its own value, not
// the agent-call timeout, so the two can change apart. A var for tests.
var doctorAssetDirTimeout = 5 * time.Second

// assetLocatorFor is the question a run with rt would ask, with doctor's
// deadline on it: hull's `assets dir` for a hull, and none for anything else,
// the same as nerdctl's run path passes.
func assetLocatorFor(rt Runtime) assetLocator {
	if h, ok := rt.(*hull); ok {
		return func() (string, error) { return h.assetDirWithin(doctorAssetDirTimeout) }
	}
	return nil
}
