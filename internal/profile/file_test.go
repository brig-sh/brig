package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reset gives a test the built-ins and nothing else, and puts the registry
// back afterwards so tests do not leak into each other.
func reset(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = Load() })
	if err := Load(); err != nil {
		t.Fatal(err)
	}
}

// Export then import is the documented way to write a profile, so it has to
// round-trip exactly -- including the explanatory header, which is the reason
// export writes YAML rather than JSON.
func TestExportImportRoundTrip(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	original, _ := Lookup("claude-code")

	blob, err := Export(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), firstHeaderLine) ||
		!strings.Contains(string(blob), BringYourOwnImageDoc) {
		t.Errorf("the export carries no explanation of its own fields:\n%s", blob)
	}
	back, path, err := Import(blob, dir)
	if err != nil {
		t.Fatalf("a profile brig itself produced did not import: %v", err)
	}
	if back.Name != original.Name || back.Image != original.Image ||
		back.GuestHome != original.GuestHome || len(back.Env) != len(original.Env) ||
		len(back.Deny) != len(original.Deny) {
		t.Errorf("round trip changed the profile:\n%+v\n%+v", original, back)
	}
	if filepath.Ext(path) != ".yaml" {
		t.Errorf("export/import produced %s, want a .yaml file", path)
	}
	if back.Onboarding == nil || back.Onboarding.TrustKey != original.Onboarding.TrustKey {
		t.Errorf("the onboarding block did not survive: %+v", back.Onboarding)
	}
	if back.HostCredential == nil || back.HostCredential.TargetVar != original.HostCredential.TargetVar {
		t.Errorf("the host credential block did not survive: %+v", back.HostCredential)
	}
}

// The name reaches a workspace path and a sandbox name, so it has to be safe
// in both.
func TestImportRejectsAnUnsafeName(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	for _, name := range []string{"../evil", "a/b", "Upper", ".hidden", "-flag", "with space", ""} {
		blob := []byte(`{"name":"` + name + `","image":"i","guestHome":"/home/x",
			"binary":"x","mem":1,"cpus":1}`)
		if _, _, err := Import(blob, dir); err == nil {
			t.Errorf("imported an unsafe name %q", name)
		}
	}
	// And a reasonable one is accepted.
	ok := []byte(`{"name":"my_agent-2.0","image":"i","guestHome":"/home/x",
		"binary":"x","mem":1,"cpus":1}`)
	if _, _, err := Import(ok, dir); err != nil {
		t.Errorf("rejected a reasonable name: %v", err)
	}
}

// A misspelled field would otherwise decode into nothing: a profile that
// forwards no credentials looks exactly like a broken sandbox. True of both
// spellings, since both go through the same strict decoder.
func TestParseRejectsUnknownFields(t *testing.T) {
	jsonBlob := []byte(`{"name":"x","image":"i","guestHome":"/home/x","binary":"x",
		"mem":1,"cpus":1,"forwards":["GH_TOKEN"]}`)
	if _, err := Parse(jsonBlob); err == nil {
		t.Error("a misspelled JSON field was ignored rather than reported")
	}
	yamlBlob := []byte("name: x\nimage: i\nguestHome: /home/x\nbinary: x\n" +
		"mem: 1\ncpus: 1\nforwards: [GH_TOKEN]\n")
	if _, err := Parse(yamlBlob); err == nil {
		t.Error("a misspelled YAML field was ignored rather than reported")
	}
}

// JSON is a subset of YAML, so one parser serves both and nothing has to
// guess at a format.
func TestParseAcceptsBothSpellings(t *testing.T) {
	yamlBlob := []byte(`# a comment, which is the point of YAML here
name: mine
image: docker.io/me/mine:latest
guestHome: /home/mine
binary: mine
forward: [GH_TOKEN]      # inline lists work too
mem: 2048
cpus: 2
`)
	jsonBlob := []byte(`{"name":"mine","image":"docker.io/me/mine:latest",
		"guestHome":"/home/mine","binary":"mine","forward":["GH_TOKEN"],
		"mem":2048,"cpus":2}`)

	fromYAML, err := Parse(yamlBlob)
	if err != nil {
		t.Fatalf("YAML did not parse: %v", err)
	}
	fromJSON, err := Parse(jsonBlob)
	if err != nil {
		t.Fatalf("JSON did not parse: %v", err)
	}
	if fromYAML.Name != fromJSON.Name || fromYAML.Image != fromJSON.Image ||
		fromYAML.Mem != fromJSON.Mem || len(fromYAML.Env) != len(fromJSON.Env) {
		t.Errorf("the two spellings disagree:\n%+v\n%+v", fromYAML, fromJSON)
	}
}

