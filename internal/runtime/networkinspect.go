package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Inspect exposes the VMM argv hull persisted at boot. A gateway's absence
// cannot stand in for it: an offline guest and an isolated guest whose gateway
// died both have no listening socket, and neither is a shared-network guest.
func (h *hull) SandboxNetwork(name string) (string, error) {
	out, stderr, err := inspectNetwork(h.bin, "inspect", name)
	if err != nil {
		// hull rc29 also says "instance not found" for unreadable or corrupt
		// metadata, and ps silently omits those records. Neither proves
		// absence, so an indexed legacy session must stay unknown here.
		return "", fmt.Errorf("inspect network of %s: %w: %s", name, err, firstLines(stderr, 3))
	}
	var state struct {
		ID      string   `json:"id"`
		Backend string   `json:"backend"`
		CmdLine []string `json:"cmdLine"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return "", fmt.Errorf("inspect network of %s: invalid hull response: %w", name, err)
	}
	if state.ID != name || len(state.CmdLine) < 2 {
		return "", fmt.Errorf("inspect network of %s: hull returned no matching VMM configuration", name)
	}
	backend := ""
	switch filepath.Base(state.CmdLine[0]) {
	case "hvi":
		backend = "hvi"
	case "vz-runner":
		backend = "vz"
	case "qemu-system-aarch64":
		backend = "qemu"
	default:
		return "", fmt.Errorf("inspect network of %s: unrecognised VMM executable", name)
	}
	args := state.CmdLine[1:]
	unknown := fmt.Errorf("inspect network of %s: unrecognised %s network configuration", name, backend)
	if state.Backend != "" && state.Backend != backend {
		return "", unknown
	}
	kernelFlag := "--kernel"
	if backend == "qemu" {
		kernelFlag = "-kernel"
	}
	if kernel, ok := argvValue(args, kernelFlag); !ok || kernel == "" {
		return "", unknown
	}
	// Future network flags cannot be read as the absence of a NIC merely
	// because this version does not know how they attach one.
	for _, arg := range args {
		if strings.HasPrefix(arg, "--net") && arg != "--net" && arg != "--net-gateway" && arg != "--net-mac" && arg != "--net-fd" {
			return "", unknown
		}
	}
	switch backend {
	case "hvi":
		if hasArg(args, "--net-fd") || hasArg(args, "--no-net") {
			return "", unknown
		}
		gateway, hasGateway := argvValue(args, "--net-gateway")
		if hasGateway {
			if gateway == "" || hasArg(args, "--net") {
				return "", unknown
			}
			return inspectedGateway(name, gateway)
		}
		if hasArg(args, "--net") {
			return "shared", nil
		}
		return "none", nil
	case "vz":
		if hasArg(args, "--net-fd") || hasArg(args, "--net-gateway") || hasArg(args, "--net") {
			// An inherited descriptor does not identify its gateway in argv.
			return "", unknown
		}
		if hasArg(args, "--no-net") {
			return "none", nil
		}
		return "shared", nil
	case "qemu":
		netdev, hasNetdev := argvValue(args, "-netdev")
		nic, hasNIC := argvValue(args, "-nic")
		if hasArg(args, "-net") || (hasNetdev && hasNIC) {
			return "", unknown
		}
		if hasNIC && nic == "none" {
			return "none", nil
		}
		if hasNetdev && strings.HasPrefix(netdev, "vmnet-shared,") {
			return "shared", nil
		}
		if hasNetdev && strings.HasPrefix(netdev, "stream,") && strings.Contains(","+netdev+",", ",addr.type=unix,") {
			for _, field := range strings.Split(netdev, ",") {
				if sock, ok := strings.CutPrefix(field, "addr.path="); ok {
					return inspectedGateway(name, sock)
				}
			}
		}
	}
	return "", unknown
}

// nerdctl's Docker-compatible inspect records NetworkMode from its persisted
// networks label, so it remains available after stop. Interface addresses do
// not: NetworkSettings can be empty for a stopped container.
func (n *nerdctl) SandboxNetwork(name string) (string, error) {
	out, stderr, err := inspectNetwork(n.bin, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		// An inspect failure alone is not evidence of absence. List has an
		// unambiguous all-containers form on this runtime, unlike hull.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if exists, listErr := n.exists(ctx, name); listErr == nil && !exists {
			return "", nil
		}
		return "", fmt.Errorf("inspect network of %s: %w: %s", name, err, firstLines(stderr, 3))
	}
	var state struct {
		HostConfig      struct{ NetworkMode, Runtime string }
		Config          struct{ Labels map[string]string }
		NetworkSettings struct{ Networks map[string]json.RawMessage }
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return "", fmt.Errorf("inspect network of %s: invalid container response: %w", name, err)
	}
	mode := state.HostConfig.NetworkMode
	persistedMode := false
	if raw, ok := state.Config.Labels["nerdctl/networks"]; ok {
		var networks []string
		if err := json.Unmarshal([]byte(raw), &networks); err != nil || len(networks) != 1 || networks[0] != mode {
			return "", fmt.Errorf("inspect network of %s: ambiguous container network attachments", name)
		}
		persistedMode = true
	}
	// nerdctl reports interfaces in the container's network namespace,
	// including the addressless TAP urunc wires to its one CNI interface.
	// That pair is one attachment. Require its persisted label and exact
	// runtime/interface shape so an unexpected second network stays unknown.
	if persistedMode && mode != "none" && state.HostConfig.Runtime == "io.containerd.urunc.v2" &&
		len(state.NetworkSettings.Networks) == 2 && addresslessUruncTAP(state.NetworkSettings.Networks["unknown-tap0_urunc"]) {
		delete(state.NetworkSettings.Networks, "unknown-tap0_urunc")
	}
	if len(state.NetworkSettings.Networks) > 1 {
		return "", fmt.Errorf("inspect network of %s: multiple container network attachments", name)
	}
	for network := range state.NetworkSettings.Networks {
		// nerdctl can name the guest-facing interface unknown-eth0. Only
		// the unambiguous persisted attachment can identify that network;
		// the placeholder alone is not evidence of any posture.
		if persistedMode && mode != "none" && network == "unknown-eth0" {
			continue
		}
		if network != mode && !(mode == "default" && network == "bridge") {
			return "", fmt.Errorf("inspect network of %s: container network configuration disagrees with its attachment", name)
		}
	}
	switch mode {
	case "none":
		return "none", nil
	case "bridge", "default":
		return "shared", nil
	case sandboxNetwork(name):
		return "isolated", nil
	default:
		return "", fmt.Errorf("inspect network of %s: unrecognised container network mode %q", name, mode)
	}
}

func addresslessUruncTAP(raw json.RawMessage) bool {
	var tap struct {
		IPAddress, GlobalIPv6Address     *string
		IPPrefixLen, GlobalIPv6PrefixLen *int
	}
	// Missing fields are not evidence of an addressless interface. A null,
	// truncated or differently shaped inspect response must still fail closed.
	if err := json.Unmarshal(raw, &tap); err != nil {
		return false
	}
	return tap.IPAddress != nil && *tap.IPAddress == "" &&
		tap.GlobalIPv6Address != nil && *tap.GlobalIPv6Address == "" &&
		tap.IPPrefixLen != nil && *tap.IPPrefixLen == 0 &&
		tap.GlobalIPv6PrefixLen != nil && *tap.GlobalIPv6PrefixLen == 0
}

func inspectNetwork(bin string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = mergeEnv(telemetryEnv(false))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, strings.TrimSpace(stderr.String()), err
}

// Duplicate flags are not a single known configuration. In particular a
// second network device must not be hidden behind the first one's isolation.
func argvValue(args []string, flag string) (string, bool) {
	value, found := "", false
	for i, arg := range args {
		if arg == flag {
			if found || i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", true
			}
			value, found = args[i+1], true
		}
	}
	return value, found
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}
