package routemode

import (
	"slices"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

func TestRegisterServesAModeUntilItIsRemoved(t *testing.T) {
	if Served("relay") {
		t.Fatal("relay is served before it is registered")
	}
	unregister := Register(Mode{Name: "relay", Presets: []v1alpha1.Preset{v1alpha1.PresetClaudeCode}})
	if !Served("relay") || !slices.Contains(Names(), "relay") || !slices.IsSorted(Names()) {
		t.Errorf("Served = %v, Names = %v", Served("relay"), Names())
	}
	if m, ok := Lookup("relay"); !ok || !slices.Equal(m.Presets, []v1alpha1.Preset{v1alpha1.PresetClaudeCode}) {
		t.Errorf("Lookup = %+v, %v", m, ok)
	}
	unregister()
	if Served("relay") || slices.Contains(Names(), "relay") {
		t.Error("relay is served after it was removed")
	}
	for _, m := range []v1alpha1.RouteMode{"", v1alpha1.ModeEndpoint} {
		if !Served(m) {
			t.Errorf("%q is not served", m)
		}
	}
	if got := NotInBuild("relay"); got != "route mode relay is not part of this build of brig" {
		t.Errorf("NotInBuild = %q", got)
	}
}

func TestRegisterRefusesWhatIsNotAMode(t *testing.T) {
	defer Register(Mode{Name: "relay"})()
	for _, name := range []v1alpha1.RouteMode{"", v1alpha1.ModeEndpoint, "Relay", "relay"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", name)
				}
			}()
			Register(Mode{Name: name})
		}()
	}
}
