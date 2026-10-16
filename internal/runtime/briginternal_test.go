package runtime

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// brig.internal on hull is a --host record of the sandbox gateway's own
// address and a divert of its port 443 to the broker's socket. The gateway
// spec holds both, so a gateway started without them is replaced.
func TestGatewayArgsRouteBrigInternalToTheGatewayAddress(t *testing.T) {
	gw := netip.MustParseAddr(sandboxGatewayIP(3))
	e := Egress{Divert: Divert{Routes: []DivertRoute{{Host: "api.anthropic.com", Port: 443}}, Socket: "/b/divert/x.sock",
		BrigInternal: true}}
	want := "--divert api.anthropic.com:443=/b/divert/x.sock --divert " + gw.String() + ":443=/b/divert/x.sock " +
		"--host brig.internal=" + gw.String()
	if got := strings.Join(e.divertArgs(gw), " "); got != want {
		t.Errorf("divertArgs = %q, want %q", got, want)
	}
	alone := Egress{Divert: Divert{Socket: "/b/divert/x.sock", BrigInternal: true}}
	if got := strings.Join(alone.divertArgs(gw), " "); got != "--divert "+gw.String()+":443=/b/divert/x.sock --host brig.internal="+gw.String() {
		t.Errorf("divertArgs = %q", got)
	}
	if !isolatedNet("shared", alone) {
		t.Error("brig.internal does not take a gateway of the sandbox's own")
	}
	spec := gatewaySpec(3, alone)
	if !strings.Contains(spec, "\ndivert "+gw.String()+":443=/b/divert/x.sock") ||
		!strings.Contains(spec, "\nhost brig.internal="+gw.String()) {
		t.Errorf("the gateway spec does not hold brig.internal: %q", spec)
	}
	if spec == gatewaySpec(3, Egress{Divert: Divert{Socket: "/b/divert/x.sock"}}) {
		t.Error("a gateway without brig.internal would be reused")
	}
}

// hull refuses brig.internal where its gateway is not the guest's way out,
// as the endpoint mode, and on hvi with no network.
func TestHullRefusesBrigInternalOffTheGateway(t *testing.T) {
	scratchIsolatedDir(t)
	spec := RunSpec{Name: "s", Net: "isolated", Egress: Egress{Divert: Divert{Socket: "/b/x.sock", BrigInternal: true}}}
	for _, hv := range []string{"vz", "qemu"} {
		err := supports(spec, hv)
		var ce *CapabilityError
		if !errors.As(err, &ce) || ce.Property != RouteModeProperty(v1alpha1.ModeEndpoint) ||
			!strings.Contains(err.Error(), "vmnet") {
			t.Errorf("%s: %v", hv, err)
		}
	}
	offline := spec
	offline.Net = "none"
	if err := supports(offline, "hvi"); err == nil || !strings.Contains(err.Error(), "BRIG_NETWORK=offline") {
		t.Errorf("hvi offline: %v", err)
	}
	if err := supports(spec, "hvi"); err != nil {
		t.Errorf("hvi: %v", err)
	}
	if err := hostSupports(spec); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("the host kind: %v", err)
	}
}

// A spec that diverts is refused on the host kind, which has no network of
// its own to divert from. Ignored, the connections would reach the
// destination directly.
func TestTheHostKindRefusesADivert(t *testing.T) {
	err := hostSupports(RunSpec{Name: "brig-s", Net: "isolated", Egress: Egress{Divert: testDivert}})
	if err == nil || !strings.Contains(err.Error(), "no network of its own to divert") {
		t.Errorf("hostSupports = %v, want the divert refused", err)
	}
	if err := hostSupports(RunSpec{Name: "brig-s", Net: "isolated"}); err != nil {
		t.Errorf("a plain host run is refused: %v", err)
	}
}

// A boot that diverts and carries no policy is refused as the route mode that
// asked for the divert, and its remedy does not tell the user to detach a
// policy it has not got.
func TestADivertOnlyRefusalNamesTheRouteMode(t *testing.T) {
	err := egressRefusal(Egress{Divert: Divert{BrigInternal: true, Socket: "/s"}}, "nft failed", "Install nftables", nil)
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.Property != RouteModeProperty(v1alpha1.ModeEndpoint) ||
		ce.Remedy != "Install nftables, or detach the policy" {
		t.Errorf("brig.internal only: %+v", ce)
	}
	err = egressRefusal(Egress{Default: "deny", Divert: testDivert}, "nft failed", "Install nftables", nil)
	if !errors.As(err, &ce) || ce.Property != EgressPolicy || ce.Remedy != "Install nftables, or detach the policy" {
		t.Errorf("under a policy: %+v", ce)
	}
}

// nerdctl routes brig.internal with nftables, which docker does not take and
// a host without nft cannot run.
func TestNerdctlProbeBrigInternal(t *testing.T) {
	prev := lookNFT
	t.Cleanup(func() { lookNFT = prev })
	lookNFT = func() (string, error) { return "/usr/sbin/nft", nil }
	if err := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).ProbeBrigInternal(); err != nil {
		t.Errorf("nerdctl with nft: %v", err)
	}
	var ce *CapabilityError
	if err := (&nerdctl{bin: "/usr/bin/docker"}).ProbeBrigInternal(); !errors.As(err, &ce) ||
		ce.State != CannotEnforce || !strings.Contains(err.Error(), "docker manages its own bridges") {
		t.Errorf("docker: %v", err)
	}
	lookNFT = func() (string, error) { return "", errors.New("nft is not installed") }
	if err := (&nerdctl{bin: "/usr/local/bin/nerdctl"}).ProbeBrigInternal(); !errors.As(err, &ce) ||
		ce.Property != RouteModeProperty(v1alpha1.ModeEndpoint) || !strings.Contains(err.Error(), "Install nftables") {
		t.Errorf("no nft: %v", err)
	}
}
