package verify

import (
	"regexp"
	"testing"
)

// The identity is handed to cosign as --certificate-identity-regexp, so it is
// matched as a regexp and not as a prefix: an unanchored one accepts anything
// that merely starts the right way, and an unescaped dot accepts any character
// in its place. Both mistakes are the same mistake in practice -- a
// certificate minted by a workflow run brig-sh never published becomes one the
// shipped verifier accepts -- so this pins the exact strings that must not
// match.
func TestDefaultIdentityAcceptsOnlyTheMainBranchWorkflow(t *testing.T) {
	re, err := regexp.Compile(DefaultPolicy().Identity)
	if err != nil {
		t.Fatalf("the shipped identity is not a valid regexp: %v", err)
	}

	const legitimate = "https://github.com/brig-sh/community-images/" +
		".github/workflows/build-images.yml@refs/heads/main"
	if !re.MatchString(legitimate) {
		t.Errorf("the identity rejects the workflow brig-sh actually publishes from:\n  %s", legitimate)
	}

	// Every one of these is a certificate somebody other than a
	// community-images maintainer on main can obtain: a branch push, a pull
	// request from a fork, another repository whose name happens to start the
	// same way, or a lookalike that only an unescaped dot lets through.
	for _, identity := range []string{
		"https://github.com/brig-sh/community-images/.github/workflows/build-images.yml@refs/heads/attacker-branch",
		"https://github.com/brig-sh/community-images/.github/workflows/build-images.yml@refs/pull/7/merge",
		"https://github.com/brig-sh/community-images/.github/workflows/build-images0yml@refs/heads/main",
		"https://githubXcom/brig-sh/community-images/.github/workflows/build-images.yml@refs/heads/main",
		"https://github.com/brig-sh/community-images/Xgithub/workflows/build-images.yml@refs/heads/main",
		"https://github.com/brig-sh/community-images/.github/workflows/build-images.yml@refs/heads/main-evil",
		"https://github.com/brig-sh/community-images/.github/workflows/build-images.yml@refs/tags/v1",
		"https://evil.example/github.com/brig-sh/community-images/" +
			".github/workflows/build-images.yml@refs/heads/main",
	} {
		if re.MatchString(identity) {
			t.Errorf("the identity accepts a certificate brig-sh never published:\n  %s", identity)
		}
	}
}
