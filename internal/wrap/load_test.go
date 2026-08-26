package wrap

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

func loadFor(t *testing.T, name string) *Config {
	t.Helper()
	p, ok := profile.Lookup(name)
	if !ok {
		t.Fatalf("no profile %q", name)
	}
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bound(c *Config, name string) bool {
	for _, b := range c.Env {
		if b.Name == name {
			return true
		}
	}
	return false
}

// BRIG_NAME replaces the whole sandbox name, and ls and reset find brig's own
// sandboxes by the brig- prefix. A name without it boots and runs but is
// invisible to both, so it is refused at creation with a message naming the
// constraint -- the alternative to tracking the sandbox by a mark the runtime
// does not hand back.
func TestBrigNameMustCarryThePrefix(t *testing.T) {
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}

	t.Setenv("BRIG_NAME", "custom")
	_, err := Load(p, Options{}, nil)
	if err == nil {
		t.Fatal("Load accepted a BRIG_NAME without the brig- prefix")
	}
	if !strings.Contains(err.Error(), NamePrefix) {
		t.Errorf("Load error %q does not name the required prefix %q", err, NamePrefix)
	}

	// The same name with the prefix is accepted and stamped onto the sandbox
	// verbatim, so ls and reset select it the way they select any other.
	t.Setenv("BRIG_NAME", "brig-custom")
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatalf("Load refused a prefixed BRIG_NAME: %v", err)
	}
	if c.VMName != "brig-custom" {
		t.Errorf("VMName = %q, want brig-custom", c.VMName)
	}
	if !strings.HasPrefix(c.VMName, NamePrefix) {
		t.Errorf("VMName = %q, which ls and reset would not find", c.VMName)
	}
}

// --workspace was made absolute and BRIG_WORKSPACE was not, so one exported
// variable meant a different host directory per directory you happened to run
// brig from: same sandbox name, same profile, and a home that moved with the
// shell. The sandbox's home is a host path, so it is resolved once.
func TestWorkspaceFromTheEnvironmentIsMadeAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("BRIG_WORKSPACE", "relative-workspace")

	c := loadFor(t, "claude-code")
	if !filepath.IsAbs(c.Workspace) {
		t.Fatalf("workspace = %q, which is relative to whatever directory brig was run from",
			c.Workspace)
	}
	if filepath.Base(c.Workspace) != "relative-workspace" {
		t.Errorf("workspace = %q, want it to end in the directory that was asked for", c.Workspace)
	}
	// Resolved against the invoking directory, not against anything else.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(filepath.Dir(c.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("workspace resolved under %q, want %q", got, want)
	}
}
