package profile

import (
	"os"
	"path/filepath"
	"testing"
)

// The profile directory follows the XDG Base Directory Specification,
// version 0.8. Each case below is a clause of it, and the relative-path one
// is the clause brig would otherwise get wrong.
func TestDirFollowsXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRIG_PROFILE_DIR", "")
	t.Setenv("BRIG_TEMPLATE_DIR", "")

	// Set and absolute: used as given.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := Dir(), filepath.Join(xdg, "brig"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}

	// "If $XDG_CONFIG_HOME is either not set or empty, a default equal to
	// $HOME/.config should be used." Empty counts as unset.
	fallback := filepath.Join(home, ".config", "brig")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := Dir(); got != fallback {
		t.Errorf("empty XDG_CONFIG_HOME: Dir() = %q, want %q", got, fallback)
	}

	// "All paths set in these environment variables must be absolute. If an
	// implementation encounters a relative path ... it should consider the
	// path invalid and ignore it." brig runs from whatever project directory
	// you are in, so honouring a relative value would resolve profiles
	// against the current directory and quietly find a different set per
	// project.
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if got := Dir(); got != fallback {
		t.Errorf("relative XDG_CONFIG_HOME was honoured: Dir() = %q, want %q", got, fallback)
	}
}

// BRIG_PROFILE_DIR is brig's own variable, so it is taken as given: an
// explicit override is a deliberate act and is not second-guessed for
// absoluteness. BRIG_TEMPLATE_DIR is the older spelling and still works.
func TestDirOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Setenv("BRIG_TEMPLATE_DIR", "/legacy/dir")
	if got := Dir(); got != "/legacy/dir" {
		t.Errorf("BRIG_TEMPLATE_DIR ignored: Dir() = %q", got)
	}
	t.Setenv("BRIG_PROFILE_DIR", "/new/dir")
	if got := Dir(); got != "/new/dir" {
		t.Errorf("BRIG_PROFILE_DIR does not win: Dir() = %q", got)
	}
}

// 0700 is the XDG spec's rule for creating a directory on write, and the right
// mode anyway for a directory of files that name credential variables. Nothing
// else pins it, so a revert to 0755 would be silent.
func TestImportCreatesThePrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "brig")
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n")
	if _, _, err := Import(blob, dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("profile directory mode = %04o, want 0700", perm)
	}
}
