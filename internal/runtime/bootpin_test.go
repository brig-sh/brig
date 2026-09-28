package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const pinnedRef = "ghcr.io/nofireai/hull-assets@sha256:3333333333333333333333333333333333333333333333333333333333333333"

// argsRecordingTool writes a stand-in that logs its arguments and environment
// and runs body, and returns the stand-in and the log.
func argsRecordingTool(t *testing.T, name, body string) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin, log = filepath.Join(dir, name), filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"args: $*\" > '" + log + "'\nenv >> '" + log + "'\n" + body + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hull is handed the bundle at the digest that verified, even where an
// override names the tag, so a tag that moved since the check cannot deliver
// another bundle. A replace asks hull to fetch over the files there.
func TestFetchAssetsPinsHullToTheVerifiedDigest(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS_REF", "ghcr.io/nofireai/hull-assets:v9-darwin-arm64")
	bin, log := argsRecordingTool(t, "hull", "")
	h := &hull{bin: bin}

	if err := h.fetchAssets(t.TempDir(), BootFetch{Ref: pinnedRef}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, log)
	if !strings.Contains(got, "\nHULL_BOOT_ASSETS_REF="+pinnedRef+"\n") {
		t.Errorf("hull was not given the pinned reference:\n%s", got)
	}
	if !strings.HasPrefix(got, "args: assets pull\n") {
		t.Errorf("a first fetch asked for more than a pull: %s", strings.SplitN(got, "\n", 2)[0])
	}

	if err := h.fetchAssets(t.TempDir(), BootFetch{Ref: pinnedRef, Replace: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, log); !strings.HasPrefix(got, "args: assets pull --force\n") {
		t.Errorf("a replace did not ask hull to fetch over the files: %s", strings.SplitN(got, "\n", 2)[0])
	}
}

// A first fetch says it happens once. A replace fetches the bundle that
// verified over an older one, and says that instead, on both runtimes.
func TestAReplaceIsNotAnnouncedAsAFirstFetch(t *testing.T) {
	bin, _ := argsRecordingTool(t, "hull", "")
	h := &hull{bin: bin}
	var first, again bytes.Buffer
	if err := h.fetchAssets(t.TempDir(), BootFetch{Ref: pinnedRef}, &first, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.fetchAssets(t.TempDir(), BootFetch{Ref: pinnedRef, Replace: true}, &again, nil); err != nil {
		t.Fatal(err)
	}

	oras, _ := argsRecordingTool(t, "oras", "")
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return oras, nil }
	var orasAgain bytes.Buffer
	_ = orasPull(t.TempDir(), BootFetch{Ref: pinnedRef, Replace: true}, &orasAgain, nil)

	if !strings.Contains(first.String(), "(once)") {
		t.Errorf("a first fetch does not say it happens once: %q", first.String())
	}
	for name, said := range map[string]string{"hull": again.String(), "oras": orasAgain.String()} {
		if strings.Contains(said, "(once)") || !strings.Contains(said, "older bundle") {
			t.Errorf("%s: a replace is announced as %q", name, said)
		}
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// oras pulls the bundle at the digest that verified, and brig records which
// bundle that was, in the shape hull records its own, so a later run can tell
// an older bundle from a changed file. A pull by tag leaves no record behind.
func TestOrasPullsTheVerifiedDigestAndRecordsIt(t *testing.T) {
	body := `out=""; prev=""; for a in "$@"; do [ "$prev" = "--output" ] && out="$a"; prev="$a"; done
printf kernel > "$out/` + bootKernelName() + `"; printf initrd > "$out/` + bootInitrdName + `"`
	bin, log := argsRecordingTool(t, "oras", body)
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return bin, nil }

	dir := t.TempDir()
	if err := orasPull(dir, BootFetch{Ref: pinnedRef}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, log); !strings.HasPrefix(got, "args: pull "+pinnedRef+" --output "+dir+"\n") {
		t.Errorf("oras was not asked for the pinned reference: %s", strings.SplitN(got, "\n", 2)[0])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Ref, Digest, VerifiedDigest string
		Files                       map[string]struct{ SHA256 string }
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(pinnedRef, "ghcr.io/nofireai/hull-assets@")
	if rec.Ref != pinnedRef || rec.Digest != digest || rec.VerifiedDigest != digest {
		t.Errorf("the record names %+v, not the bundle fetched", rec)
	}
	if rec.Files[bootKernelName()].SHA256 != sha256Hex([]byte("kernel")) ||
		rec.Files[bootInitrdName].SHA256 != sha256Hex([]byte("initrd")) {
		t.Errorf("the record does not list the files fetched: %+v", rec.Files)
	}

	if err := orasPull(dir, BootFetch{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "provenance.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a pull by tag left the record of another fetch behind: %v", err)
	}
}

// A replace fetches over the files in a directory brig chose, and never over
// a directory named in BRIG_BOOT_ASSETS.
func TestAReplaceFetchesOnlyWhereBrigChoseTheDirectory(t *testing.T) {
	emptyAssetHome(t)
	dir := t.TempDir()
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fetched := 0
	fetch := func(string) error { fetched++; return nil }
	locate := func() (string, error) { return dir, nil }

	if _, _, err := replaceBootArtifacts(locate, fetch, false); err != nil || fetched != 0 {
		t.Fatalf("files that are there were fetched without a replace (%d, %v)", fetched, err)
	}
	if _, _, err := replaceBootArtifacts(locate, fetch, true); err != nil || fetched != 1 {
		t.Fatalf("a replace did not fetch over the files brig chose (%d, %v)", fetched, err)
	}

	t.Setenv("BRIG_BOOT_ASSETS", dir)
	if _, _, err := replaceBootArtifacts(locate, fetch, true); err != nil || fetched != 1 {
		t.Fatalf("a replace fetched over a directory BRIG_BOOT_ASSETS named (%d, %v)", fetched, err)
	}
}
