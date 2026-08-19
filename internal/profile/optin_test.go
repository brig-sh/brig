package profile

import (
	"strings"
	"testing"
)

// The Claude profiles no longer carry the refresh token as an environment
// binding at all. They deliver the whole credential document as a file into a
// tmpfs (see volumes: and files: in the specs), which is what lets the agent
// refresh in place without the token ever reaching host disk -- so there is no
// env binding left for optIn: to hold back.
//
// optIn: itself is unchanged and still applies to any profile that binds a
// durable credential as a variable; TestOptInHoldsABindingBack covers the
// mechanism against a fixture rather than against a shipped spec.
func TestTheClaudeSpecsDeliverTheCredentialAsAFile(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code", "claude-desktop"} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("no profile %q", name)
		}
		for _, b := range p.Env {
			if strings.HasPrefix(b.Name, "CLAUDE_CODE_OAUTH") {
				t.Errorf("%s still binds %s as a variable; the credential travels "+
					"as a file now", name, b.Name)
			}
		}
		if len(p.Files) == 0 {
			t.Errorf("%s delivers no credential file", name)
		}
		var covered bool
		for _, v := range p.Volumes {
			if v.Kind == VolumeTmpfs {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s writes a credential without covering the directory it "+
				"lands in, so it would reach host disk", name)
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
