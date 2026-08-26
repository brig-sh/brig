package runtime

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
)

// Addresses on the gateway's network.
//
// hull configures the guest statically: --gateway-cidr is the address the VM
// gets, not a pool to draw from, and it must be passed together with
// --gateway-sock. So brig has to hand out one address per sandbox, and two
// sandboxes sharing one would give a virtual network with a duplicate address
// on it -- which fails in a way nobody enjoys diagnosing.
//
// The map is keyed by sandbox name and persisted, so a sandbox that is stopped
// and started again comes back on the address it had. Removing a sandbox frees
// its address for the next one.

// gatewayIPStore is the file holding the assignments.
func gatewayIPStore() (string, error) {
	sock, err := gatewaySocket()
	if err != nil {
		return "", err
	}
	// Beside the socket, so an unusual BRIG_GATEWAY_SOCK keeps its own
	// assignments rather than sharing the default ones.
	return filepath.Join(filepath.Dir(sock), "gateway-ips.json"), nil
}

func readGatewayIPs(path string) map[string]int {
	assigned := map[string]int{}
	blob, err := os.ReadFile(path)
	if err != nil {
		return assigned
	}
	// A corrupt file is not worth failing a boot over: the addresses are
	// derivable again, and the worst case is a sandbox moving to a new one.
	if err := json.Unmarshal(blob, &assigned); err != nil {
		return map[string]int{}
	}
	return assigned
}

func writeGatewayIPs(path string, assigned map[string]int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(assigned, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file and renamed, so a crash mid-write
	// cannot leave a half-parsed map behind.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gateway-ips-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// gatewayCIDR returns the address this sandbox should use, in the form hull
// takes on --gateway-cidr: the guest's own address with the subnet's prefix.
func gatewayCIDR(name string) (string, error) {
	path, err := gatewayIPStore()
	if err != nil {
		return "", err
	}
	assigned := readGatewayIPs(path)
	if host, ok := assigned[name]; ok && host >= firstGuestHost && host <= lastGuestHost {
		return formatGatewayCIDR(host), nil
	}

	host, err := lowestFreeHost(assigned)
	if err != nil {
		return "", err
	}
	assigned[name] = host
	if err := writeGatewayIPs(path, assigned); err != nil {
		return "", fmt.Errorf("could not record the sandbox address: %w", err)
	}
	return formatGatewayCIDR(host), nil
}

// releaseGatewayIP gives a removed sandbox's address back. Losing this is not
// fatal -- it costs one address out of 253 -- so callers ignore the error
// rather than failing a removal over bookkeeping.
func releaseGatewayIP(name string) {
	path, err := gatewayIPStore()
	if err != nil {
		return
	}
	assigned := readGatewayIPs(path)
	if _, ok := assigned[name]; !ok {
		return
	}
	delete(assigned, name)
	_ = writeGatewayIPs(path, assigned)
}

func lowestFreeHost(assigned map[string]int) (int, error) {
	taken := make(map[int]bool, len(assigned))
	for _, host := range assigned {
		taken[host] = true
	}
	for host := firstGuestHost; host <= lastGuestHost; host++ {
		if !taken[host] {
			return host, nil
		}
	}
	// Sorted only so the message is stable enough to be worth reading.
	names := make([]string, 0, len(assigned))
	for name := range assigned {
		names = append(names, name)
	}
	sort.Strings(names)
	return 0, fmt.Errorf("the sandbox network %s is full: %d addresses are in use (%v). "+
		"Remove some sandboxes with `brig rm`", gatewaySubnet, len(names), names)
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
