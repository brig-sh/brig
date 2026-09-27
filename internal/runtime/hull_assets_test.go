package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envRecordingHull writes a stand-in hull that dumps its environment to a log
// and exits, and returns both paths.
func envRecordingHull(t *testing.T) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "hull")
	log = filepath.Join(dir, "env.log")
	script := "#!/bin/sh\nenv > '" + log + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// pulledEnv runs pullAssets against the stand-in and returns the environment
// hull saw, one entry per line.
func pulledEnv(t *testing.T, dir string) []string {
	t.Helper()
	bin, log := envRecordingHull(t)
	if err := (&hull{bin: bin}).pullAssets(dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

// brig verifies the bundle BRIG_BOOT_ASSETS_REF names. If hull is not told the
// same reference it fetches its own default, and the kernel that boots comes
// from a bundle brig never checked (#234).
func TestPullAssetsPassesTheVerifiedRefToHull(t *testing.T) {
	const ref = "ghcr.io/example/assets:v9-darwin-arm64"
	t.Setenv("BRIG_BOOT_ASSETS_REF", ref)
	t.Setenv("HULL_BOOT_ASSETS_REF", "ghcr.io/elsewhere/assets:x")

	env := pulledEnv(t, t.TempDir())
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "HULL_BOOT_ASSETS_REF=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "HULL_BOOT_ASSETS_REF="+ref {
		t.Fatalf("hull got %q, want exactly HULL_BOOT_ASSETS_REF=%s", got, ref)
	}
}

// With BRIG_BOOT_ASSETS_REF unset, brig verified its own default. A
// HULL_BOOT_ASSETS_REF left in brig's environment points hull at another
// bundle, so it must not reach hull. The directory pin must survive the
// stripping, or hull writes somewhere brig never looks.
func TestPullAssetsDropsAnInheritedHullRef(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS_REF", "")
	t.Setenv("HULL_BOOT_ASSETS_REF", "ghcr.io/elsewhere/assets:x")

	dir := t.TempDir()
	env := pulledEnv(t, dir)
	pinned := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "HULL_BOOT_ASSETS_REF=") {
			t.Errorf("hull was handed a reference brig did not verify: %s", kv)
		}
		if kv == "HULL_BOOT_ASSETS="+dir {
			pinned = true
		}
	}
	if !pinned {
		t.Errorf("hull lost the directory pin HULL_BOOT_ASSETS=%s:\n%s", dir, strings.Join(env, "\n"))
	}
}