// A profile is a file someone wrote. Re-serialising it from the parsed
// struct would drop their comments and reorder their fields for no gain, so
// import stores the bytes as they came in -- and names the file after the
// format they are actually in.
func TestImportPreservesWhatYouWrote(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	blob := []byte(`# why this profile exists: the vendored CLI needs a bigger guest
name: mine
image: docker.io/me/mine:latest
guestHome: /home/mine
binary: mine
mem: 8192   # the CLI is a memory hog
cpus: 2
`)
	if _, path, err := Import(blob, dir); err != nil {
		t.Fatal(err)
	} else if filepath.Ext(path) != ".yaml" {
		t.Errorf("stored as %s, want .yaml", path)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "mine.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(blob) {
		t.Errorf("the file was rewritten:\n%s", stored)
	}
	for _, comment := range []string{"why this profile exists", "memory hog"} {
		if !strings.Contains(string(stored), comment) {
			t.Errorf("a comment was lost: %q", comment)
		}
	}

	// JSON keeps its own extension, and importing over a profile of the same
	// name does not leave both formats behind for directory order to pick
	// between.
	jsonBlob := []byte(`{"name":"mine","image":"i","guestHome":"/home/mine",
		"binary":"mine","mem":1,"cpus":1}`)
	if _, path, err := Import(jsonBlob, dir); err != nil {
		t.Fatal(err)
	} else if filepath.Ext(path) != ".json" {
		t.Errorf("stored as %s, want .json", path)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); !os.IsNotExist(err) {
		t.Error("both spellings are on disk; which one wins is now directory order")
	}
}

func TestValidateRequiresTheEssentials(t *testing.T) {
	cases := map[string]Profile{
		"no image":      {Name: "x", GuestHome: "/home/x", Binary: "x", Mem: 1, CPUs: 1},
		"no home":       {Name: "x", Image: "i", Binary: "x", Mem: 1, CPUs: 1},
		"relative home": {Name: "x", Image: "i", GuestHome: "home/x", Binary: "x", Mem: 1, CPUs: 1},
		"no binary":     {Name: "x", Image: "i", GuestHome: "/home/x", Mem: 1, CPUs: 1},
		"no size":       {Name: "x", Image: "i", GuestHome: "/home/x", Binary: "x"},
		"contradictory": {Name: "x", Image: "i", GuestHome: "/home/x", Binary: "x", Mem: 1, CPUs: 1,
			Env: []EnvBinding{{Name: "K", Ref: "env.K"}}, Deny: []string{"K"}},
	}
	for what, tmpl := range cases {
		if err := tmpl.Validate(); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	// A shell or gui profile needs no binary.
	shell := Profile{Name: "x", Image: "i", GuestHome: "/home/x", Kind: KindShell, Mem: 1, CPUs: 1}
	if err := shell.Validate(); err != nil {
		t.Errorf("a shell profile was rejected: %v", err)
	}
	// The missing-image error points at the documentation, because that is
	// the question someone writing their first profile actually has.
	err := Profile{Name: "x", GuestHome: "/home/x", Binary: "x", Mem: 1, CPUs: 1}.Validate()
	if !strings.Contains(err.Error(), "bring-your-own-image") {
		t.Errorf("error does not point at the docs: %v", err)
	}
}

// Overriding a built-in is the point: it is how you pin your own image for an
// agent brig already knows about.
func TestCustomProfileOverridesABuiltIn(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	blob := []byte("name: claude-code\nimage: docker.io/me/mine:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 2048\ncpus: 2\n")
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup("claude-code")
	if !ok || got.Image != "docker.io/me/mine:latest" {
		t.Errorf("the built-in was not overridden: %+v", got)
	}
	// The alias resolves to it too, and the listing says it is not ours.
	if got, _ := Lookup("claude"); got.Image != "docker.io/me/mine:latest" {
		t.Errorf("the alias still resolves to the built-in: %+v", got)
	}
	if !IsCustom("claude-code") {
		t.Error("the override is not reported as custom")
	}
	// Exactly one entry, not two with the same name.
	count := 0
	for _, tmpl := range All() {
		if tmpl.Name == "claude-code" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("claude-code appears %d times in the listing", count)
	}
}

// One unusable file must not take down the agent you were actually asking for.
func TestLoadReportsBadFilesAndKeepsGoing(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	good := []byte("name: good\nimage: i\nguestHome: /home/g\nbinary: g\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("name: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Anything that is not a profile is not a broken profile.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("the bad file was not reported: %v", err)
	}
	if _, ok := Lookup("good"); !ok {
		t.Error("the good profile was lost because another file was broken")
	}
}

// Most installs have no profile directory at all.
func TestLoadIgnoresAMissingDirectory(t *testing.T) {
	reset(t)
	if err := Load(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("a missing directory was an error: %v", err)
	}
}
