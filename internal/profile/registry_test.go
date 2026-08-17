package profile

import (
	"os"
	"path/filepath"
	"testing"
)

// Every profile brig ships is present with no configuration at all. This is
// the test that replaces the compiler: the specs are files now, so nothing
// else catches a spec that stopped parsing.
func TestBuiltInsLoadFromTheEmbeddedSpecs(t *testing.T) {
	reset(t)
	if err := Load(); err != nil {
		t.Fatalf("the embedded specs did not load: %v", err)
	}
	want := []string{
		"claude-code", "claude-desktop", "codex", "cursor",
		"gemini", "grok", "opencode", "ubuntu",
	}
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], name)
		}
	}
	// And they are usable, not merely present.
	for _, name := range want {
		p, ok := Lookup(name)
		if !ok {
			t.Errorf("%s does not look up", name)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s does not validate: %v", name, err)
		}
		if IsCustom(name) {
			t.Errorf("%s reports as custom, but it is embedded", name)
		}
	}
}

// A fresh install has no profile directory. Nothing pre-seeds it: every
// profile comes from the binary until you put a file there yourself.
func TestLoadWritesNothing(t *testing.T) {
	reset(t)
	dir := filepath.Join(t.TempDir(), "brig")
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the profile directory was created: %v", err)
	}
	if len(Names()) != 8 {
		t.Errorf("the built-ins are missing: %v", Names())
	}
}

// Later sources override earlier ones by name, which is how a file in the
// profile directory shadows a built-in -- and how brig would resolve two
// directories if it ever read more than one.
func TestLaterSourcesOverrideEarlier(t *testing.T) {
	reset(t)
	earlier, later := t.TempDir(), t.TempDir()
	write := func(dir, image string) {
		blob := []byte("name: claude-code\nimage: " + image + "\n" +
			"guestHome: /home/claude\nbinary: claude\nmem: 2048\ncpus: 2\n")
		if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(earlier, "docker.io/me/earlier:latest")
	write(later, "docker.io/me/later:latest")

	if err := Load(earlier, later); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup("claude-code")
	if !ok || got.Image != "docker.io/me/later:latest" {
		t.Errorf("the last source did not win: %+v", got)
	}
	if !IsCustom("claude-code") {
		t.Error("a file-backed override does not report as custom")
	}
	// Exactly one entry, not one per source.
	count := 0
	for _, p := range All() {
		if p.Name == "claude-code" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("claude-code appears %d times", count)
	}
}

// Nothing else asserts that Load starts from a clean slate rather than
// merging into whatever was there before; without this, the rule survives
// only on the order the test files happen to run in.
func TestLoadResetsTheRegistry(t *testing.T) {
	reset(t)
	before, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code is missing")
	}
	dir := t.TempDir()
	blob := []byte("name: claude-code\nimage: docker.io/me/mine:latest\n" +
		"guestHome: /home/claude\nbinary: claude\nmem: 2048\ncpus: 2\n")
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := Lookup("claude-code"); got.Image != "docker.io/me/mine:latest" {
		t.Fatalf("the override did not take: %+v", got)
	}
	// No directories this time: the override must not survive a Load that
	// never mentioned it.
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if got, _ := Lookup("claude-code"); got.Image != before.Image {
		t.Errorf("Load() did not reset the registry: got image %q, want the embedded %q",
			got.Image, before.Image)
	}
}

// Flat layout means every yaml in the directory is a profile, so brig reserves
// the basenames it would want for a settings file rather than reporting one as
// a broken profile.
func TestConfigFileIsNotAProfile(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	for _, name := range []string{"config.yaml", "config.yml", "config.json"} {
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte("runtime: nerdctl\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Load(dir); err != nil {
		t.Errorf("a settings file was reported as a broken profile: %v", err)
	}
	if _, ok := Lookup("config"); ok {
		t.Error("config.yaml was loaded as a profile")
	}
}

// A profile named config could never be read back: the loader skips that
// basename in every extension it accepts, so importing one would write a file
// that nothing loads and no command can remove.
func TestImportRefusesAReservedName(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	blob := []byte("name: config\nimage: i\nguestHome: /home/c\nbinary: c\nmem: 1\ncpus: 1\n")
	if _, _, err := Import(blob, dir); err == nil {
		t.Fatal("imported a profile the loader will never read")
	}
	// A name that merely starts with the reserved word is fine: config-mine.yaml
	// is not a basename the loader skips.
	ok := []byte("name: config-mine\nimage: i\nguestHome: /home/c\nbinary: c\nmem: 1\ncpus: 1\n")
	if _, _, err := Import(ok, dir); err != nil {
		t.Errorf("refused a name that only resembles the reserved one: %v", err)
	}
}
