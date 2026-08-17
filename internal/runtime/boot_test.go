package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A clean machine is the case that matters: nothing on disk, so the fetcher
// runs, and what it wrote is what the annotations carry.
func TestBootArtifactsFetchesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "share"))

	calls := 0
	fetch := func(assetDir string) error {
		calls++
		if err := os.MkdirAll(assetDir, 0o755); err != nil {
			return err
		}
		for _, name := range []string{bootKernelName(), bootInitrdName} {
			if err := os.WriteFile(filepath.Join(assetDir, name), []byte("x"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}

	kernel, initrd, err := bootArtifacts(nil, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fetcher ran %d times, want exactly 1", calls)
	}
	for _, p := range []string{kernel, initrd} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was not usable after the fetch: %v", p, err)
		}
	}
}

// The assets being present is the common case and downloading tens of
// megabytes over them would be a real cost, so the fetcher must not run.
func TestBootArtifactsDoesNotFetchWhenPresent(t *testing.T) {
	stageBootAssets(t)
	// stageBootAssets sets BRIG_BOOT_ASSETS; clear it so this exercises the
	// present-and-no-override path rather than the override path below.
	dir := os.Getenv("BRIG_BOOT_ASSETS")
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", filepath.Dir(dir))
	t.Setenv("XDG_DATA_HOME", dir)
	// Put the files where the default resolver will look.
	assetDir, err := defaultBootAssetsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fetch := func(string) error {
		t.Fatal("fetcher ran even though the assets were already present")
		return nil
	}
	if _, _, err := bootArtifacts(nil, fetch); err != nil {
		t.Fatal(err)
	}
}

// An explicit BRIG_BOOT_ASSETS points at a build someone is iterating on.
// Downloading a release bundle into it would replace their work.
func TestBootArtifactsNeverFetchesOverAnExplicitDir(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())
	fetch := func(string) error {
		t.Fatal("fetcher ran despite an explicit BRIG_BOOT_ASSETS")
		return nil
	}
	_, _, err := bootArtifacts(nil, fetch)
	if err == nil {
		t.Fatal("expected an error for an empty explicit asset directory")
	}
}

// A zero-length file satisfies a bare existence check and then fails at boot,
// which is much harder to diagnose. Treat it as missing.
func TestBootArtifactsTreatsEmptyFilesAsMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "share"))
	assetDir, err := defaultBootAssetsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(assetDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	called := false
	fetch := func(string) error { called = true; return nil }
	if _, _, err := bootArtifacts(nil, fetch); err == nil {
		t.Fatal("expected zero-length assets to be rejected")
	}
	if !called {
		t.Fatal("fetcher did not run for zero-length assets")
	}
}

// A failed download must say so, rather than surfacing as the generic "point
// BRIG_BOOT_ASSETS somewhere" message that sends the user down the wrong path.
func TestBootArtifactsReportsFetchFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "share"))

	_, _, err := bootArtifacts(nil, func(string) error { return errors.New("no credentials") })
	if err == nil {
		t.Fatal("expected the fetch failure to surface")
	}
	if !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("error lost the cause: %v", err)
	}
}

// With no runtime able to fetch -- the Linux path, where hull does not exist --
// the error must say that, and must not send the user after a macOS-only
// command that would not be there to run.
func TestBootArtifactsWithoutFetcherSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "share"))

	_, _, err := bootArtifacts(nil, nil)
	if err == nil {
		t.Fatal("expected missing assets to be an error")
	}
	if !strings.Contains(err.Error(), "cannot fetch") {
		t.Fatalf("error does not explain that nothing can fetch: %v", err)
	}
	if strings.Contains(err.Error(), "hull assets pull") {
		t.Fatalf("error points at a command this platform may not have: %v", err)
	}
}

// An explicit BRIG_BOOT_ASSETS gets its own message, naming the directory the
// user chose rather than the one brig would have picked.
func TestBootArtifactsExplicitDirNamesThatDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", dir)
	_, _, err := bootArtifacts(nil, nil)
	if err == nil {
		t.Fatal("expected an error for an empty explicit asset directory")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error does not name the chosen directory: %v", err)
	}
}

// The runtime knows where its assets are; brig does not. Asking it is the
// whole point of the locator, so its answer has to win over the compiled-in
// per-platform default.
func TestBootArtifactsPrefersTheRuntimesAnswer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	runtimeDir := t.TempDir()
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(runtimeDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	kernel, _, err := bootArtifacts(func() (string, error) { return runtimeDir, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(kernel) != runtimeDir {
		t.Fatalf("resolved %s, want it under the runtime's directory %s", kernel, runtimeDir)
	}
}

// An explicit BRIG_BOOT_ASSETS is the user overriding both of us, so it beats
// the runtime too.
func TestBootArtifactsExplicitBeatsTheRuntime(t *testing.T) {
	explicit := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", explicit)
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(explicit, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kernel, _, err := bootArtifacts(func() (string, error) {
		t.Fatal("runtime was asked despite an explicit BRIG_BOOT_ASSETS")
		return "", nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(kernel) != explicit {
		t.Fatalf("resolved %s, want it under %s", kernel, explicit)
	}
}

// An older hull has no `assets dir`. That must degrade to the historical path,
// not refuse to boot.
func TestBootArtifactsFallsBackWhenTheRuntimeCannotAnswer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	fallback, err := defaultBootAssetsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(fallback, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	kernel, _, err := bootArtifacts(
		func() (string, error) { return "", errors.New("unknown command \"dir\"") }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(kernel) != fallback {
		t.Fatalf("resolved %s, want the default %s", kernel, fallback)
	}
}
