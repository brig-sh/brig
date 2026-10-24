package runtime

import (
	"net/netip"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// brig.internal on nerdctl. The sandbox's resolver answers the name with the
// address of the sandbox bridge, and the sandbox's nftables table diverts
// port 443 of that address to the broker (see nerdctlegress.go and
// internal/divert).

// lookNFT finds nft. A variable so a test can run without nftables.
var lookNFT = func() (string, error) { return lookSbin("nft") }

// ProbeBrigInternal reports whether this host routes brig.internal on
// nerdctl: the runtime is nerdctl, not docker, and nft is installed. The
// sandbox's table diverts brig.internal with nftables.
func (n *nerdctl) ProbeBrigInternal() error {
	path := RunPath{n.driver(), containerdRuntime()}
	refuse := func(why, remedy string) error {
		return &CapabilityError{Property: RouteModeProperty(v1alpha1.ModeEndpoint), Path: path,
			State: CannotEnforce, Why: why, Remedy: remedy}
	}
	if n.isDocker() {
		return refuse(endpointDockerWhy, "Point BRIG_RUNTIME_BIN at nerdctl, or detach the policy")
	}
	if _, err := lookNFT(); err != nil {
		return refuse("brig diverts brig.internal to the broker with nftables, and nft is not installed",
			"Install nftables, or detach the policy")
	}
	return nil
}

// BrigInternalAddr is the address of the bridge of the sandbox's own
// network, which the sandbox's resolver answers brig.internal with. The
// network is created when it does not exist, as a boot creates it. A route
// takes the isolated posture, so the boot then finds it in place.
func (n *nerdctl) BrigInternalAddr(name string) (netip.Addr, error) {
	if err := n.ensureSandboxNetwork(name); err != nil {
		return netip.Addr{}, err
	}
	b, err := n.bridgeOf(sandboxNetwork(name))
	if err != nil {
		return netip.Addr{}, err
	}
	return b.Gateway, nil
}
