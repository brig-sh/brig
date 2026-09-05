package runtime

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchGatewayDir points the socket -- and so the address map beside it --
// at a directory this test owns.
func scratchGatewayDir(t *testing.T) {
	t.Helper()
	t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(t.TempDir(), "gateway.sock"))
}

// Two sandboxes on one virtual network must not share an address, and a
// sandbox must keep its own across restarts: hull configures the guest
// statically, so a changed address is a changed machine.
func TestGatewayCIDRIsStablePerSandboxAndDistinctAcross(t *testing.T) {
	scratchGatewayDir(t)

	first, err := gatewayCIDR("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	second, err := gatewayCIDR("brig-plain-ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two sandboxes were given the same address: %s", first)
	}
	again, err := gatewayCIDR("brig-claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("a sandbox's address moved between calls: %s then %s", first, again)
	}
}

// The gateway is .1 on the subnet, so no guest may be given it.
func TestGatewayCIDRNeverHandsOutTheGatewayAddress(t *testing.T) {
	scratchGatewayDir(t)

	for i := 0; i < 20; i++ {
		got, err := gatewayCIDR(strings.Repeat("s", i+1))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(got, gatewayAddr+"/") {
			t.Fatalf("handed out the gateway's own address: %s", got)
		}
		if !strings.HasSuffix(got, "/24") {
			t.Fatalf("address carries the wrong prefix: %s", got)
		}
	}
}

func TestReleaseGatewayIPFreesTheAddress(t *testing.T) {
	scratchGatewayDir(t)

	first, err := gatewayCIDR("gone")
	if err != nil {
		t.Fatal(err)
	}
	releaseGatewayIP("gone")

	// The lowest free address is the one just released, so a new sandbox
	// takes it rather than the map growing without bound.
	reused, err := gatewayCIDR("new")
	if err != nil {
		t.Fatal(err)
	}
	if reused != first {
		t.Errorf("released address was not reused: freed %s, then handed out %s", first, reused)
	}
}

// A corrupt map must not stop a sandbox booting: the addresses are derivable
// again and the file is a cache, not a record of record.
func TestGatewayCIDRSurvivesACorruptStore(t *testing.T) {
	scratchGatewayDir(t)

	path, err := gatewayIPStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := gatewayCIDR("s")
	if err != nil {
		t.Fatalf("a corrupt address map must not fail a boot: %v", err)
	}
	if got == "" {
		t.Fatal("no address returned")
	}
}

func TestLowestFreeHostReportsAFullNetwork(t *testing.T) {
	assigned := map[string]int{}
	for host := firstGuestHost; host <= lastGuestHost; host++ {
		assigned[string(rune(host))] = host
	}
	if _, err := lowestFreeHost(assigned); err == nil {
		t.Fatal("expected a full network to be reported")
	} else if !strings.Contains(err.Error(), "brig rm") {
		t.Errorf("error does not say how to free one: %v", err)
	}
}

// The addresses handed out must come from the subnet the gateway is told to
// serve. They were two independent literals, so moving one and not the other
// gave every guest an address on a network nothing was routing -- the failure
// the subnet comment warns about, and one that shows up as "the sandbox has no
// network" rather than as anything pointing here.
func TestHandedOutAddressesSitInTheServedSubnet(t *testing.T) {
	net, err := netip.ParsePrefix(gatewaySubnet)
	if err != nil {
		t.Fatalf("gatewaySubnet %q does not parse: %v", gatewaySubnet, err)
	}
	for _, host := range []int{firstGuestHost, 17, lastGuestHost} {
		cidr := formatGatewayCIDR(host)
		addr, err := netip.ParsePrefix(cidr)
		if err != nil {
			t.Fatalf("formatGatewayCIDR(%d) = %q, which does not parse: %v", host, cidr, err)
		}
		if !net.Contains(addr.Addr()) {
			t.Errorf("guest address %s is outside the served subnet %s", cidr, gatewaySubnet)
		}
		if addr.Bits() != net.Bits() {
			t.Errorf("guest address %s has prefix /%d, the subnet is /%d", cidr, addr.Bits(), net.Bits())
		}
	}
	// The gateway's own address has to be on it too, and hull derives it as
	// the network address plus one.
	gw, err := netip.ParseAddr(gatewayAddr)
	if err != nil || !net.Contains(gw) {
		t.Errorf("gateway address %s is not in %s (%v)", gatewayAddr, gatewaySubnet, err)
	}
}
