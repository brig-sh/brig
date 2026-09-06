package runtime

import (
	"net/netip"
	"path/filepath"
)

// Networks for the isolated posture: one per sandbox, rather than one address
// each on a network they share.
//
// hull configures the guest statically -- --gateway-cidr is the address the VM
// gets, not a pool to draw from -- so brig hands these out. On the shared
// network that means an address per sandbox; see gatewayip.go. Here it means a
// whole network per sandbox, which is what makes an egress policy per sandbox
// possible and what keeps two sandboxes off each other's segment.
//
// The bookkeeping is netAlloc's, shared with the addresses on the shared
// network; see netalloc.go.

const (
	// The space the isolated networks are carved out of.
	//
	// A different /24 from the shared network's, and it has to be: the shared
	// gateway serves the whole of gatewaySubnet and hands out addresses across
	// it, so a /30 taken from there would be a network inside a network, with
	// two gateways claiming the same addresses.
	//
	// Adjacent to it, and inside the same 198.18.0.0/15 the shared network
	// already sits in, so brig's claim on the host's address space grows by
	// one /24 rather than moving somewhere new. Every reason gatewaySubnet
	// gives for that range applies here unchanged, including leaving the
	// sibling 198.19.0.0/16 to OrbStack.
	isolatedSpace = "198.18.1.0/24"

	// One sandbox's slice of it: a network address, the gateway, the guest,
	// and a broadcast address. Four is every address a network with a single
	// member on it can use, so nothing is wasted by making them this small.
	sandboxNetBits   = 30
	sandboxBlockSize = 4

	// How many sandboxes the space holds. Well past what a host can run one
	// microVM each for, and past what their gateways alone would fit in
	// memory: 64 of them is nearly 2 GB before a single guest boots.
	sandboxNets = 256 / sandboxBlockSize
)

// isolatedNets is the allocator for these networks.
func isolatedNets() (netAlloc, error) {
	dir, err := gatewayDir()
	if err != nil {
		return netAlloc{}, err
	}
	return netAlloc{
		// Beside the sockets, so an unusual BRIG_GATEWAY_DIR keeps its own
		// assignments. A separate file from the shared network's
		// gateway-ips.json, because the two hand out different things out of
		// different spaces.
		path:  filepath.Join(dir, "sandbox-nets.json"),
		first: 0,
		last:  sandboxNets - 1,
		space: "the isolated address space " + isolatedSpace,
		unit:  "networks",
	}, nil
}

// sandboxNet returns the index of the network this sandbox sits on, assigning
// one if it has none. Everything addressable about the sandbox is derived from
// it: the subnet, the gateway on it, and the guest's own address.
func sandboxNet(name string) (int, error) {
	alloc, err := isolatedNets()
	if err != nil {
		return 0, err
	}
	return alloc.slot(name)
}

// lookupSandboxNet answers what this sandbox already has, and nothing when it
// has none. It is what a question about a sandbox asks, so that asking cannot
// spend one of the 64 networks; see netAlloc.lookup.
func lookupSandboxNet(name string) (int, bool) {
	alloc, err := isolatedNets()
	if err != nil {
		return 0, false
	}
	return alloc.lookup(name)
}

// releaseSandboxNet gives a removed sandbox's network back.
func releaseSandboxNet(name string) {
	if alloc, err := isolatedNets(); err == nil {
		alloc.release(name)
	}
}

// sandboxBase is the network address of one sandbox's block.
//
// The arithmetic is on the last octet alone, which holds while the space is a
// /24 and the blocks divide it evenly. Both are constants above, and a test
// checks they still agree.
func sandboxBase(index int) netip.Addr {
	octets := netip.MustParsePrefix(isolatedSpace).Addr().As4()
	octets[3] = byte(index * sandboxBlockSize)
	return netip.AddrFrom4(octets)
}

// nth is the address n positions after a, which is only ever called for the
// two addresses inside a block.
func nth(a netip.Addr, n int) netip.Addr {
	for ; n > 0; n-- {
		a = a.Next()
	}
	return a
}

// sandboxSubnet is the network the gateway serves, as --subnet takes it.
func sandboxSubnet(index int) string {
	return netip.PrefixFrom(sandboxBase(index), sandboxNetBits).String()
}

// sandboxGatewayIP is the gateway's own address on that network. hull derives
// the same address from the guest's CIDR by adding one to the network address,
// so these two must not be derived differently here.
func sandboxGatewayIP(index int) string {
	return nth(sandboxBase(index), 1).String()
}

// sandboxCIDR is the guest's address, in the form hull takes on
// --gateway-cidr: the address itself with the network's prefix.
func sandboxCIDR(index int) string {
	return netip.PrefixFrom(nth(sandboxBase(index), 2), sandboxNetBits).String()
}
