package runtime

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// The tag names the guest platform, and the published bundles are tagged that
// way. A mismatch here asks the registry for something that does not exist.
func TestBootAssetsRefNamesThePlatform(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS_REF", "")
	want := bootAssetsRepo + ":" + goruntime.GOOS + "-" + goruntime.GOARCH
	if got := bootAssetsRef(); got != want {
		t.Fatalf("bootAssetsRef() = %q, want %q", got, want)
	}
}

func TestBootAssetsRefHonoursOverride(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS_REF", "ghcr.io/example/assets:v9-linux-amd64")
	if got := bootAssetsRef(); got != "ghcr.io/example/assets:v9-linux-amd64" {
		t.Fatalf("bootAssetsRef() = %q, want the override", got)
	}
}

// oras missing is the common case on a fresh Linux box, and it must produce
// something actionable rather than an exec error.
func TestOrasFetchWithoutOrasExplainsItself(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	err := orasFetch(t.TempDir(), nil, nil)
	if err == nil {
		t.Fatal("expected an error when oras is not installed")
	}
	for _, want := range []string{"oras", "oras.land", "BRIG_BOOT_ASSETS"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %q: %v", want, err)
		}
	}
	// The reference belongs in the message: it is what someone fetching by
	// hand has to ask for.
	if !strings.Contains(err.Error(), bootAssetsRepo) {
		t.Fatalf("error does not name the bundle to fetch: %v", err)
	}
}

// The directory brig resolved is the one that must be created and written to,
// not one the fetcher derives for itself.
func TestOrasFetchCreatesTheDirectoryItWasGiven(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "bin", "oras")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return stub, nil }

	dir := filepath.Join(t.TempDir(), "not", "yet", "there")
	if err := orasFetch(dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("fetcher did not create %s: %v", dir, err)
	}
}

// A failing oras must surface as a fetch failure, with the reference in it.
func TestOrasFetchReportsFailure(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "bin", "oras")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return stub, nil }

	err := orasFetch(t.TempDir(), nil, nil)
	if err == nil {
		t.Fatal("expected a failing oras to be reported")
	}
	if !strings.Contains(err.Error(), "oras pull") {
		t.Fatalf("error does not say what was attempted: %v", err)
	}
}
