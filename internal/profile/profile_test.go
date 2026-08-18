package profile

import (
	"slices"
	"strings"
	"testing"
)

// The denylist is the billing guard: a variable that outranks the
// subscription credential must never be in the forward list, or a sandbox
// moves onto metered API billing without saying so. The Homebrew formula
// asserted this too, and it is the one invariant a careless edit to the table
// above would break silently.
func TestDeniedVarsAreNotForwarded(t *testing.T) {
	reset(t)
	for _, tmpl := range All() {
		for _, denied := range tmpl.Deny {
			for _, b := range tmpl.Env {
				if b.Name == denied {
					t.Errorf("%s binds %s, which is on its own denylist", tmpl.Name, denied)
				}
			}
			if !tmpl.Denied(denied) {
				t.Errorf("%s: Denied(%q) = false", tmpl.Name, denied)
			}
		}
	}
}

// Claude Code's own precedence puts both of these ahead of the credential
// brig delivers, so they stay denied whatever channel that credential takes.
func TestClaudeDeniesTheMeteredKeys(t *testing.T) {
	reset(t)
	tmpl, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code profile is missing")
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if !tmpl.Denied(want) {
			t.Errorf("claude-code does not deny %s", want)
		}
	}
}

// GH_TOKEN keeps its chain, and the chain's ORDER is the whole compatibility
// argument for the switchover: `GH_TOKEN=$(gh auth token) brig run claude-code`
// worked for every user before this profile bound the name at all, and a bound
// name is dropped from the ambient forward. env. first is what keeps that
// working; the store is the fallback for a shell that exports nothing.
func TestClaudeReadsGHTokenFromTheShellFirst(t *testing.T) {
	reset(t)
	tmpl, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code profile is missing")
	}
	for _, b := range tmpl.Env {
		if b.Name != "GH_TOKEN" {
			continue
		}
		want := []string{"env.GH_TOKEN", "secrets.gh-token"}
		if !slices.Equal(b.RefList(), want) {
			t.Fatalf("GH_TOKEN refs = %v, want %v", b.RefList(), want)
		}
		return
	}
	t.Error("claude-code no longer binds GH_TOKEN")
}

// The switchover: the OAuth variables leave the profile entirely. The
// credential is a file now, and a binding left behind would keep sweeping an
// ambient CLAUDE_CODE_OAUTH_TOKEN into the guest where it outranks the
// document brig writes.
func TestClaudeNoLongerBindsTheOAuthVariables(t *testing.T) {
	reset(t)
	tmpl, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code profile is missing")
	}
	for _, b := range tmpl.Env {
		if strings.HasPrefix(b.Name, "CLAUDE_CODE_OAUTH_") {
			t.Errorf("claude-code still binds %s", b.Name)
		}
	}
	if tmpl.HostCredential != nil {
		t.Error("claude-code still carries hostCredential:, so brig would read " +
			"the Claude keychain item on every run")
	}
}

// The credential is delivered as a file, into a directory nothing can write
// through to the host. EphemeralPath is the predicate the whole design rests
// on, so it is asserted against the shipped spec and not only against
// fixtures.
func TestClaudeDeliversItsCredentialIntoATmpfs(t *testing.T) {
	reset(t)
	tmpl, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code profile is missing")
	}
	if len(tmpl.Files) != 1 || tmpl.Files[0].Path != ".claude/.credentials.json" {
		t.Fatalf("files = %+v, want the credential document", tmpl.Files)
	}
	if !tmpl.EphemeralPath(tmpl.Files[0].Path) {
		t.Errorf("%s reaches host disk", tmpl.Files[0].Path)
	}
	// --skills copies the host's own skills and plugins into the workspace at
	// these paths. Without a hostmount each the tmpfs covers the copy and the
	// flag silently does nothing, which is the failure mode this design trades
	// the old leak for.
	kept := map[string]bool{}
	for _, v := range tmpl.HostMounts() {
		kept[v.Path] = true
	}
	for _, rel := range tmpl.ProjectPaths {
		if !kept[".claude/"+rel] {
			t.Errorf("--skills copies .claude/%s into the workspace and no hostmount "+
				"keeps it, so the tmpfs hides it", rel)
		}
	}
}

// A profile name is also the image name in brig-sh/community-images, so a
// rename here that is not mirrored there breaks image resolution.
//
// The -stock suffix is the plain OCI image: the CLI and nothing else, no
// kernel and no init, because these profiles boot generically and the runtime
// supplies both. The unsuffixed name is still the bootable guest image, which
// is why the suffix has to be spelled here rather than assumed -- dropping it
// would resolve to an image carrying a second, unused kernel.
func TestPublishedImagesMatchProfileNames(t *testing.T) {
	reset(t)
	// :latest is the multi-arch index, so the same reference resolves on an
	// arm64 Mac and an amd64 Linux host. An arch-suffixed tag here would
	// silently pull the wrong architecture on one of the two.
	published := map[string]string{
		"claude-code": "ghcr.io/brig-sh/claude-code-stock:latest",
		"codex":       "ghcr.io/brig-sh/codex-stock:latest",
		"gemini":      "ghcr.io/brig-sh/gemini-stock:latest",
		"grok":        "ghcr.io/brig-sh/grok-stock:latest",
		"opencode":    "ghcr.io/brig-sh/opencode-stock:latest",
	}
	for name, image := range published {
		tmpl, ok := Lookup(name)
		if !ok {
			t.Fatalf("profile %s is missing", name)
		}
		if tmpl.Image != image {
			t.Errorf("%s image = %q, want %q", name, tmpl.Image, image)
		}
		if tmpl.Unpublished {
			t.Errorf("%s is marked unpublished but we publish it", name)
		}
	}
}

// cursor is built by community-images and deliberately never pushed. If that
// ever changes, this test is the reminder to drop the flag.
func TestCursorIsMarkedUnpublished(t *testing.T) {
	reset(t)
	tmpl, ok := Lookup("cursor")
	if !ok {
		t.Fatal("cursor profile is missing")
	}
	if !tmpl.Unpublished {
		t.Error("cursor must be marked unpublished: no image is pushed for it")
	}
}

func TestLookupAliases(t *testing.T) {
	reset(t)
	for alias, want := range map[string]string{
		"claude":      "claude-code",
		"desktop":     "claude-desktop",
		"claude-code": "claude-code",
	} {
		tmpl, ok := Lookup(alias)
		if !ok || tmpl.Name != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", alias, tmpl.Name, ok, want)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error(`Lookup("nope") succeeded`)
	}
}

func TestGuestUser(t *testing.T) {
	reset(t)
	tmpl, _ := Lookup("codex")
	if got := tmpl.GuestUser(); got != "codex" {
		t.Errorf("GuestUser() = %q, want codex", got)
	}
}

// A clone whose Required pointer is shared is a registry-wide behaviour change
// one caller can make for every later caller in the process, which is the bug
// clone exists for.
func TestCloneDoesNotShareSecretInternals(t *testing.T) {
	no := false
	p := Profile{Secrets: []SecretDecl{{Name: "s", Required: &no, Sources: []Source{{From: SourceEnv, Var: "X"}}}}}
	c := p.clone()
	*c.Secrets[0].Required = true
	c.Secrets[0].Sources[0].Var = "Y"
	if *p.Secrets[0].Required || p.Secrets[0].Sources[0].Var != "X" {
		t.Error("a clone shares the original's secret internals")
	}
}
