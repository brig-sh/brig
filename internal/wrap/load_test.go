package wrap

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
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

// optIn: holds a binding back until the run asks for it by name.
//
// Tested against a fixture rather than a shipped profile: which credentials a
// built-in offers is a policy that changes -- the Claude profiles now deliver
// theirs as a file -- while the mechanism has to keep working for any profile
// that binds a durable credential as a variable.
func optInFixture(t *testing.T) profile.Profile {
	t.Helper()
	return profile.Profile{
		Name: "fixture", Image: "x", GuestHome: "/home/u", Binary: "x", Mem: 1, CPUs: 1,
		Env: []profile.EnvBinding{
			{Name: "SHORT_LIVED", Ref: "env.SHORT_LIVED"},
			{Name: "DURABLE", Ref: "env.DURABLE", OptIn: true},
		},
	}
}

func loadFixture(t *testing.T) *Config {
	t.Helper()
	c, err := Load(optInFixture(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOptInIsNotForwardedByDefault(t *testing.T) {
	c := loadFixture(t)
	if bound(c, "DURABLE") {
		t.Error("an opt-in binding was forwarded with nothing asked for")
	}
	if !bound(c, "SHORT_LIVED") {
		t.Error("holding one binding back dropped the others")
	}
	if len(c.HeldOptIn) == 0 {
		t.Error("the binding was held back without recording it for the report")
	}
}

// Held back, not removed: a user who needs it says so.
func TestOptInIsForwardedWhenAskedFor(t *testing.T) {
	t.Setenv("BRIG_FORWARD_OPTIN", "DURABLE")
	c := loadFixture(t)
	if !bound(c, "DURABLE") {
		t.Error("BRIG_FORWARD_OPTIN did not bring the opt-in binding back")
	}
	if len(c.HeldOptIn) != 0 {
		t.Errorf("a binding that was asked for is still reported as held: %v", c.HeldOptIn)
	}
}

// The per-profile spelling every other setting has works here too.
func TestOptInIsPerProfileToo(t *testing.T) {
	t.Setenv("BRIG_FIXTURE_FORWARD_OPTIN", "DURABLE")
	if c := loadFixture(t); !bound(c, "DURABLE") {
		t.Error("the per-profile setting did not opt in")
	}
}

// BRIG_FORWARD_ENV names what the environment forwards, so naming it there is
// the other way of asking -- and it must keep working, or the opt-in becomes
// a value nobody can forward.
func TestBrigForwardEnvCanAskForTheRefreshTokenToo(t *testing.T) {
	t.Setenv("BRIG_FORWARD_ENV", "CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CODE_OAUTH_REFRESH_TOKEN")
	c := loadFor(t, "claude-code")
	if !bound(c, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
		t.Error("BRIG_FORWARD_ENV did not forward what it named")
	}
}

// The report is what makes a held binding discoverable: a capability the
// profile offers and this run did not take.
func TestStatusNamesWhatIsHeldBack(t *testing.T) {
	c := loadFixture(t)
	c.Runtime = fakeRuntime{}
	out := &strings.Builder{}
	c.Out = out
	c.Status(creds.Set{})
	got := out.String()
	if !strings.Contains(got, "DURABLE") || !strings.Contains(got, "BRIG_FORWARD_OPTIN") {
		t.Errorf("the report does not say the held binding exists and how to ask for it:\n%s", got)
	}
}
