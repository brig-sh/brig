package runtime

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchIsolatedDir points the isolated sockets -- and the network map beside
// -- at a directory this test owns.
//
// Not t.TempDir(), which names the directory after the test: on macOS that
// alone can push a socket path past what one fits in, and a test would be
// exercising the shortening rather than what it is about.
func scratchIsolatedDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("BRIG_GATEWAY_DIR", dir)
}

// Two sandboxes must not land on one network, and a sandbox must keep its own
// across restarts: hull configures the guest statically, so a changed address
// is a changed machine.
func TestSandboxNetIsStablePerSandboxAndDistinctAcross(t *testing.T) {
	scratchIsolatedDir(t)

	first, err := sandboxNet("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sandboxNet("brig-plain-ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two sandboxes were given the same network: %d", first)
	}
	again, err := sandboxNet("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("a sandbox's network moved between calls: %d then %d", first, again)
	}
}

// The whole point of a network per sandbox: no two sandboxes share one, so no
// sandbox is on a network another can address at all.
func TestSandboxNetworksDoNotOverlap(t *testing.T) {
	seen := map[netip.Addr]string{}
	for index := range sandboxNets {
		subnet, err := netip.ParsePrefix(sandboxSubnet(index))
		if err != nil {
			t.Fatalf("network %d has an unparseable subnet %q: %v", index, sandboxSubnet(index), err)
		}
		guest, err := netip.ParsePrefix(sandboxCIDR(index))
		if err != nil {
			t.Fatalf("network %d has an unparseable guest CIDR: %v", index, err)
		}
		gateway, err := netip.ParseAddr(sandboxGatewayIP(index))
		if err != nil {
			t.Fatalf("network %d has an unparseable gateway address: %v", index, err)
		}
		for _, addr := range []netip.Addr{subnet.Addr(), guest.Addr(), gateway} {
			if other, ok := seen[addr]; ok {
				t.Fatalf("%s is on network %s and on network %d", addr, other, index)
			}
			seen[addr] = sandboxSubnet(index)
		}
		if !subnet.Contains(guest.Addr()) || !subnet.Contains(gateway) {
			t.Fatalf("network %d puts its members outside its own subnet: %s has %s and %s",
				index, subnet, guest, gateway)
		}
		if guest.Addr() == gateway {
			t.Fatalf("network %d hands the guest the gateway's own address: %s", index, gateway)
		}
	}
}

// hull is given the guest's CIDR and derives the gateway from it by adding one
// to the network address. brig passes that same gateway to the gateway
// process, so the two derivations have to agree or the guest routes to an
// address nothing is listening on.
func TestGatewayAddressMatchesHullsDerivation(t *testing.T) {
	for index := range sandboxNets {
		guest, err := netip.ParsePrefix(sandboxCIDR(index))
		if err != nil {
			t.Fatal(err)
		}
		// What hull does: mask to the network, then add one.
		hullsGateway := guest.Masked().Addr().Next()
		if got := sandboxGatewayIP(index); got != hullsGateway.String() {
			t.Fatalf("network %d: brig starts the gateway on %s, hull routes the guest to %s",
				index, got, hullsGateway)
		}
	}
}

// Every isolated network stays inside the one /24 claimed for them, so brig's
// claim on the host's address space is two adjacent /24s and not whatever the
// arithmetic happens to produce.
func TestEverySandboxNetworkFitsTheClaimedSpace(t *testing.T) {
	space, err := netip.ParsePrefix(isolatedSpace)
	if err != nil {
		t.Fatal(err)
	}
	for index := range sandboxNets {
		subnet, err := netip.ParsePrefix(sandboxSubnet(index))
		if err != nil {
			t.Fatal(err)
		}
		if !space.Contains(subnet.Addr()) {
			t.Fatalf("network %d (%s) is outside %s", index, subnet, space)
		}
		if subnet.Bits() != sandboxNetBits {
			t.Fatalf("network %d carries the wrong prefix: %s", index, subnet)
		}
	}
	// And the space holds exactly the number of them claimed, so the two
	// constants cannot drift into a sandbox addressed outside the space.
	if got := 1 << (32 - space.Bits()) / sandboxBlockSize; got != sandboxNets {
		t.Errorf("%s holds %d networks of %d addresses, but sandboxNets is %d",
			space, got, sandboxBlockSize, sandboxNets)
	}
}

func TestReleaseSandboxNetFreesIt(t *testing.T) {
	scratchIsolatedDir(t)

	first, err := sandboxNet("gone")
	if err != nil {
		t.Fatal(err)
	}
	releaseSandboxNet("gone")

	// The lowest free network is the one just released, so a new sandbox
	// takes it rather than the map growing without bound.
	reused, err := sandboxNet("new")
	if err != nil {
		t.Fatal(err)
	}
	if reused != first {
		t.Errorf("released network was not reused: freed %d, then handed out %d", first, reused)
	}
}

// A corrupt map must not stop a sandbox booting: the networks are derivable
// again and the file is a cache, not a record of record.
func TestSandboxNetSurvivesACorruptStore(t *testing.T) {
	scratchIsolatedDir(t)

	alloc, err := isolatedNets()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(alloc.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alloc.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := sandboxNet("s"); err != nil {
		t.Fatalf("a corrupt network map must not fail a boot: %v", err)
	}
}

// The shared network's map holds host numbers on one /24. Read as network
// indexes they would put a sandbox somewhere nobody assigned it, so the two
// are different files and this one does not consult the other.
func TestSandboxNetIgnoresTheSharedAddressMap(t *testing.T) {
	scratchIsolatedDir(t)

	dir, err := gatewayDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "gateway-ips.json")
	if err := os.WriteFile(old, []byte(`{"s":200}`), 0o600); err != nil {
		t.Fatal(err)
	}

	index, err := sandboxNet("s")
	if err != nil {
		t.Fatal(err)
	}
	if index != 0 {
		t.Errorf("the shared address map was read: got network %d, want the first free one", index)
	}
}

func TestLowestFreeNetReportsAFullSpace(t *testing.T) {
	assigned := map[string]int{}
	for index := range sandboxNets {
		assigned[strings.Repeat("s", index+1)] = index
	}
	alloc, err := isolatedNets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alloc.lowestFree(assigned); err == nil {
		t.Fatal("expected a full address space to be reported")
	} else if !strings.Contains(err.Error(), "brig rm") {
		t.Errorf("error does not say how to free one: %v", err)
	}
}
