package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Starting from the closest existing profile is the documented way to write
// one, so export has to hand back the annotated file brig ships rather than a
// re-marshalled struct. The comments are the reason: they carry why the deny
// list is what it is, which no amount of reading the values recovers.
func TestExportIsVerbatim(t *testing.T) {
	reset(t)
	p, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code is missing")
	}
	blob, err := Export(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"outrank CLAUDE_CODE_OAUTH_TOKEN", // the billing-precedence comment
		"Memory is deliberately absent",   // why memory is not projected
		"name: claude-code",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("the export lost %q:\n%s", want, blob)
		}
	}
	// Field order is the file's, not the marshaller's alphabetical one.
	if strings.Index(string(blob), "name:") > strings.Index(string(blob), "cpus:") {
		t.Error("the fields were reordered, so this is a re-marshalled struct")
	}
}

// The field-reference header is what makes an export a starting point rather
// than a puzzle, and it must not stack up when a file goes round more than
// once.
func TestExportHeaderIsNotDuplicated(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	p, _ := Lookup("codex")

	once, err := Export(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(once), firstHeaderLine) != 1 {
		t.Fatalf("expected exactly one header:\n%s", once)
	}
	// Round-trip it and export again: the stored bytes now begin with the
	// header, so a second one must not be prepended.
	back, _, err := Import(once, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	twice, err := Export(back)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(twice), firstHeaderLine); got != 1 {
		t.Errorf("the header appears %d times after a round trip:\n%s", got, twice)
	}
}

// A profile built in Go has no source bytes, so export falls back to
// marshalling it. Anything generating profiles programmatically relies on
// this.
func TestExportFallsBackToMarshalling(t *testing.T) {
	reset(t)
	p := Profile{Name: "synthetic", Image: "i", GuestHome: "/home/s",
		Binary: "s", Mem: 1, CPUs: 1}
	blob, err := Export(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "synthetic") {
		t.Errorf("the fallback did not render the profile:\n%s", blob)
	}
	if !strings.Contains(string(blob), firstHeaderLine) {
		t.Error("the fallback dropped the header")
	}
}

// `brig profiles` asks these two where a profile came from, so a built-in and
// a name that was never registered must not answer the same way an override
// does.
func TestPathAndOverridesBuiltIn(t *testing.T) {
	reset(t)
	if _, ok := Path("claude-code"); ok {
		t.Error("an embedded profile reported a file path")
	}
	if OverridesBuiltIn("claude-code") {
		t.Error("an embedded profile reported as an override")
	}
	dir := t.TempDir()
	// Two files: one shadowing a built-in, one that is only ever a file.
	for name, image := range map[string]string{
		"claude-code": "docker.io/me/mine:latest",
		"mine":        "docker.io/me/other:latest",
	} {
		blob := []byte("name: " + name + "\nimage: " + image +
			"\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n")
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if got, ok := Path("claude-code"); !ok || got != filepath.Join(dir, "claude-code.yaml") {
		t.Errorf("Path = %q, %v", got, ok)
	}
	if !OverridesBuiltIn("claude-code") {
		t.Error("a file shadowing a built-in did not report as an override")
	}
	// A file-backed profile with no built-in of that name is custom but
	// shadows nothing -- the distinction the listing exists to draw.
	if !IsCustom("mine") || OverridesBuiltIn("mine") {
		t.Error("a profile that is only ever a file reported as an override")
	}
	if _, ok := Path("nosuchprofile"); ok {
		t.Error("an unknown name reported a path")
	}
}

// firstHeaderLine is a prefix of exportHeader, not a copy of its first line
// -- the header's first line carries the import hint too. Export's dedup
// check (bytes.HasPrefix) and the tests above (strings.Count) both depend on
// firstHeaderLine appearing exactly once, at the very start, so pin both
// properties directly rather than trusting the header text by eye.
func TestFirstHeaderLineIsAPrefixOfExportHeader(t *testing.T) {
	if !strings.HasPrefix(exportHeader, firstHeaderLine) {
		t.Errorf("exportHeader does not start with firstHeaderLine:\n%s", exportHeader)
	}
	if got := strings.Count(exportHeader, firstHeaderLine); got != 1 {
		t.Errorf("firstHeaderLine appears %d times in exportHeader, want 1:\n%s", got, exportHeader)
	}
}
