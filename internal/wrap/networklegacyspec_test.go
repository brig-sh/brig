package wrap

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// Keep Hull's inspection, staleness check and gateway cleanup real. Only the
// guest and its workspace are simulated: returning a posture from this fake
// would miss the stopped isolated guest that no longer has a gateway spec.
type legacySpecRuntime struct {
	*livenessRuntime
	adapter runtime.Runtime
	checks  int
}

func (r *legacySpecRuntime) SandboxNetwork(name string) (string, error) {
	return r.adapter.(runtime.NetworkInspector).SandboxNetwork(name)
}

func (r *legacySpecRuntime) NetworkStale(name, hypervisor, network string, egress runtime.Egress) bool {
	r.checks++
	return r.adapter.(runtime.NetworkChecker).NetworkStale(name, hypervisor, network, egress)
}

func (r *legacySpecRuntime) CanRun(spec runtime.RunSpec) error {
	return r.adapter.(runtime.RunChecker).CanRun(spec)
}

func (r *legacySpecRuntime) Stop(name string) error {
	r.stops++
	if err := r.adapter.Stop(name); err != nil {
		return err
	}
	r.running = false
	return nil
}

func legacySpecSession(t *testing.T) (profile.Profile, *legacySpecRuntime, string) {
	t.Helper()
	p, home := legacySession(t, "ubuntu")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BRIG_GATEWAY_SOCK", "")
	// macOS's Unix socket limit includes the test directory. Keep this private
	// directory short enough that the socket has the same name Brig derives.
	dir, err := os.MkdirTemp("/tmp", "brig-spec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	sock := filepath.Join(dir, "sandbox-brig-ubuntu.sock")
	state, err := json.Marshal(map[string]any{
		"id": "brig-ubuntu", "backend": "hvi",
		"cmdLine": []string{"/runtime/hvi", "--kernel", "/runtime/vmlinux", "--net-gateway", sock + ".qemu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "hull")
	script := "#!/bin/sh\ncase \"$1\" in\ninspect) cat <<'STATE'\n" + string(state) +
		"\nSTATE\n;;\nstop) exit 0;;\ntelemetry) echo disabled;;\n*) exit 97;;\nesac\n"
	for path, content := range map[string]string{
		bin:                                     script,
		filepath.Join(dir, "sandbox-nets.json"): `{"brig-ubuntu":0}`,
		filepath.Join(dir, "sandbox-brig-ubuntu.spec"): "subnet=198.18.1.0/30\ngateway=198.18.1.1\nunfiltered\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	adapter, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	return p, &legacySpecRuntime{
		livenessRuntime: &livenessRuntime{running: true, workspace: home},
		adapter:         adapter,
	}, sock
}

func listenLegacyGateway(t *testing.T, sock string) {
	t.Helper()
	listener, err := net.Listen("unix", sock+".qemu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

func TestLegacyIsolatedGatewaySpecKeepsTheRunningGuest(t *testing.T) {
	p, live, sock := legacySpecSession(t)
	listenLegacyGateway(t, sock)
	c, err := Load(p, Options{NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if c.Network != NetIsolated || c.netRecovery != networkRecovered {
		t.Fatalf("a recorded isolated gateway resolved as %q: %s", c.Network, c.networkLine())
	}
	if got := mustBootedNet(t, c.VMName); got != "" {
		t.Fatalf("inspection wrote a posture before reuse: %q", got)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.stops != 0 || live.removes != 0 || live.boots != 0 || live.checks != 1 {
		t.Fatalf("healthy reuse: stops=%d removes=%d boots=%d gateway checks=%d",
			live.stops, live.removes, live.boots, live.checks)
	}
	if got := mustBootedNet(t, c.VMName); got != "isolated" {
		t.Errorf("healthy reuse recorded %q, want isolated", got)
	}
}

func TestLegacyGatewayWithoutReadableSpecNeedsAnExplicitNetwork(t *testing.T) {
	for _, record := range []string{"missing", "unreadable", "stopped"} {
		for _, choice := range []string{"", "isolated", "shared"} {
			t.Run(record+"/network="+choice, func(t *testing.T) {
				p, live, sock := legacySpecSession(t)
				spec := strings.TrimSuffix(sock, ".sock") + ".spec"
				if record == "stopped" {
					// This is the ordinary pre-posture-record lifecycle: stopping
					// an isolated guest removes its spec, without removing the VM.
					if err := live.Stop("brig-ubuntu"); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(spec); !os.IsNotExist(err) {
						t.Fatalf("Stop did not remove the gateway spec: %v", err)
					}
					live.stops = 0 // Only count mutations after the upgrade below.
				} else {
					if err := os.Remove(spec); err != nil {
						t.Fatal(err)
					}
					if record == "unreadable" {
						// Unlike chmod(000), a directory also fails ReadFile when
						// the test suite runs with elevated permissions.
						if err := os.Mkdir(spec, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
				c, err := Load(p, Options{NoProject: true, Network: choice}, live)
				if err != nil {
					t.Fatal(err)
				}
				c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
				if !strings.HasPrefix(c.networkLine(), "unknown") {
					t.Fatalf("an unavailable isolated spec was reported as %q", c.networkLine())
				}
				err = c.EnsureRunning(creds.Set{})
				if choice == "" {
					if err == nil || !strings.Contains(err.Error(), "--network") {
						t.Fatalf("flagless run did not refuse with an explicit remedy: %v", err)
					}
					if live.stops != 0 || live.removes != 0 || live.boots != 0 {
						t.Fatal("missing isolation evidence mutated the existing guest")
					}
					if got := mustBootedNet(t, c.VMName); got != "" {
						t.Errorf("refused recovery recorded %q", got)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				wantStops := 1
				if record == "stopped" {
					wantStops = 0
				}
				if live.stops != wantStops || live.removes == 0 || live.boots != 1 || live.spec.Net != choice {
					t.Errorf("explicit %s: stops=%d removes=%d boots=%d network=%q",
						choice, live.stops, live.removes, live.boots, live.spec.Net)
				}
			})
		}
	}
}
