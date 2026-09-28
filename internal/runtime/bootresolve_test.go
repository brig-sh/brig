package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An adapter handed the paths boots them. It does not ask hull where the
// assets live and does not fetch, so the resolve wrap made before the verify
// summary is the only one (#234).
func TestRunArgsGivenBootAssetsNeitherLocatesNorFetches(t *testing.T) {
	// Unset, so a run that resolves for itself asks locate, finds an empty
	// directory and fetches into it.
	t.Setenv("BRIG_BOOT_ASSETS", "")
	empty := t.TempDir()
	locate := func() (string, error) {
		t.Error("asked the runtime where the boot assets live")
		return empty, nil
	}
	fetch := func(string) error {
		t.Error("fetched the boot assets")
		return errors.New("test: no fetch")
	}
	given := BootAssets{Kernel: "/given/" + bootKernelName(), Initrd: "/given/" + bootInitrdName}

	args, _, err := runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true, BootAssets: given},
		"vz", "shared", "", "", locate, fetch)
	if err != nil {
		t.Fatalf("given boot assets were refused: %v", err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--annotation "+annotationBootKernel+"="+given.Kernel) {
		t.Errorf("the given kernel is not the one that boots: %s", got)
	}
	if !strings.Contains(got, "--annotation "+annotationBootInitrd+"="+given.Initrd) {
		t.Errorf("the given initrd is not the one that boots: %s", got)
	}
}

// One path alone is not a resolve. The adapter resolves both itself, so a
// kernel is never booted beside an initrd nobody found.
func TestRunArgsHalfGivenBootAssetsResolveAsBefore(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())
	half := BootAssets{Kernel: "/given/" + bootKernelName()}

	_, _, err := runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true, BootAssets: half},
		"vz", "shared", "", "", nil, nil)
	if err == nil {
		t.Fatal("a kernel with no initrd reached the command line")
	}
}

// The same on Linux. The default asset directory is empty, so a run that
// resolves for itself goes to oras, and the stub below fails the test.
func TestNerdctlGivenBootAssetsDoesNotFetch(t *testing.T) {
	emptyAssetHome(t)
	was := lookPath
	lookPath = func(string) (string, error) {
		t.Error("looked for oras to fetch the boot assets")
		return "", errors.New("test: no oras")
	}
	t.Cleanup(func() { lookPath = was })
	given := BootAssets{Kernel: "/given/" + bootKernelName(), Initrd: "/given/" + bootInitrdName}

	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true, BootAssets: given})
	if err != nil {
		t.Fatalf("given boot assets were refused: %v", err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--annotation "+annotationBootKernel+"="+given.Kernel) ||
		!strings.Contains(got, "--annotation "+annotationBootInitrd+"="+given.Initrd) {
		t.Errorf("the given paths are not the ones that boot: %s", got)
	}
}

// docker is refused before the resolve reaches oras. A docker user gets the
// refusal that names the cause, not a download or "oras is not installed".
func TestNerdctlResolveRefusesDockerBeforeFetching(t *testing.T) {
	emptyAssetHome(t)
	was := lookPath
	lookPath = func(string) (string, error) {
		t.Error("looked for oras on docker")
		return "", errors.New("test: no oras")
	}
	t.Cleanup(func() { lookPath = was })

	d := &nerdctl{bin: "/usr/bin/docker"}
	_, err := d.ResolveBootAssets(nil, nil)
	if err == nil {
		t.Fatal("resolved boot assets for docker, which cannot pass them on")
	}
	if !strings.Contains(err.Error(), "nerdctl") {
		t.Errorf("the refusal does not say what to use instead: %v", err)
	}
}

// Both resolvers return the paths bootArtifacts finds, the ones Run used to
// find for itself.
func TestResolveBootAssetsReturnsThePair(t *testing.T) {
	kernel, initrd := stageBootAssets(t)
	for _, rt := range []BootResolver{
		&hull{bin: "/nonexistent/hull"},
		&nerdctl{bin: "/usr/local/bin/nerdctl"},
	} {
		got, err := rt.ResolveBootAssets(nil, nil)
		if err != nil {
			t.Fatalf("%T: %v", rt, err)
		}
		if got.Kernel != kernel || got.Initrd != initrd {
			t.Errorf("%T resolved %+v, want %s and %s", rt, got, kernel, initrd)
		}
	}
}

// The resolve says whether BRIG_BOOT_ASSETS chose the directory. wrap refuses
// a differing digest in a directory brig chose, and only states one in a
// directory somebody named. A resolve that reports a named directory as
// brig's own turns a refusal into a warning (#234).
func TestResolveBootAssetsSaysWhoChoseTheDirectory(t *testing.T) {
	stageBootAssets(t)
	got, err := resolveBootAssets(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Named {
		t.Error("a directory BRIG_BOOT_ASSETS named reads as one brig chose")
	}

	emptyAssetHome(t)
	dir := t.TempDir()
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err = resolveBootAssets(func() (string, error) { return dir, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Named {
		t.Error("the directory the runtime reported reads as one BRIG_BOOT_ASSETS named")
	}
}

// emptyAssetHome leaves BRIG_BOOT_ASSETS unset and points the default asset
// directory at an empty one. An explicit BRIG_BOOT_ASSETS is refused before
// any fetch, so a test that sets it never reaches oras and a lookPath stub
// there proves nothing.
func emptyAssetHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
}
