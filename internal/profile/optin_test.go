package profile

import "testing"

// The refresh token is the one credential in these profiles that is not
// short-lived: it mints new access tokens for as long as it lives, so a
// sandbox holding one holds the account rather than the session. It stays in
// the spec, because the refresh-token pair is how a headless flow provisions
// auth, and it stays marked so nothing forwards it without being asked.
func TestTheClaudeSpecsMarkTheRefreshTokenOptIn(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code", "claude-desktop"} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("no profile %q", name)
		}
		var found bool
		for _, b := range p.Env {
			switch b.Name {
			case "CLAUDE_CODE_OAUTH_REFRESH_TOKEN":
				found = true
				if !b.OptIn {
					t.Errorf("%s forwards a refresh token by default", name)
				}
			case "CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN":
				// The short-lived credentials are unchanged: this is about
				// which of them a zero-config run hands over, not about
				// making the profile ask for everything.
				if b.OptIn {
					t.Errorf("%s made %s opt-in, which breaks the default login", name, b.Name)
				}
			}
		}
		if !found {
			t.Errorf("%s no longer offers the refresh token at all, so a headless "+
				"login has no way to provision one", name)
		}
	}
}

// A denied variable and an opt-in one are different things, and conflating
// them would put a metered API key one BRIG_ALLOW_DENIED=1 away from the same
// switch that forwards a refresh token.
func TestOptInIsNotTheDenylist(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	p, _ := Lookup("claude-code")
	for _, want := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if !p.Denied(want) {
			t.Errorf("%s is no longer denied", want)
		}
	}
	if p.Denied("CLAUDE_CODE_OAUTH_REFRESH_TOKEN") {
		t.Error("the refresh token was put on the billing denylist, which is not what it is")
	}
}

// BuiltInDeny is what a report compares a file-backed override against, so it
// has to read the shipped spec rather than the registry the override replaced.
func TestBuiltInDenyReadsTheShippedSpec(t *testing.T) {
	if got := BuiltInDeny("claude-code"); len(got) == 0 {
		t.Error("the built-in claude-code denylist reads as empty")
	}
	if got := BuiltInDeny("no-such-profile"); got != nil {
		t.Errorf("BuiltInDeny invented a list for a profile brig does not ship: %v", got)
	}
}
