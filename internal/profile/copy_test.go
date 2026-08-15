package profile

import "testing"

// The registry hands out profiles by value, which copies the struct but not
// what its slices point at. Without a deep copy a caller that edits Deny is
// editing the registry's own profile for every later caller in the process --
// and brigd loads once and serves many requests. Deny is the billing guard, so
// the failure is a sandbox quietly moved onto metered API billing.
func TestLookupDoesNotHandOutTheRegistrysOwnSlices(t *testing.T) {
	reset(t)
	first, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code is missing")
	}
	if len(first.Deny) == 0 || len(first.Forward) == 0 {
		t.Fatal("this test needs a profile with both lists populated")
	}
	wantDeny := first.Deny[0]

	// What a caller filtering a list in place would do.
	first.Deny[0] = "CLOBBERED"
	first.Forward = append(first.Forward[:0], "CLOBBERED")

	second, _ := Lookup("claude-code")
	if second.Deny[0] != wantDeny {
		t.Errorf("Deny[0] = %q after a caller edited its copy, want %q", second.Deny[0], wantDeny)
	}
	if second.Forward[0] == "CLOBBERED" {
		t.Error("Forward was overwritten through a caller's copy")
	}
	if !second.Denied(wantDeny) {
		t.Errorf("%s is no longer denied, so the billing guard was edited away", wantDeny)
	}
}

// All returns copies for the same reason Lookup does, and it is the accessor
// the listing and the reserved-name check both go through.
func TestAllDoesNotHandOutTheRegistrysOwnSlices(t *testing.T) {
	reset(t)
	for _, p := range All() {
		if len(p.Deny) > 0 {
			p.Deny[0] = "CLOBBERED"
		}
	}
	for _, p := range All() {
		for _, d := range p.Deny {
			if d == "CLOBBERED" {
				t.Fatalf("%s: All() aliases the registry's deny list", p.Name)
			}
		}
	}
}
