package runtime

import (
	"strings"
	"testing"
)

// A policy on a container runtime was once dropped in silence, and printed a
// POLICY row over a sandbox with unrestricted egress. nerdctl now enforces
// one. docker, which the same adapter drives, still does not, and must refuse.
func TestDockerRefusesAPolicyItCannotEnforce(t *testing.T) {
	spec := RunSpec{Name: "brig-s", Image: "img", Net: "isolated",
		Egress: Egress{Default: "deny", Allow: []Rule{{Host: "example.com"}}}}

	if err := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).CanRun(spec); err != nil {
		t.Errorf("nerdctl refused a policy it enforces: %v", err)
	}

	d := &nerdctl{bin: "/usr/local/bin/docker"}
	err := d.CanRun(spec)
	if err == nil {
		t.Fatal("docker accepted a policy it cannot enforce")
	}
	if !strings.Contains(err.Error(), "enforce") {
		t.Errorf("the error does not say the rules would not be enforced: %v", err)
	}
	// Run refuses it too, so a caller that never asks CanRun is still covered.
	if err := d.Run(spec); err == nil {
		t.Error("Run booted a docker sandbox whose policy it cannot enforce")
	}

	// An offline sandbox reaches nothing, which satisfies every rule set.
	spec.Net = "none"
	if err := d.CanRun(spec); err != nil {
		t.Errorf("an offline sandbox with a policy was refused: %v", err)
	}
	// And an ordinary run is untouched.
	if err := d.CanRun(RunSpec{Name: "brig-s", Image: "img", Net: "shared"}); err != nil {
		t.Errorf("a sandbox with no policy was refused: %v", err)
	}
}

// Both adapters answer the same question through the same interface, so the
// check can run on the path that boots nothing. A backend that gained a
// refusal inside Run alone would be a backend where an already-running sandbox
// is waved through.
func TestBothAdaptersAnswerCanRun(t *testing.T) {
	var _ RunChecker = &hull{}
	var _ RunChecker = &nerdctl{}

	policied := RunSpec{Name: "brig-s", Image: "img", Net: "shared",
		Egress: Egress{Default: "deny"}}
	for _, tt := range []struct {
		name string
		rt   RunChecker
	}{
		{"hull on vz", &hull{bin: "hull"}},
		{"docker", &nerdctl{bin: "docker"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.rt.CanRun(policied); err == nil {
				t.Error("a policy this backend cannot enforce was accepted")
			}
		})
	}
	// nerdctl and hull on hvi can, and must not refuse it.
	if err := (&nerdctl{bin: "nerdctl"}).CanRun(policied); err != nil {
		t.Errorf("nerdctl refused a policy it can enforce: %v", err)
	}
	policied.Hypervisor = "hvi"
	if err := (&hull{bin: "hull"}).CanRun(policied); err != nil {
		t.Errorf("hvi refused a policy it can enforce: %v", err)
	}
}

// --network isolated was accepted and did nothing on the backends where brig
// owns no network, while the envelope reported a network of the sandbox's own.
// The same silent gap the isolated posture closes on hvi, left open on the
// backend most runs use.
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
		// The other way out has to be one that keeps sandboxes apart. The
		// error once sent the user to the shared network, and on hvi two
		// sandboxes there reach each other.
		if !strings.Contains(err.Error(), "separate hosts") {
			t.Errorf("the error does not name separate hosts as the other way out: %v", err)
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
