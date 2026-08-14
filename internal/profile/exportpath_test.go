package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bare word is a name. The profile directory is the only place a profile
// file does anything, so resolving there is what makes `brig profile export
// codex mine` a complete act rather than the first half of one.
func TestExportPathResolvesABareNameIntoTheProfileDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)

	cases := []struct {
		dest   string
		asJSON bool
		want   string
	}{
		{"mine", false, filepath.Join(dir, "mine.yaml")},
		{"mine", true, filepath.Join(dir, "mine.json")},
		// An extension the user typed is one brig must not type again.
		{"mine.yaml", false, filepath.Join(dir, "mine.yaml")},
		{"mine.yml", false, filepath.Join(dir, "mine.yml")},
		{"mine.json", true, filepath.Join(dir, "mine.json")},
	}
	for _, c := range cases {
		got, err := ExportPath(c.dest, c.asJSON)
		if err != nil {
			t.Errorf("ExportPath(%q, %v): %v", c.dest, c.asJSON, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExportPath(%q, %v) = %q, want %q", c.dest, c.asJSON, got, c.want)
		}
	}
}

// Export writes one directory and no other. A path is refused rather than
// honoured: wanting a copy elsewhere already has a spelling, and it is the one
// the error names.
func TestExportPathRefusesAPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	for _, dest := range []string{
		"/tmp/mine.yaml", "./mine.yaml", "../mine.yaml", "sub/mine.yaml", "~/mine.yaml",
		"/etc/passwd", "../../escape",
	} {
		got, err := ExportPath(dest, false)
		if err == nil {
			t.Errorf("ExportPath(%q) = %q, want a refusal", dest, got)
			continue
		}
		if !strings.Contains(err.Error(), ">") {
			t.Errorf("ExportPath(%q) does not name the redirect: %v", dest, err)
		}
	}
	// No destination is stdout, and stays empty for the caller to notice.
	if got, err := ExportPath("", false); err != nil || got != "" {
		t.Errorf(`ExportPath("") = %q, %v`, got, err)
	}
}

// Whatever a destination says, the file lands in the profile directory --
// there is no spelling of it that escapes.
func TestExportPathNeverLeavesTheProfileDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	for _, dest := range []string{"mine", "mine.yaml", "a.b.c", "x_1-2"} {
		got, err := ExportPath(dest, false)
		if err != nil {
			t.Errorf("ExportPath(%q): %v", dest, err)
			continue
		}
		if filepath.Dir(got) != dir {
			t.Errorf("ExportPath(%q) = %q, which is outside %s", dest, got, dir)
		}
	}
}

// A name that cannot be a profile file name is refused rather than turned into
// one: the same rule Validate applies to the name field, because these are the
// same characters ending up in the same places.
func TestExportPathRefusesAnUnusableName(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, dest := range []string{"MiXeD", "has space", "-leading", "wat?"} {
		if got, err := ExportPath(dest, false); err == nil {
			t.Errorf("ExportPath(%q) = %q, want an error", dest, got)
		}
	}
	// config.yaml in the profile directory is brig's own, so a profile
	// written there would never be read back.
	if _, err := ExportPath("config", false); err == nil {
		t.Error("export accepted a destination the loader skips")
	}
}

// Files in the pre-profile directory are read by nothing and look exactly like
// files that work: the profile they pinned silently reverts to brig's own.
// The hint is the only signal that happened.
func TestLegacyHintNamesTheOldDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	t.Setenv("BRIG_TEMPLATE_DIR", "")

	// Nothing there: nothing to say, so callers can print it unconditionally.
	if hint := LegacyHint(); hint != "" {
		t.Errorf("a hint with no legacy directory: %q", hint)
	}

	old := filepath.Join(home, ".config", "brig", "templates")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	// An empty directory is not a migration anyone needs told about.
	if hint := LegacyHint(); hint != "" {
		t.Errorf("a hint for an empty legacy directory: %q", hint)
	}
	write(t, old, "mine.yaml", "mine", "docker.io/me/mine:latest")
	if err := os.WriteFile(filepath.Join(old, "README"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hint := LegacyHint()
	if !strings.Contains(hint, old) || !strings.Contains(hint, Dir()) {
		t.Errorf("the hint does not name both directories: %q", hint)
	}
	if !strings.Contains(hint, "1 profile(s)") {
		t.Errorf("the hint counted the README as a profile: %q", hint)
	}
}

// $XDG_CONFIG_HOME is not consulted for the legacy directory, and that is the
// point: the code that created it joined the home directory with .config
// unconditionally. Resolving it the modern way would name a directory that
// never existed, for exactly the users who set that variable.
func TestLegacyDirIgnoresXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := filepath.Join(home, ".config", "brig", "templates")
	if got := LegacyDir(); got != want {
		t.Errorf("LegacyDir() = %q, want %q", got, want)
	}
}

// Pointing BRIG_PROFILE_DIR at the old directory makes it the live one, and a
// hint telling you to migrate a directory brig is already reading is noise.
func TestLegacyHintIsSilentWhenTheOldDirectoryIsTheLiveOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := filepath.Join(home, ".config", "brig", "templates")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, old, "mine.yaml", "mine", "docker.io/me/mine:latest")
	t.Setenv("BRIG_PROFILE_DIR", old)
	if hint := LegacyHint(); hint != "" {
		t.Errorf("hinted about the directory brig is reading: %q", hint)
	}
}
