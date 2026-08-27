package wrap

import "testing"

// Every posture has to carry both vocabularies. They are two switches on one
// type precisely so a posture added later cannot pick up one translation and
// quietly miss the other -- the miss would be silent, because a missing case
// falls through to whatever the default arm says rather than failing.
func TestEveryPostureHasBothTranslations(t *testing.T) {
	for _, n := range AllNetworks() {
		if got := n.RuntimeNet(); got == "" {
			t.Errorf("posture %q has no runtime word", n)
		}
		if got := n.Line(); got == "" {
			t.Errorf("posture %q has no line for a reader", n)
		}
	}
	// And the parser accepts exactly the postures that exist, so the two
	// cannot drift either.
	for _, n := range AllNetworks() {
		got, err := ParseNetworkStrict(string(n), "--network")
		if err != nil || got != n {
			t.Errorf("ParseNetworkStrict(%q) = %q, %v", n, got, err)
		}
	}
}

// isolated is the posture that asks for a network of this sandbox's own.
func TestIsolatedIsItsOwnRuntimeWord(t *testing.T) {
	if NetIsolated.RuntimeNet() == NetShared.RuntimeNet() {
		t.Error("isolated and shared ask the runtime for the same thing, so no adapter can tell them apart")
	}
	if NetOffline.RuntimeNet() != "none" {
		t.Errorf("offline runtime word = %q, want none", NetOffline.RuntimeNet())
	}
}
