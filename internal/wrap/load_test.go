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

// A refresh token is durable account access rather than the short-lived
// credential beside it, so a zero-config run does not hand one to a sandbox
// with unrestricted egress.
func TestTheRefreshTokenIsNotForwardedByDefault(t *testing.T) {
	for _, name := range []string{"claude-code", "claude-desktop"} {
		c := loadFor(t, name)
		if bound(c, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
			t.Errorf("%s forwards a refresh token with nothing asked for", name)
		}
		// The short-lived half is still forwarded: this is about which of the
		// two a sandbox gets by default, not about breaking the login.
		if !bound(c, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Errorf("%s stopped forwarding the access token", name)
		}
		if len(c.HeldOptIn) == 0 {
			t.Errorf("%s held the binding back without recording it for the report", name)
		}
	}
}

// Held back, not removed: the refresh-token pair is the documented way to
// provision auth without a browser, and a user who needs it says so.
func TestTheRefreshTokenIsForwardedWhenAskedFor(t *testing.T) {
	t.Setenv("BRIG_FORWARD_OPTIN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN")
	c := loadFor(t, "claude-code")
	if !bound(c, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
		t.Error("BRIG_FORWARD_OPTIN did not bring the opt-in binding back")
	}
	if len(c.HeldOptIn) != 0 {
		t.Errorf("a binding that was asked for is still reported as held: %v", c.HeldOptIn)
	}
}

// The per-profile spelling every other setting has works here too.
func TestTheRefreshTokenOptInIsPerProfileToo(t *testing.T) {
	t.Setenv("BRIG_CLAUDE_CODE_FORWARD_OPTIN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN")
	if c := loadFor(t, "claude-code"); !bound(c, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
		t.Error("the per-profile setting did not opt in")
	}
	if c := loadFor(t, "claude-desktop"); bound(c, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
		t.Error("a setting for one profile opted another one in")
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
	c := loadFor(t, "claude-code")
	c.Runtime = fakeRuntime{}
	out := &strings.Builder{}
	c.Out = out
	c.Status(creds.Set{})
	got := out.String()
	if !strings.Contains(got, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN") ||
		!strings.Contains(got, "BRIG_FORWARD_OPTIN") {
		t.Errorf("the report does not say the refresh token exists and how to ask for it:\n%s", got)
	}
}
