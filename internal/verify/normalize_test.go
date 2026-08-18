package verify

import "testing"

// The registry test decides whether an image is checked at all, and NotOurs
// does not refuse -- it warns and boots unverified. So every spelling of our
// own registry that failed a byte-prefix test was a way to ask for the
// signature check to be skipped, and a profile is free to write the image
// either way.
func TestEquivalentSpellingsAreStillOurs(t *testing.T) {
	// A cosign that cannot be found, so the decision under test -- "is this
	// ours?" -- is reached without a network round trip. Anything that gets
	// past the prefix test lands on NoTooling rather than NotOurs.
	p := DefaultPolicy()
	p.Cosign = "cosign-that-does-not-exist"
	for _, ref := range []string{
		"ghcr.io/brig-sh/claude-code:latest",
		"ghcr.io:443/brig-sh/claude-code:latest",
		"GHCR.IO/brig-sh/claude-code:latest",
		"https://ghcr.io/brig-sh/claude-code:latest",
	} {
		if got := p.Image(ref); got.Outcome == NotOurs {
			t.Errorf("%s read as somebody else's image, so it would boot unverified", ref)
		}
	}
}

// The other direction: a registry that merely starts the same way is not ours,
// and must not be dressed up as ours.
func TestForeignRegistriesAreStillForeign(t *testing.T) {
	// A cosign that cannot be found, so the decision under test -- "is this
	// ours?" -- is reached without a network round trip. Anything that gets
	// past the prefix test lands on NoTooling rather than NotOurs.
	p := DefaultPolicy()
	p.Cosign = "cosign-that-does-not-exist"
	for _, ref := range []string{
		"ghcr.io.evil.example/brig-sh/claude-code:latest",
		"evil.example/ghcr.io/brig-sh/claude-code:latest",
		"ghcr.io/brig-sh-evil/claude-code:latest",
	} {
		if got := p.Image(ref); got.Outcome != NotOurs {
			t.Errorf("%s was treated as ours (outcome %v)", ref, got.Outcome)
		}
	}
}
