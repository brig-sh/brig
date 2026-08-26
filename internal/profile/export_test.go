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
		"outrank Claude Code's subscription credential", // the billing-precedence comment
		"Memory is deliberately absent",                 // why memory is not projected
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

// Export under a name of the caller's choosing rewrites the profile's own
// name and leaves the rest of the file alone. The rename is what makes the
// documented recipe work -- export the closest profile under a name of your
// own, edit it, run it -- and the rest of the file is what makes starting
// from an existing profile worth doing at all.
func TestExportAsRenamesTheProfileAndNothingElse(t *testing.T) {
	reset(t)
	p, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code is missing")
	}
	blob, err := ExportAs(p, "mytool")
	if err != nil {
		t.Fatal(err)
	}
	// It declares the name it was asked for, and reads back as that profile:
	// brig keys on this field, so anything else is a file that cannot be
	// addressed by the name it was written under.
	got, err := Parse(blob)
	if err != nil {
		t.Fatalf("the renamed export does not parse: %v\n%s", err, blob)
	}
	if got.Name != "mytool" {
		t.Errorf("name = %q, want mytool", got.Name)
	}
	if got.Image != p.Image || got.GuestHome != p.GuestHome {
		t.Error("the rename changed more than the name")
	}
	// The other places the word appears in an export are a list entry's field
	// and the header's field reference. Neither is the profile's name.
	if !strings.Contains(string(blob), "- name: claude-credentials") {
		t.Errorf("a secret's own name was rewritten:\n%s", blob)
	}
	if !strings.Contains(string(blob), "outrank Claude Code's subscription credential") {
		t.Error("the rename cost the file its comments")
	}
	if got := strings.Count(string(blob), firstHeaderLine); got != 1 {
		t.Errorf("the header appears %d times after a rename, want 1", got)
	}
	// The header explains the fields and says nothing about which profile this
	// is, which is what keeps it true after a rename. A header that named the
	// profile it was copied from would describe a file that no longer exists
	// under that name.
	header, _, _ := strings.Cut(string(blob), "\nname:")
	if strings.Contains(header, p.Name) {
		t.Errorf("the header still describes %s:\n%s", p.Name, header)
	}
	// The plain export is unchanged: nothing was asked to be renamed.
	same, err := ExportAs(p, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := Export(p); err != nil || string(plain) != string(same) {
		t.Errorf("exporting under the profile's own name rewrote the file: %v", err)
	}
}

// A body with no top-level name: line to rewrite -- a profile of your own
// written as JSON, or a flow mapping on one line -- still has to come out
// declaring the name it was exported under. It loses its comments to the
// marshaller, which is the price of a file the line-based rewrite cannot
// reach, and a file that lied about its own name is not an alternative.
func TestExportAsFallsBackWhenThereIsNoNameLineToRewrite(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	flow := []byte(`{name: flowagent, image: docker.io/me/f:latest, ` +
		`guestHome: /home/f, binary: f, mem: 1, cpus: 1}`)
	if err := os.WriteFile(filepath.Join(dir, "flowagent.yaml"), flow, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	p, ok := Lookup("flowagent")
	if !ok {
		t.Fatal("flowagent did not load")
	}
	blob, err := ExportAs(p, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(blob)
	if err != nil {
		t.Fatalf("the fallback export does not parse: %v\n%s", err, blob)
	}
	if got.Name != "renamed" {
		t.Errorf("name = %q, want renamed", got.Name)
	}
	if got.Image != p.Image {
		t.Errorf("the fallback lost the profile's settings: %+v", got)
	}
}

// The JSON spelling carries no comments, so the rename is the field and
// nothing else -- but it is still the same promise: the file declares the name
// it was written under.
func TestExportJSONAsRenames(t *testing.T) {
	reset(t)
	p, _ := Lookup("codex")
	blob, err := ExportJSONAs(p, "robot")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "robot" {
		t.Errorf("name = %q, want robot", got.Name)
	}
	// The value it was called on is a copy: renaming an export must not rename
	// the profile the registry is holding.
	if p.Name != "codex" {
		t.Errorf("ExportJSONAs renamed the caller's profile: %q", p.Name)
	}
}

// A profile written on Windows, or edited by something that keeps CRLF, comes
// back as it went in. The rename is a text edit on one line, and the export is
// verbatim everywhere else -- so a line that arrived ending in \r\n has to
// leave ending in \r\n. Rebuilding it with a bare \n leaves one LF line in a
// CRLF file, which is a byte the export was never asked to change and enough
// to make a diff of the two files noise.
func TestExportAsKeepsTheLineEndingsItWasGiven(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	src := firstHeaderLine + " Edit it, then: brig profile import <this file>\r\n" +
		"name: mytool\r\n" +
		"image: docker.io/me/mine:latest\r\n" +
		"guestHome: /home/mytool\r\n" +
		"binary: mytool\r\n" +
		"mem: 1\r\n" +
		"cpus: 1\r\n"
	if err := os.WriteFile(filepath.Join(dir, "mytool.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	p, ok := Lookup("mytool")
	if !ok {
		t.Fatal("the CRLF profile did not load")
	}
	blob, err := ExportAs(p, "other")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(src, "name: mytool\r\n", "name: other\r\n", 1)
	if string(blob) != want {
		t.Errorf("the export is not the file it was given, renamed:\ngot  %q\nwant %q", blob, want)
	}
	// Stated separately because it is the failure someone reads in a diff: one
	// line of a CRLF file ending differently from the rest.
	if strings.Count(string(blob), "\n") != strings.Count(string(blob), "\r\n") {
		t.Errorf("the rename left a bare LF among the CRLF lines:\n%q", blob)
	}
}
