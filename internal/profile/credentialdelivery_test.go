package profile

import (
	"strings"
	"testing"
)

// The Claude profiles carry no OAuth credential as an environment binding.
// They deliver the whole credential document as a file into a tmpfs (see
// volumes: and files: in the specs), which is what lets the agent refresh it
// in place without the token reaching host disk.
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

// The tmpfs covering .claude is what keeps the credential off host disk, and
// it takes everything else under that directory with it. What the user owns
// -- the permission allowlist and the user-level memory -- has to come back
// through a hostmount, or the agent re-asks for permissions it was already
// granted on every boot. The credential itself must NOT: it is re-delivered
// from the store each boot, and a hostmount would put it on host disk, which
// is the whole thing this design is for.
func TestUserConfigSurvivesButTheCredentialDoesNot(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code", "claude-desktop"} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("no profile %q", name)
		}
		for _, keep := range []string{".claude/settings.json", ".claude/CLAUDE.md"} {
			if p.EphemeralPath(keep) {
				t.Errorf("%s: %s is ephemeral; the user's own configuration is lost on every stop", name, keep)
			}
		}
		for _, b := range p.Files {
			if !p.EphemeralPath(b.Path) {
				t.Errorf("%s: the credential at %s persists, so it reaches host disk", name, b.Path)
			}
		}
	}
}

// A tmpfs is a ceiling the guest meets as ENOSPC from its own tools, so the
// profiles covering an agent's whole home say what that ceiling is rather
// than taking a default sized for a configuration directory.
func TestTheClaudeProfilesSizeTheirTmpfs(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code", "claude-desktop"} {
		p, _ := Lookup(name)
		for _, v := range p.Tmpfs() {
			if v.Size == "" {
				t.Errorf("%s: tmpfs %q takes the %s default; an agent home needs its own size:",
					name, v.Path, defaultTmpfsSize)
			}
		}
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
