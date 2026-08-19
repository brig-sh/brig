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
