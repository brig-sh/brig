package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHullSandboxNetworkRecoversRecordedGatewaySpec(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-legacy"
	// Isolation's first release wrote only the subnet and gateway. Later
	// records include policy rules; either records an isolated gateway.
	const legacySpec = "subnet=198.18.1.4/30\ngateway=198.18.1.5\n"
	for _, tt := range []struct {
		name, socket, spec, want string
		readError                bool
	}{
		{"original spec", "sandbox-brig-legacy.sock", legacySpec, "isolated", false},
		{"current spec", "sandbox-brig-legacy.sock", gatewaySpec(1, Egress{}), "isolated", false},
		{"hashed name", "sandbox-a1b2c3d4.sock", legacySpec, "isolated", false},
		{"missing spec", "sandbox-brig-legacy.sock", "", "", false},
		{"empty spec", "sandbox-brig-legacy.sock", "\n", "", false},
		{"unreadable spec", "sandbox-brig-legacy.sock", "", "", true},
	} {
		for _, backend := range []string{"hvi", "qemu"} {
			t.Run(tt.name+"/"+backend, func(t *testing.T) {
				control := filepath.Join(t.TempDir(), tt.socket)
				if tt.readError {
					// A directory reliably makes ReadFile fail, including when
					// tests run with permission to read mode-000 files.
					if err := os.Mkdir(gatewaySpecPath(control), 0o700); err != nil {
						t.Fatal(err)
					}
				} else if tt.spec != "" {
					if err := os.WriteFile(gatewaySpecPath(control), []byte(tt.spec), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				// Today's directory has a spec even when the recorded path
				// does not. Recovery must use the path in the saved argv.
				today := t.TempDir()
				t.Setenv("BRIG_GATEWAY_DIR", today)
				t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(today, "shared.sock"))
				if err := os.WriteFile(gatewaySpecPath(filepath.Join(today, tt.socket)), []byte(legacySpec), 0o600); err != nil {
					t.Fatal(err)
				}
				argv := []string{"hvi", "--kernel", "Image", "--net-gateway", qemuGatewaySocket(control)}
				if backend == "qemu" {
					argv = []string{"qemu-system-aarch64", "-kernel", "Image", "-netdev", "stream,id=net0,addr.type=unix,addr.path=" + qemuGatewaySocket(control)}
				}
				fixture, _ := json.Marshal(map[string]any{"id": name, "backend": backend, "cmdLine": argv})
				h := &hull{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
				got, err := h.SandboxNetwork(name)
				if got != tt.want || (err != nil) != (tt.want == "") {
					t.Fatalf("SandboxNetwork = %q, %v; want %q", got, err, tt.want)
				}
			})
		}
	}
}

func TestHullSandboxNetworkRecoversPreviousGatewayPaths(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-legacy"
	const shared = "ip=198.18.0.2::198.18.0.1:255.255.255.0:urunc:eth0:off:198.18.0.1"
	const isolated = "ip=198.18.1.6::198.18.1.5:255.255.255.252:urunc:eth0:off:198.18.1.5"
	for _, tt := range []struct {
		name, socket, kernel, want string
	}{
		{"old untagged shared socket", "gateway.sock.qemu", "", "shared"},
		{"old subnet socket", "gateway-10-87-0-0_24.sock.qemu", "", "shared"},
		{"moved shared directory", "gateway-198-18-0-0_24.sock.qemu", "", "shared"},
		{"previous custom shared socket", "custom.sock.qemu", "", "shared"},
		{"moved isolated directory", "sandbox-brig-legacy.sock.qemu", "", ""},
		{"moved hashed isolated directory", "sandbox-a1b2c3d4.sock.qemu", "", ""},
		{"different sandbox name", "sandbox-other.sock.qemu", "", ""},
		{"case variant of isolated name", "SANDBOX-brig-legacy.SOCK.qemu", "", ""},
		{"different shared subnet", "gateway-192-0-2-0_24.sock.qemu", "", "shared"},
		{"shared name with isolated addressing", "custom.sock.qemu", isolated, "shared"},
		{"isolated name with shared addressing", "sandbox-brig-legacy.sock.qemu", shared, ""},
		{"isolated name with DHCP", "sandbox-brig-legacy.sock.qemu", "ip=dhcp", ""},
		{"isolated prefix requires socket suffix", "sandbox-custom.qemu", "", "shared"},
		{"isolated suffix requires sandbox prefix", "custom-sandbox.sock.qemu", "", "shared"},
		{"wrong stream suffix", "custom.sock", shared, ""},
	} {
		for _, backend := range []string{"hvi", "qemu"} {
			t.Run(tt.name+"/"+backend, func(t *testing.T) {
				socket := filepath.Join("/previous/brig", tt.socket)
				argv := []string{"hvi", "boot", "--kernel", "Image", "--net-gateway", socket}
				kernelFlag := "--cmdline"
				if backend == "qemu" {
					argv = []string{"qemu-system-aarch64", "-kernel", "Image", "-netdev", "stream,id=net0,addr.type=unix,addr.path=" + socket}
					kernelFlag = "-append"
				}
				if tt.kernel != "" {
					argv = append(argv, kernelFlag, tt.kernel)
				}
				fixture, _ := json.Marshal(map[string]any{"id": name, "backend": backend, "cmdLine": argv})
				h := &hull{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
				got, err := h.SandboxNetwork(name)
				if got != tt.want || (err != nil) != (tt.want == "") {
					t.Fatalf("SandboxNetwork = %q, %v; want %q", got, err, tt.want)
				}
			})
		}
	}
}

func TestHullSandboxNetworkSharedOverrideCanLookIsolated(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-legacy"
	sock, err := isolatedSocket(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_GATEWAY_SOCK", sock)
	// Older versions allowed this shared override. Neither keeping it,
	// unsetting it, nor pointing elsewhere can identify what this guest booted.
	for _, override := range []string{sock, "", filepath.Join(t.TempDir(), "shared.sock")} {
		t.Setenv("BRIG_GATEWAY_SOCK", override)
		for _, argv := range [][]string{
			{"hvi", "--kernel", "Image", "--net-gateway", qemuGatewaySocket(sock)},
			{"qemu-system-aarch64", "-kernel", "Image", "-netdev", "stream,id=net0,addr.type=unix,addr.path=" + qemuGatewaySocket(sock)},
		} {
			fixture, _ := json.Marshal(map[string]any{"id": name, "cmdLine": argv})
			h := &hull{bin: networkInspectFixture(t, string(fixture), "", 0, "", 0)}
			if got, err := h.SandboxNetwork(name); got != "" || err == nil {
				t.Fatalf("override %q: recovery = %q, %v; want unknown", override, got, err)
			}
		}
	}
}
