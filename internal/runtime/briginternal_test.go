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

// hull's gateway takes --divert only when its help lists that flag as a flag
// of its own. A lookalike such as --divert-dns, or a sentence that mentions
// the flag, does not count, and the run is refused before boot as a property
// hull cannot enforce (exit 7).
func TestTheDivertProbeMatchesTheWholeFlag(t *testing.T) {
	for name, help := range map[string]string{
		"a lookalike flag":    "OPTIONS:\n   --divert-dns string   DNS divert\n   --host string   record\n",
		"a sentence about it": "DESCRIPTION:\n   A connection that --divert names goes to a socket.\n\nOPTIONS:\n   --host string   record\n",
		"no such flag":        "OPTIONS:\n   --host string   record\n",
	} {
		bin := fakeHull(t, help)
		err := (&hull{bin: bin}).ProbeBrigInternal()
		var ce *CapabilityError
		if !errors.As(err, &ce) || ce.State != CannotEnforce || !strings.Contains(err.Error(), "has no --divert") {
			t.Errorf("%s: ProbeBrigInternal = %v, want the refusal that hull has no --divert", name, err)
		}
	}
	for name, help := range map[string]string{
		"the flag alone":         "OPTIONS:\n   --divert string   divert host:port=socket\n",
		"the flag, repeatable":   "OPTIONS:\n   --divert string [ --divert string ]   divert host:port=socket (repeatable)\n",
		"beside a lookalike":     "OPTIONS:\n   --divert-dns string   DNS\n   --divert string   divert\n",
		"with an alias after it": "OPTIONS:\n   --divert value, -d value   divert\n",
	} {
		if err := (&hull{bin: fakeHull(t, help)}).ProbeBrigInternal(); err != nil {
			t.Errorf("%s: ProbeBrigInternal = %v, want nil", name, err)
		}
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
