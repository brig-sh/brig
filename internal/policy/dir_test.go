package policy

import (
	"path/filepath"
	"testing"
)

// The policy directory follows the same XDG rules internal/profile's Dir
// does. Each case below is the same clause internal/profile/dir_test.go
// checks, applied here.
func TestDirFollowsXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRIG_POLICY_DIR", "")

	// Set and absolute: used as given.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := Dir(), filepath.Join(xdg, "brig", "policies"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}

	// Unset or empty falls back to $HOME/.config.
	fallback := filepath.Join(home, ".config", "brig", "policies")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := Dir(); got != fallback {
		t.Errorf("empty XDG_CONFIG_HOME: Dir() = %q, want %q", got, fallback)
	}

	// A relative value is invalid per the spec, and ignored the same way
	// internal/profile's Dir ignores one.
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if got := Dir(); got != fallback {
		t.Errorf("relative XDG_CONFIG_HOME was honoured: Dir() = %q, want %q", got, fallback)
	}
}

// BRIG_POLICY_DIR is its own variable: it must win over the default, and it
// must not be satisfied by BRIG_PROFILE_DIR.
func TestDirOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Setenv("BRIG_PROFILE_DIR", "/some/profile/dir")
	t.Setenv("BRIG_POLICY_DIR", "")
	if got := Dir(); got == "/some/profile/dir" {
		t.Errorf("Dir() followed BRIG_PROFILE_DIR: %q", got)
	}

	t.Setenv("BRIG_POLICY_DIR", "/new/dir")
	if got := Dir(); got != "/new/dir" {
		t.Errorf("BRIG_POLICY_DIR does not win: Dir() = %q", got)
	}
}
