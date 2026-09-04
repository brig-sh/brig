package runtime

import (
	"strings"
	"testing"
)

// --network isolated was accepted and did nothing on the backends where brig
// owns no network, while the envelope reported a network of the sandbox's own.
// The same silent gap this posture closes on hvi, left open on the backend most
// runs use.
func TestIsolatedIsRefusedWhereBrigOwnsNoNetwork(t *testing.T) {
	spec := RunSpec{Name: "brig-s", Image: "img", Net: "isolated"}

	for _, hv := range []string{"vz", "qemu"} {
		err := supports(spec, hv)
		if err == nil {
			t.Fatalf("--network isolated was accepted on %s, which cannot give one", hv)
		}
		if !strings.Contains(err.Error(), "hvi") {
			t.Errorf("the error does not name the backend that can: %v", err)
		}
	}
	if err := supports(spec, "hvi"); err != nil {
		t.Errorf("isolated was refused on the backend that implements it: %v", err)
	}
	// The shared and offline postures are unaffected everywhere.
	for _, net := range []string{"", "shared", "none"} {
		for _, hv := range []string{"vz", "hvi", "qemu"} {
			if err := supports(RunSpec{Name: "brig-s", Image: "img", Net: net}, hv); err != nil {
				t.Errorf("posture %q was refused on %s: %v", net, hv, err)
			}
		}
	}
}

// The refusal is asked through an interface, so it can run on the path that
// boots nothing. A check living only inside Run would refuse the posture on the
// first `brig run` and wave it past on the second.
func TestHullAnswersCanRun(t *testing.T) {
	var _ RunChecker = &hull{}

	h := &hull{bin: "hull"}
	isolated := RunSpec{Name: "brig-s", Image: "img", Net: "isolated"}
	if err := h.CanRun(isolated); err == nil {
		t.Error("isolated was accepted on the default backend, which cannot give one")
	}
	isolated.Hypervisor = "hvi"
	if err := h.CanRun(isolated); err != nil {
		t.Errorf("isolated was refused on the backend that implements it: %v", err)
	}
}
