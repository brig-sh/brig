package secret

import (
	"strings"
	"testing"
)

// The name is a keychain account, appears in error messages, and becomes
// `ref: secrets.<name>` in a profile. A dot would make that reference
// ambiguous, so it is refused here rather than migrated away from later.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"gh", "GH_TOKEN", "gh-token", "gh_token", "a1", "A"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "_lead", "1", "a.b", "a b", "a/b", "a:b", "é"} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", bad)
		}
	}
}

// A leading digit is refused, but a digit anywhere else is fine.
func TestValidNameLength(t *testing.T) {
	long := strings.Repeat("a", 129)
	if err := ValidName(long); err == nil {
		t.Error("a 129-character name was accepted")
	}
	if err := ValidName(long[:128]); err != nil {
		t.Errorf("a 128-character name was refused: %v", err)
	}
}
