package profile

import "testing"

// No profile brig ships binds a policy, and none should.
//
// This is the default-open guarantee, held where it can actually be broken. A
// policy is opt-in: `brig run claude` on a fresh install has unrestricted
// egress, the same as before policies existed, and a sandbox only ever answers
// to rules someone attached. One `policy:` line added to a shipped profile
// would silently make every run of it filtered -- and under a `deny` default,
// every run of it broken -- for everyone who upgraded.
//
// If a profile ever should ship with one, this test is the place that argument
// gets made rather than a line nobody notices in a spec file.
func TestNoShippedProfileBindsAPolicy(t *testing.T) {
	for _, p := range All() {
		if len(p.Policy) != 0 {
			t.Errorf("the shipped profile %q binds %v. A shipped policy makes every run of "+
				"that profile filtered for everyone who upgrades; policies are opt-in",
				p.Name, p.Policy)
		}
	}
}
