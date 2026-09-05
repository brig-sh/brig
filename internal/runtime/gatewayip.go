package runtime

import (
	"fmt"
	"net/netip"
	"path/filepath"
)

// Addresses on the shared gateway's network.
//
// hull configures the guest statically: --gateway-cidr is the address the VM
// gets, not a pool to draw from, and it must be passed together with
// --gateway-sock. So brig has to hand out one address per sandbox, and two
// sandboxes sharing one would give a virtual network with a duplicate address
// on it -- which fails in a way nobody enjoys diagnosing.
//
// The bookkeeping is netAlloc's; see netalloc.go, which the isolated networks
// share.

// sharedIPs is the allocator for this network.
func sharedIPs() (netAlloc, error) {
	dir, err := gatewayDir()
	if err != nil {
		return netAlloc{}, err
	}
	return netAlloc{
		// Beside the socket, so an unusual BRIG_GATEWAY_DIR keeps its own
		// assignments rather than sharing the default ones.
		path:  filepath.Join(dir, "gateway-ips.json"),
		first: firstGuestHost,
		last:  lastGuestHost,
		space: "the sandbox network " + gatewaySubnet,
		unit:  "addresses",
	}, nil
}

// gatewayCIDR returns the address this sandbox should use, in the form hull
// takes on --gateway-cidr: the guest's own address with the subnet's prefix.
func gatewayCIDR(name string) (string, error) {
	alloc, err := sharedIPs()
	if err != nil {
		return "", err
	}
	host, err := alloc.slot(name)
	if err != nil {
		return "", err
	}
	return formatGatewayCIDR(host), nil
}

// releaseGatewayIP gives a removed sandbox's address back.
func releaseGatewayIP(name string) {
	if alloc, err := sharedIPs(); err == nil {
		alloc.release(name)
	}
}

// formatGatewayCIDR is the address for a guest, built from the subnet the
// gateway serves rather than from a second copy of its octets. The two used to
// be independent literals, which is one edit away from handing every guest an
// address on a network nothing routes.
func formatGatewayCIDR(host int) string {
	base := netip.MustParsePrefix(gatewaySubnet).Addr().As4()
	base[3] = byte(host)
	return fmt.Sprintf("%s/%d", netip.AddrFrom4(base), gatewayPrefix)
}
