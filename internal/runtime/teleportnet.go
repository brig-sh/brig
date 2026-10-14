package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// A teleported guest keeps the network it had. hull configures a guest's
// address statically, so the address is in the guest's memory, and the moved
// memory still holds it: the guest on the destination sends from the same
// address, to the same gateway address, as it did on the source. So the
// destination has to give it the same network: the same /30 for an isolated
// sandbox, with the same egress rules on its gateway, or the same address on
// the shared network. A gateway serving any other network would leave the
// guest with no route out, and one serving other rules would put the guest
// under rules it was never under, or under none.
//
// The source reads what the sandbox has (SandboxAddress), the destination
// checks it can give the same (AddressHolder), and takes it before hull
// resumes the guest there (ReceiveNetwork). ReleaseReceivedNetwork gives it
// back when the move is called off.

// SandboxAddress is the network a sandbox was booted on and its slot there:
// the index of its /30 on the isolated network, or its host number on the
// shared one. net is "isolated", "shared" or "none", and slot is meaningless
// for none. ok is false when either half is not recorded, which a move cannot
// recover: the guest's address is in its memory and nowhere else.
func SandboxAddress(name string) (net string, slot int, ok bool) {
	net, err := BootedNet(name)
	if err != nil || net == "" {
		return "", 0, false
	}
	switch net {
	case "none":
		return net, 0, true
	case "isolated":
		slot, ok = lookupSandboxNet(name)
	case "shared":
		alloc, err := sharedIPs()
		if err != nil {
			return "", 0, false
		}
		slot, ok = alloc.lookup(name)
	default:
		return "", 0, false
	}
	return net, slot, ok
}

// AddressHolder names the sandbox that holds this slot on this host's network
// of that kind, or "" when it is free or already held by name. An error is a
// network word brig does not know.
func AddressHolder(net string, slot int, name string) (string, error) {
	alloc, err := allocFor(net)
	if err != nil || alloc == nil {
		return "", err
	}
	return alloc.holder(slot, name), nil
}

// ReceiveNetwork gives a sandbox arriving on this host the network it left:
// the same slot, the gateway serving it started with policy on it, and the
// posture recorded, so the brig here reads the sandbox as booted on it. It
// returns the gateway's control socket, or "" for a guest with no network.
//
// The gateway is started before hull resumes the guest here, which is the
// order a boot keeps too: a guest that runs before its gateway answers has
// already lost its first connections.
func ReceiveNetwork(bin, name, net string, slot int, policy Egress) (string, error) {
	if net == "none" {
		return "", RecordBootedNet(name, net)
	}
	alloc, err := allocFor(net)
	if err != nil {
		return "", err
	}
	if err := alloc.claim(name, slot); err != nil {
		return "", err
	}
	var sock string
	switch net {
	case "isolated":
		sock, err = ensureIsolatedGateway(bin, name, slot, policy)
	case "shared":
		// A shared network carries no rules, and a move of a sandbox under
		// a policy is isolated by construction. See Load in wrap.
		if policy.Filtered() {
			err = fmt.Errorf("the sandbox %s carries egress rules and arrives on the shared network, "+
				"which cannot hold rules for one member", name)
			break
		}
		var release func()
		sock, release, err = ensureGateway(bin)
		if err == nil {
			release()
		}
	}
	if err != nil {
		alloc.release(name)
		return "", err
	}
	if err := RecordBootedNet(name, net); err != nil {
		return "", err
	}
	return sock, nil
}

// ReleaseReceivedNetwork gives back what ReceiveNetwork took, for a move that
// was called off before the guest ran here.
func ReleaseReceivedNetwork(name string) {
	shutDownGateway(name)
	releaseSandboxNet(name)
	releaseGatewayIP(name)
	ForgetBootedNet(name)
}

// GatewayEnforcesEgress is the probe a policy boot runs, for a caller that
// is not booting: whether bin's network gateway takes egress rules. It is
// the one check, so `brig teleport-probe` on a destination answers it the way
// a boot there would.
func GatewayEnforcesEgress(bin string) error { return gatewayEnforces(bin) }

// HviPath is the hvi binary hull would run next to bin, or the one on PATH,
// or "" when neither is there. hull looks beside itself first, which is how
// a packaged hull finds the hvi it ships with.
func HviPath(bin string) string {
	if resolved, err := exec.LookPath(bin); err == nil {
		beside := filepath.Join(filepath.Dir(resolved), "hvi")
		if fi, err := os.Stat(beside); err == nil && !fi.IsDir() {
			return beside
		}
	}
	if p, err := exec.LookPath("hvi"); err == nil {
		return p
	}
	return ""
}

// allocFor is the allocator of one network kind, or nil for none.
func allocFor(net string) (*netAlloc, error) {
	var (
		a   netAlloc
		err error
	)
	switch net {
	case "none":
		return nil, nil
	case "isolated":
		a, err = isolatedNets()
	case "shared":
		a, err = sharedIPs()
	default:
		return nil, fmt.Errorf("no network called %q", net)
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
