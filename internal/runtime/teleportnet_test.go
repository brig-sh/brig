package runtime

import (
	"strings"
	"testing"
)

// A sandbox arriving from another host takes exactly the slot it had there,
// because its guest still holds that address. A slot another sandbox holds is
// refused, never shared, and a second claim of its own slot is a no-op.
func TestClaimTakesExactlyTheSlotTheGuestHolds(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	alloc, err := isolatedNets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alloc.slot("brig-a"); err != nil { // takes 0
		t.Fatal(err)
	}
	if err := alloc.claim("brig-moved", 5); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookupSandboxNet("brig-moved"); !ok || got != 5 {
		t.Errorf("brig-moved holds %d, %v; want 5", got, ok)
	}
	if err := alloc.claim("brig-moved", 5); err != nil {
		t.Errorf("claiming its own slot again: %v", err)
	}
	err = alloc.claim("brig-other", 0)
	if err == nil || !strings.Contains(err.Error(), "brig-a") {
		t.Errorf("a slot brig-a holds was claimed: %v", err)
	}
	if err := alloc.claim("brig-x", sandboxNets); err == nil {
		t.Errorf("a slot outside the space was claimed")
	}
	if holder, _ := AddressHolder("isolated", 0, "brig-moved"); holder != "brig-a" {
		t.Errorf("holder of 0 = %q", holder)
	}
	if holder, _ := AddressHolder("isolated", 5, "brig-moved"); holder != "" {
		t.Errorf("a sandbox's own slot named a holder: %q", holder)
	}
	if _, err := AddressHolder("bridged", 1, "x"); err == nil {
		t.Errorf("an unknown network word was answered")
	}
}

// What the source reads about a sandbox's address is what its records say,
// and a sandbox with no posture recorded cannot be moved: its address is in
// its memory and nowhere else.
func TestSandboxAddressReadsTheRecords(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	if _, _, ok := SandboxAddress("brig-a"); ok {
		t.Errorf("an unrecorded sandbox has an address")
	}
	if _, err := sandboxNet("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := SandboxAddress("brig-a"); ok {
		t.Errorf("a sandbox with no posture recorded has an address")
	}
	if err := RecordBootedNet("brig-a", "isolated"); err != nil {
		t.Fatal(err)
	}
	if net, slot, ok := SandboxAddress("brig-a"); !ok || net != "isolated" || slot != 0 {
		t.Errorf("got %s %d %v", net, slot, ok)
	}
	if err := RecordBootedNet("brig-off", "none"); err != nil {
		t.Fatal(err)
	}
	if net, _, ok := SandboxAddress("brig-off"); !ok || net != "none" {
		t.Errorf("an offline sandbox: %s %v", net, ok)
	}
}

// Receiving an offline sandbox records its posture and starts nothing, and
// releasing it forgets the posture.
func TestReceiveNetworkForAGuestWithNoNetwork(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	sock, err := ReceiveNetwork("/nonexistent/hull", "brig-off", "none", 0, Egress{})
	if err != nil || sock != "" {
		t.Fatalf("got %q, %v", sock, err)
	}
	if net, _ := BootedNet("brig-off"); net != "none" {
		t.Errorf("posture %q", net)
	}
	ReleaseReceivedNetwork("brig-off")
	if net, _ := BootedNet("brig-off"); net != "" {
		t.Errorf("posture after release %q", net)
	}
}

// A claim that cannot be had leaves nothing behind: no gateway is started for
// a sandbox whose slot another holds.
func TestReceiveNetworkRefusesATakenSlot(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	if _, err := sandboxNet("brig-a"); err != nil {
		t.Fatal(err)
	}
	_, err := ReceiveNetwork("/nonexistent/hull", "brig-moved", "isolated", 0, Egress{})
	if err == nil || !strings.Contains(err.Error(), "brig-a") {
		t.Fatalf("got %v", err)
	}
	if net, _ := BootedNet("brig-moved"); net != "" {
		t.Errorf("a refused arrival recorded posture %q", net)
	}
}
