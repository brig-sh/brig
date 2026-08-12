package agent

import "testing"

// The denylist is the billing guard: a variable that outranks the
// subscription credential must never be in the forward list, or a sandbox
// moves onto metered API billing without saying so. The Homebrew formula
// asserted this too, and it is the one invariant a careless edit to the table
// above would break silently.
func TestDeniedVarsAreNotForwarded(t *testing.T) {
	for _, tmpl := range All() {
		for _, denied := range tmpl.Deny {
			for _, fwd := range tmpl.Forward {
				if fwd == denied {
					t.Errorf("%s forwards %s, which is on its own denylist", tmpl.Name, denied)
				}
			}
			if !tmpl.Denied(denied) {
				t.Errorf("%s: Denied(%q) = false", tmpl.Name, denied)
			}
		}
	}
}

// Claude Code's own precedence puts both of these ahead of the OAuth token.
func TestClaudeDeniesTheMeteredKeys(t *testing.T) {
	tmpl, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code template is missing")
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if !tmpl.Denied(want) {
			t.Errorf("claude-code does not deny %s", want)
		}
	}
	for _, want := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		"GH_TOKEN",
	} {
		found := false
		for _, f := range tmpl.Forward {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("claude-code no longer forwards %s", want)
		}
	}
}

// A template name is also the image name in brig-sh/community-images, so a
// rename here that is not mirrored there breaks image resolution.
func TestPublishedImagesMatchTemplateNames(t *testing.T) {
	// :latest is the multi-arch index, so the same reference resolves on an
	// arm64 Mac and an amd64 Linux host. An arch-suffixed tag here would
	// silently pull the wrong architecture on one of the two.
	published := map[string]string{
		"claude-code": "ghcr.io/brig-sh/claude-code:latest",
		"codex":       "ghcr.io/brig-sh/codex:latest",
		"gemini":      "ghcr.io/brig-sh/gemini:latest",
		"grok":        "ghcr.io/brig-sh/grok:latest",
		"opencode":    "ghcr.io/brig-sh/opencode:latest",
	}
	for name, image := range published {
		tmpl, ok := Lookup(name)
		if !ok {
			t.Fatalf("template %s is missing", name)
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
	tmpl, ok := Lookup("cursor")
	if !ok {
		t.Fatal("cursor template is missing")
	}
	if !tmpl.Unpublished {
		t.Error("cursor must be marked unpublished: no image is pushed for it")
	}
}

func TestLookupAliases(t *testing.T) {
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
	tmpl, _ := Lookup("codex")
	if got := tmpl.GuestUser(); got != "codex" {
		t.Errorf("GuestUser() = %q, want codex", got)
	}
}
