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
	"time"
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
		Files                       map[string]struct {
			SHA256 string
			Size   int64
		}
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
	// hull on Linux refuses a kernel whose size differs from the record's, and
	// reads an absent size as zero.
	if rec.Files[bootKernelName()].Size != int64(len("kernel")) ||
		rec.Files[bootInitrdName].Size != int64(len("initrd")) {
		t.Errorf("the record does not give the size of each file fetched: %+v", rec.Files)
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

// A record a brig without sizes wrote is completed for the files that still
// match it, so hull on Linux stops refusing the boot. A file that changed
// keeps the record as it was: filling in its size would make hull accept it.
func TestFillFetchRecordSizesCompletesOnlyMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(bootKernelName(), "kernel")
	write(bootInitrdName, "initrd")
	record := `{"ref":"r@sha256:aa","digest":"sha256:aa","verifiedDigest":"sha256:aa","files":{` +
		`"` + bootKernelName() + `":{"sha256":"` + sha256Hex([]byte("kernel")) + `"},` +
		`"` + bootInitrdName + `":{"sha256":"` + sha256Hex([]byte("other")) + `"}}}`
	write(fetchRecordName, record)

	if err := fillFetchRecordSizes(dir); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, fetchRecordName))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Ref, Digest, VerifiedDigest string
		Files                       map[string]struct {
			SHA256 string
			Size   *int64
		}
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Ref != "r@sha256:aa" || rec.Digest != "sha256:aa" || rec.VerifiedDigest != "sha256:aa" {
		t.Errorf("the record lost what it named: %s", raw)
	}
	if k := rec.Files[bootKernelName()]; k.Size == nil || *k.Size != int64(len("kernel")) {
		t.Errorf("the matching kernel did not get its size: %s", raw)
	}
	if i := rec.Files[bootInitrdName]; i.Size != nil || i.SHA256 != sha256Hex([]byte("other")) {
		t.Errorf("an initrd that differs from the record got a size: %s", raw)
	}
}

// A directory with no record gets none: the fill completes a record, it never
// writes one.
func TestFillFetchRecordSizesWritesNoRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bootKernelName()), []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fillFetchRecordSizes(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, fetchRecordName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a record appeared where there was none: %v", err)
	}
}

// sizelessRecord writes the kernel and initrd into dir and a record of them
// with digests and no sizes, as a brig before the sizes wrote it, plus any
// extra entries.
func sizelessRecord(t *testing.T, dir string, extra string) {
	t.Helper()
	for name, body := range map[string]string{bootKernelName(): "kernel", bootInitrdName: "initrd"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record := `{"ref":"r@sha256:aa","digest":"sha256:aa","files":{` +
		`"` + bootKernelName() + `":{"sha256":"` + sha256Hex([]byte("kernel")) + `"},` +
		`"` + bootInitrdName + `":{"sha256":"` + sha256Hex([]byte("initrd")) + `"}` + extra + `}}`
	if err := os.WriteFile(filepath.Join(dir, fetchRecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The fill reads the two files hull boots and nothing else the record names.
// A name that leads out of the directory is neither read nor given a size, and
// the record is replaced whole, with no temporary file left beside it.
func TestFillFetchRecordSizesReadsOnlyTheBootFiles(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "assets")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sizelessRecord(t, dir, `,"../outside":{"sha256":"`+sha256Hex([]byte("secret"))+`"}`)

	if err := fillFetchRecordSizes(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, fetchRecordName))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Files map[string]struct{ Size *int64 }
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Files["../outside"].Size != nil {
		t.Errorf("a file outside the directory was read for the record: %s", raw)
	}
	if k := rec.Files[bootKernelName()].Size; k == nil || *k != int64(len("kernel")) {
		t.Errorf("the kernel did not get its size: %s", raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("the record's write left %s behind", e.Name())
		}
	}
}

// A kernel that is a FIFO is refused, not read: reading one waits for a writer
// that never comes, and every run would hang. An initrd entry with no digest
// is not read at all, whatever the file is.
func TestFillFetchRecordSizesReadsNoFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs")
	}
	dir := t.TempDir()
	sizelessRecord(t, dir, "")
	for _, name := range []string{bootKernelName(), bootInitrdName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if err := mkfifo(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- fillFetchRecordSizes(dir) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a FIFO kernel was not refused: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fill hung on a FIFO")
	}

	record := `{"files":{"` + bootInitrdName + `":{}}}`
	if err := os.WriteFile(filepath.Join(dir, fetchRecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() { done <- fillFetchRecordSizes(dir) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("an entry with no digest was read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fill opened an entry with no digest")
	}
}

// A BRIG_BOOT_ASSETS directory is the user's, and its record is left as it
// is. A directory brig chose gets the sizes.
func TestFillBootRecordLeavesANamedDirectory(t *testing.T) {
	for _, named := range []bool{true, false} {
		dir := t.TempDir()
		sizelessRecord(t, dir, "")
		before, _ := os.ReadFile(filepath.Join(dir, fetchRecordName))
		fillBootRecord(RunSpec{GenericBoot: true, BootAssets: BootAssets{
			Kernel: filepath.Join(dir, bootKernelName()), Initrd: filepath.Join(dir, bootInitrdName), Named: named}})
		after, _ := os.ReadFile(filepath.Join(dir, fetchRecordName))
		if named && !bytes.Equal(before, after) {
			t.Errorf("the record in a named directory was rewritten: %s", after)
		}
		if !named && bytes.Equal(before, after) {
			t.Errorf("the record in the directory brig chose was not filled: %s", after)
		}
	}
}

// A record the fill could not complete is named on the notice, with the
// remedy, rather than left to hull's refusal, which reads as tampering.
func TestFillBootRecordSaysWhenItCannotFill(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	sizelessRecord(t, dir, "")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	var notice bytes.Buffer
	fillBootRecord(RunSpec{GenericBoot: true, Notice: &notice, BootAssets: BootAssets{
		Kernel: filepath.Join(dir, bootKernelName()), Initrd: filepath.Join(dir, bootInitrdName)}})
	if got := notice.String(); !strings.Contains(got, "from an older brig") || !strings.Contains(got, "delete the kernel and initrd") {
		t.Errorf("the notice does not say what happened and what to do: %q", got)
	}
}
