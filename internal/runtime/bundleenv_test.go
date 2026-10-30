package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// bundleTree lays out the part of a runtime bundle's tree that the check reads,
// and returns the path of the brig inside it.
func bundleTree(t *testing.T, prefix string) string {
	t.Helper()
	for _, dir := range []string{"bin", "etc"} {
		if err := os.MkdirAll(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(prefix, "bin", "brig")
	if err := os.WriteFile(exe, []byte("stand-in\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "etc", "brig-env.sh"), []byte("export X=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return exe
}

func noBundleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BRIG_RUNTIME_BIN", "")
	t.Setenv("BRIG_BOOT_ASSETS", "")
}

// A brig run from the bundle's tree with nothing exported is the host that
// looked in /opt/cni/bin and booted a fetched initrd the bundle's urunc cannot
// use. The tree says so, and it says which launcher was skipped.
func TestMissingBundleEnvFindsTheBypassedLauncher(t *testing.T) {
	noBundleEnv(t)
	// Resolved, as the check resolves the executable: a temporary directory is
	// behind a symlink on macOS.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	prefix := filepath.Join(home, ".local", "share", "brig", "data")
	exe := bundleTree(t, prefix)

	env, missing := MissingBundleEnv(exe)
	if !missing {
		t.Fatal("a brig in the bundle's tree with no environment was not reported")
	}
	if want := filepath.Join(prefix, "etc", "brig-env.sh"); env.File != want {
		t.Errorf("File = %q, want %q", env.File, want)
	}
	if want := filepath.Join(home, ".local", "bin"); env.Launchers != want {
		t.Errorf("Launchers = %q, want %q", env.Launchers, want)
	}
}

// A node-wide tree is not under the home, and its launchers are the system's.
func TestMissingBundleEnvNodeWideLaunchers(t *testing.T) {
	noBundleEnv(t)
	t.Setenv("HOME", t.TempDir())
	env, missing := MissingBundleEnv(bundleTree(t, t.TempDir()))
	if !missing {
		t.Fatal("a node-wide tree with no environment was not reported")
	}
	if env.Launchers != "/usr/local/bin" {
		t.Errorf("Launchers = %q, want /usr/local/bin", env.Launchers)
	}
}

// A symlink into the tree is the same bypass: a link on PATH reaches the same
// binary, and the launcher is still skipped.
func TestMissingBundleEnvFollowsASymlink(t *testing.T) {
	noBundleEnv(t)
	exe := bundleTree(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "brig")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	if _, missing := MissingBundleEnv(link); !missing {
		t.Error("a symlink into the bundle's tree was not reported")
	}
}

// The launcher exports both, and someone who set either has taken charge of
// this by hand. Neither may be told to do what they already did.
func TestMissingBundleEnvQuietWhenSet(t *testing.T) {
	for _, name := range []string{"BRIG_RUNTIME_BIN", "BRIG_BOOT_ASSETS"} {
		t.Run(name, func(t *testing.T) {
			noBundleEnv(t)
			t.Setenv(name, "/somewhere")
			if _, missing := MissingBundleEnv(bundleTree(t, t.TempDir())); missing {
				t.Errorf("reported with %s set", name)
			}
		})
	}
}

// A release archive unpacked anywhere has no tree around it, and a directory at
// the file's path is not an environment to load. Neither is a bypass.
func TestMissingBundleEnvQuietOutsideABundle(t *testing.T) {
	noBundleEnv(t)
	loose := filepath.Join(t.TempDir(), "bin", "brig")
	if _, missing := MissingBundleEnv(loose); missing {
		t.Error("a brig with no tree around it was reported")
	}

	prefix := t.TempDir()
	exe := bundleTree(t, prefix)
	file := filepath.Join(prefix, "etc", "brig-env.sh")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, missing := MissingBundleEnv(exe); missing {
		t.Error("a directory at the environment file's path was reported")
	}
}
