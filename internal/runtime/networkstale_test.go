package runtime

import (
	"os"
	"testing"
)

// The one path that skips the boot is the one where a stale policy would go
// unnoticed: brig finds the sandbox running, returns, and prints a POLICY row
// for rules that sandbox is not under. Rules are fixed when a gateway starts,
// so the only honest answer is to restart it.
func TestNetworkStale(t *testing.T) {
	deny := Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}}}
	wider := Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}, {Host: "b.example"}}}

	for _, tt := range []struct {
		name string
		// booted is what the running sandbox's gateway was started to serve;
		// nil means it has none, which is the shared network.
		booted *Egress
		asked  Egress
		net    string
		want   bool
	}{
		{"shared then shared", nil, Egress{}, "shared", false},
		{"shared, then a policy attached", nil, deny, "shared", true},
		{"the same policy", &deny, deny, "shared", false},
		{"the policy edited", &deny, wider, "shared", true},
		{"the policy detached", &deny, Egress{}, "shared", true},
		{"isolated with no policy, unchanged", &Egress{}, Egress{}, "isolated", false},
		{"shared, then isolated asked for", nil, Egress{}, "isolated", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scratchIsolatedDir(t)
			const name = "brig-s"
			if tt.booted != nil {
				sock := mustSocket(t, name)
				index, err := sandboxNet(name)
				if err != nil {
					t.Fatal(err)
				}
				listenAt(t, sock)
				writeGatewayRecord(sock, os.Getpid(), gatewaySpec(index, *tt.booted))
			}
			h := &hull{bin: "hull"}
			if got := h.NetworkStale(name, "hvi", tt.net, tt.asked); got != tt.want {
				t.Errorf("NetworkStale = %t, want %t", got, tt.want)
			}
		})
	}
}

// Only hvi has a gateway brig owns. On vz the network comes from vmnet, which
// brig neither chose nor can inspect, so it has nothing to compare and must
// not answer "stale" -- that would restart a healthy sandbox on every run.
func TestNetworkStaleSaysNoWhereBrigOwnsNoNetwork(t *testing.T) {
	scratchIsolatedDir(t)
	h := &hull{bin: "hull"}

	for _, hv := range []string{"vz", "qemu", ""} {
		if h.NetworkStale("brig-s", hv, "shared", Egress{Default: "deny"}) {
			t.Errorf("hypervisor %q: reported stale with no network of brig's to compare", hv)
		}
	}
	// And an offline sandbox has no network either way.
	if h.NetworkStale("brig-s", "hvi", "none", Egress{Default: "deny"}) {
		t.Error("an offline sandbox was reported stale")
	}
}

// A sandbox whose gateway died is not "running under the wrong policy" -- it
// has no gateway at all, and the run that follows starts one. What matters is
// that this does not silently read the dead gateway's record as current.
func TestNetworkStaleIgnoresARecordWithNothingListening(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-s"
	sock := mustSocket(t, name)
	index, err := sandboxNet(name)
	if err != nil {
		t.Fatal(err)
	}
	// A record, but nothing answering on the socket.
	writeGatewayRecord(sock, os.Getpid(), gatewaySpec(index, Egress{Default: "deny"}))

	h := &hull{bin: "hull"}
	if h.NetworkStale(name, "hvi", "shared", Egress{}) {
		t.Error("a dead gateway's record was read as the sandbox's current policy")
	}
}

// An offline sandbox reaches nothing, which satisfies every rule set. Refusing
// one for carrying a policy would be refusing the stricter posture for not
// being the weaker one.
func TestSupportsAllowsAPolicyOnAnOfflineSandbox(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", Net: "none", Egress: Egress{Default: "deny"}}
	for _, hv := range []string{"vz", "hvi", "qemu"} {
		if err := supports(spec, hv); err != nil {
			t.Errorf("an offline sandbox with a policy was refused on %s: %v", hv, err)
		}
	}
	// With a network, the refusal stands.
	spec.Net = "shared"
	if err := supports(spec, "vz"); err == nil {
		t.Error("a networked sandbox with a policy was accepted on vz")
	}
}
