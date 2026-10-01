package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// These fixtures use hull's inspect InstanceState (backend and cmdLine), not
// gateway liveness: a stopped sandbox and a failed gateway still retain the
// network that must survive migration to posture records.
func TestHullSandboxNetworkReadsStoredVMMConfiguration(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-legacy"
	isolated, err := isolatedSocket(name)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, backend, want string
		argv                []string
	}{
		{"hvi shared", "hvi", "shared", []string{"hvi", "boot", "--kernel", "Image", "--net-gateway", qemuGatewaySocket(shared)}},
		{"hvi isolated without a gateway process", "hvi", "", []string{"hvi", "boot", "--kernel", "Image", "--net-gateway", qemuGatewaySocket(isolated)}},
		{"hvi offline", "hvi", "none", []string{"hvi", "boot", "--kernel", "Image", "--net-mac", "02:00:00:00:00:01"}},
		{"old hvi without backend field", "", "", []string{"/opt/hvi", "boot", "--kernel", "Image", "--net-gateway", qemuGatewaySocket(isolated)}},
		{"hvi built in network", "hvi", "shared", []string{"hvi", "boot", "--kernel", "Image", "--net"}},
		{"vz shared", "vz", "shared", []string{"vz-runner", "--kernel", "Image"}},
		{"vz offline", "vz", "none", []string{"vz-runner", "--kernel", "Image", "--no-net"}},
		{"qemu shared", "qemu", "shared", []string{"qemu-system-aarch64", "-kernel", "Image", "-netdev", "vmnet-shared,id=net0"}},
		{"qemu offline", "qemu", "none", []string{"qemu-system-aarch64", "-kernel", "Image", "-nic", "none"}},
		{"qemu gateway", "qemu", "shared", []string{"qemu-system-aarch64", "-kernel", "Image", "-netdev", "stream,id=net0,addr.type=unix,addr.path=" + qemuGatewaySocket(shared)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture, _ := json.Marshal(map[string]any{"id": name, "status": "stopped", "backend": tt.backend, "cmdLine": tt.argv})
			h := &hull{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
			if got, err := h.SandboxNetwork(name); got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("SandboxNetwork = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, ok := lookupSandboxNet(name); ok {
		t.Fatal("inspection allocated an isolated network")
	}
}

func TestHullSandboxNetworkRefusesUnknownMetadata(t *testing.T) {
	scratchIsolatedDir(t)
	for _, fixture := range []string{
		`{`,
		`{}`,
		`{"id":"brig-legacy","cmdLine":[]}`,
		`{"id":"another","cmdLine":["hvi","--kernel","Image"]}`,
		`{"id":"brig-legacy","cmdLine":["unknown-vmm","--kernel","Image"]}`,
		`{"id":"brig-legacy","backend":"vz","cmdLine":["hvi","--kernel","Image"]}`,
		`{"id":"brig-legacy","cmdLine":["hvi","--kernel","Image","--network-future"]}`,
		`{"id":"brig-legacy","cmdLine":["hvi","--kernel","Image","--net-gateway"]}`,
		`{"id":"brig-legacy","cmdLine":["hvi","--kernel","Image","--net-gateway","/other.sock"]}`,
		`{"id":"brig-legacy","cmdLine":["hvi","--kernel","Image","--net-gateway","/other.sock","--net"]}`,
		`{"id":"brig-legacy","cmdLine":["vz-runner","--kernel","Image","--net-fd","3"]}`,
		`{"id":"brig-legacy","cmdLine":["qemu-system-aarch64","-kernel","Image"]}`,
		`{"id":"brig-legacy","cmdLine":["qemu-system-aarch64","-nic","none","-netdev","vmnet-shared,id=net0"]}`,
		`{"id":"brig-legacy","cmdLine":["qemu-system-aarch64","-netdev","vmnet-shared,id=net0","-netdev","user,id=net1"]}`,
	} {
		h := &hull{bin: networkInspectFixture(t, fixture, "", 0, "", 0)}
		if got, err := h.SandboxNetwork("brig-legacy"); got != "" || err == nil {
			t.Errorf("fixture %s: got %q, %v; want unknown with error", fixture, got, err)
		}
	}
}

func TestHullSandboxNetworkDistinguishesAbsenceFromFailure(t *testing.T) {
	for _, tt := range []struct {
		stderr  string
		wantErr bool
	}{
		// hull gives this same answer for corrupt/unreadable state. It
		// cannot establish absence, even if ps would omit the sandbox too.
		{"Error: instance not found: brig-legacy", true},
		{"Error: store is locked", true},
		{"", true},
	} {
		h := &hull{bin: networkInspectFixture(t, "", tt.stderr, 1, "", 0)}
		got, err := h.SandboxNetwork("brig-legacy")
		if got != "" || (err != nil) != tt.wantErr {
			t.Errorf("stderr %q: got %q, %v", tt.stderr, got, err)
		}
	}
}

func TestNerdctlSandboxNetworkReadsPersistedNetworkMode(t *testing.T) {
	for _, mode := range []string{"bridge", "default", "none", "brig-legacy"} {
		want := "shared"
		if mode == "none" {
			want = "none"
		} else if mode == "brig-legacy" {
			want = "isolated"
		}
		// A stopped container has no interface state. The persisted label
		// is the source of HostConfig.NetworkMode in nerdctl's inspect.
		networks, _ := json.Marshal([]string{mode})
		fixture, _ := json.Marshal(map[string]any{
			"HostConfig":      map[string]string{"NetworkMode": mode},
			"Config":          map[string]any{"Labels": map[string]string{"nerdctl/networks": string(networks)}},
			"NetworkSettings": map[string]any{},
		})
		n := &nerdctl{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
		if got, err := n.SandboxNetwork("brig-legacy"); got != want || err != nil {
			t.Errorf("mode %q: got %q, %v; want %q", mode, got, err, want)
		}
	}
}

func TestNerdctlSandboxNetworkRefusesUnknownMetadata(t *testing.T) {
	for _, fixture := range []string{
		`{`, `{}`, `{"HostConfig":{"NetworkMode":"host"}}`,
		`{"HostConfig":{"NetworkMode":"some-custom-network"}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"Config":{"Labels":{"nerdctl/networks":"[]"}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"Config":{"Labels":{"nerdctl/networks":"[\"brig-legacy\",\"bridge\"]"}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"Config":{"Labels":{"nerdctl/networks":"[\"bridge\"]"}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"Config":{"Labels":{"nerdctl/networks":"invalid"}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"NetworkSettings":{"Networks":{"brig-legacy":{},"bridge":{}}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"NetworkSettings":{"Networks":{"bridge":{}}}}`,
		`{"HostConfig":{"NetworkMode":"brig-legacy"},"NetworkSettings":{"Networks":{"unknown-eth0":{}}}}`,
		`{"HostConfig":{"NetworkMode":"none"},"Config":{"Labels":{"nerdctl/networks":"[\"none\"]"}},"NetworkSettings":{"Networks":{"unknown-eth0":{}}}}`,
	} {
		n := &nerdctl{bin: networkInspectFixture(t, fixture, "", 0, "", 0)}
		if got, err := n.SandboxNetwork("brig-legacy"); got != "" || err == nil {
			t.Errorf("fixture %s: got %q, %v; want unknown with error", fixture, got, err)
		}
	}
}

func TestNerdctlSandboxNetworkRecoversTheLegacyInterfaceName(t *testing.T) {
	for _, mode := range []string{"bridge", "brig-legacy"} {
		networks, _ := json.Marshal([]string{mode})
		fixture, _ := json.Marshal(map[string]any{
			"HostConfig":      map[string]string{"NetworkMode": mode},
			"Config":          map[string]any{"Labels": map[string]string{"nerdctl/networks": string(networks)}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"unknown-eth0": map[string]any{}}},
		})
		n := &nerdctl{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
		want := "shared"
		if mode == "brig-legacy" {
			want = "isolated"
		}
		if got, err := n.SandboxNetwork("brig-legacy"); got != want || err != nil {
			t.Errorf("legacy interface on %q: got %q, %v; want %q", mode, got, err, want)
		}
	}
}

func TestNerdctlSandboxNetworkRecognizesUruncHelperTAP(t *testing.T) {
	for _, tt := range []struct {
		name, mode, runtime, helperName, ipv4, ipv6, want string
		ipv4Prefix, ipv6Prefix                            int
		labels                                            []string
		omitLabel                                         bool
		missingField, nullField                           string
		nullHelper                                        bool
		remainingInterfaces                               []string
	}{
		{name: "shared urunc guest", want: "shared"},
		{name: "isolated urunc guest", mode: "brig-legacy", want: "isolated"},
		{name: "non-urunc runtime", runtime: "io.containerd.runc.v2"},
		{name: "helper has IPv4", ipv4: "192.0.2.8"},
		{name: "helper has IPv6", ipv6: "2001:db8::8"},
		{name: "helper has IPv4 prefix", ipv4Prefix: 24},
		{name: "helper has IPv6 prefix", ipv6Prefix: 64},
		{name: "null helper metadata", nullHelper: true},
		{name: "missing helper IPv4", missingField: "IPAddress"},
		{name: "missing helper IPv6", missingField: "GlobalIPv6Address"},
		{name: "missing helper IPv4 prefix", missingField: "IPPrefixLen"},
		{name: "missing helper IPv6 prefix", missingField: "GlobalIPv6PrefixLen"},
		{name: "null helper IPv4", nullField: "IPAddress"},
		{name: "null helper IPv6", nullField: "GlobalIPv6Address"},
		{name: "null helper IPv4 prefix", nullField: "IPPrefixLen"},
		{name: "null helper IPv6 prefix", nullField: "GlobalIPv6PrefixLen"},
		{name: "unknown helper interface", helperName: "unknown-tap1_urunc"},
		{name: "multiple persisted attachments", labels: []string{"bridge", "other"}},
		{name: "mismatched persisted attachment", labels: []string{"other"}},
		{name: "missing persisted attachment", omitLabel: true},
		{name: "offline mode with interfaces", mode: "none"},
		{name: "helper without a network interface", remainingInterfaces: []string{}},
		{name: "helper with two network interfaces", remainingInterfaces: []string{"unknown-eth0", "bridge"}},
		{name: "helper with wrong remaining interface", remainingInterfaces: []string{"unknown-eth1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mode := tt.mode
			if mode == "" {
				mode = "bridge"
			}
			runtime := tt.runtime
			if runtime == "" {
				runtime = "io.containerd.urunc.v2"
			}
			helperName := tt.helperName
			if helperName == "" {
				helperName = "unknown-tap0_urunc"
			}
			labels := map[string]string{}
			if !tt.omitLabel {
				networks := tt.labels
				if networks == nil {
					networks = []string{mode}
				}
				encoded, err := json.Marshal(networks)
				if err != nil {
					t.Fatal(err)
				}
				labels["nerdctl/networks"] = string(encoded)
			}
			helper := map[string]any{
				"IPAddress": tt.ipv4, "IPPrefixLen": tt.ipv4Prefix,
				"GlobalIPv6Address": tt.ipv6, "GlobalIPv6PrefixLen": tt.ipv6Prefix,
				"MacAddress": "02:00:00:00:00:02",
			}
			if tt.missingField != "" {
				delete(helper, tt.missingField)
			}
			if tt.nullField != "" {
				helper[tt.nullField] = nil
			}
			if tt.nullHelper {
				helper = nil
			}
			interfaces := map[string]any{helperName: helper}
			remaining := tt.remainingInterfaces
			if remaining == nil {
				remaining = []string{"unknown-eth0"}
			}
			for _, name := range remaining {
				interfaces[name] = map[string]any{
					"IPAddress": "192.0.2.7", "IPPrefixLen": 24,
					"GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0,
					"MacAddress": "02:00:00:00:00:01",
				}
			}
			// Real urunc inspect includes its unaddressed helper TAP beside
			// unknown-eth0. Keep that shape, including zero prefix lengths
			// and a MAC, without copying image, mount or session metadata.
			fixture, err := json.Marshal(map[string]any{
				"HostConfig":      map[string]string{"NetworkMode": mode, "Runtime": runtime},
				"Config":          map[string]any{"Labels": labels},
				"NetworkSettings": map[string]any{"Networks": interfaces},
			})
			if err != nil {
				t.Fatal(err)
			}
			n := &nerdctl{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
			got, err := n.SandboxNetwork("brig-legacy")
			if tt.want == "" {
				if got != "" || err == nil {
					t.Fatalf("SandboxNetwork = %q, %v; want unknown with error", got, err)
				}
			} else if got != tt.want || err != nil {
				t.Fatalf("SandboxNetwork = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestNerdctlSandboxNetworkConfirmsAbsence(t *testing.T) {
	for _, tt := range []struct {
		name, list string
		listExit   int
		wantErr    bool
	}{
		{"absent", "", 0, false},
		{"exists stopped", "brig-legacy\tExited (0)", 0, true},
		{"daemon unavailable", "", 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := &nerdctl{bin: networkInspectFixture(t, "", "inspect failed", 1, tt.list, tt.listExit)}
			got, err := n.SandboxNetwork("brig-legacy")
			if got != "" || (err != nil) != tt.wantErr {
				t.Errorf("got %q, %v; want error %t", got, err, tt.wantErr)
			}
		})
	}
	var _ NetworkInspector = &hull{}
	var _ NetworkInspector = &nerdctl{}
}

func networkInspectFixture(t *testing.T, stdout, stderr string, status int, list string, listStatus int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  'inspect brig-legacy'|'inspect --format {{json .}} brig-legacy')
    cat <<'INSPECT'
%s
INSPECT
    cat <<'ERROR' >&2
%s
ERROR
    exit %d ;;
  'ps -a --format {{.Names}}	{{.Status}}')
    cat <<'LIST'
%s
LIST
    exit %d ;;
  *) echo "unexpected fixture invocation" >&2; exit 99 ;;
esac
`, stdout, stderr, status, list, listStatus)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
