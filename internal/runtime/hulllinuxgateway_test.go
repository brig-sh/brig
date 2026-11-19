package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// argvLogHull is a hull on Linux that records each command line it is run
// with, one per line, and does nothing else. Every gateway a test needs is up
// before the run, so the only call is `hull run`.
func argvLogHull(t *testing.T) (*hull, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "hull")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+log+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin, linux: true}, log
}

// isolatedGatewayUp leaves a gateway answering for name on its own network,
// recorded as serving it with no rules, which is the one ensureIsolatedGateway
// reuses rather than starts.
func isolatedGatewayUp(t *testing.T, name string) string {
	t.Helper()
	sock := mustSocket(t, name)
	index, err := sandboxNet(name)
	if err != nil {
		t.Fatal(err)
	}
	listenAt(t, sock)
	writeGatewayRecord(sock, fakeGateway(t, sock), gatewaySpec(index, Egress{}))
	return sock
}

// The gateway a `hull run` line names, and the address it takes on it.
func gatewayOnLine(t *testing.T, line string) (sock, cidr string) {
	t.Helper()
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "--gateway-sock":
			sock = f[i+1]
		case "--gateway-cidr":
			cidr = f[i+1]
		}
	}
	if sock == "" || cidr == "" {
		t.Fatalf("the run line joins no gateway: %s", line)
	}
	return sock, cidr
}

// Negative, for the network promise: two isolated sandboxes on hull on Linux
// are each on a gateway of their own, on networks of their own, and neither
// is on the shared gateway. On hull's CNI bridge, where they were before,
// one reached the other's listener and could answer ARP for it.
func TestTwoIsolatedSandboxesOnLinuxHullShareNoGateway(t *testing.T) {
	overlayHost(t)
	scratchIsolatedDir(t)
	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	h, log := argvLogHull(t)
	want := map[string]string{}
	for _, name := range []string{"brig-a", "brig-b"} {
		want[name] = isolatedGatewayUp(t, name)
		spec := linuxBootSpec()
		spec.Name, spec.Net = name, "isolated"
		if err := h.Run(spec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	blob, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(blob)), "\n")
	if len(calls) != 2 {
		t.Fatalf("hull was run %d times, want twice: %q", len(calls), calls)
	}
	cidrs := map[string]bool{}
	for i, name := range []string{"brig-a", "brig-b"} {
		if !strings.HasPrefix(calls[i], "run --detach --name "+name+" --net shared ") {
			t.Errorf("%s was run as: %s", name, calls[i])
		}
		sock, cidr := gatewayOnLine(t, calls[i])
		if sock != want[name] {
			t.Errorf("%s joined %s, want its own gateway %s", name, sock, want[name])
		}
		if sock == shared {
			t.Errorf("%s, isolated, joined the shared gateway", name)
		}
		cidrs[cidr] = true
	}
	if len(cidrs) != 2 {
		t.Errorf("the two isolated sandboxes took addresses on one network: %v", cidrs)
	}
}

// Negative: `brig rm --all` never stops a shared gateway on hull on Linux
// that it cannot ask for its guests, as it cannot ask one from a hull older
// than GET /members. The process table does not show them there (see
// PruneSharedNetwork), so stopping it would take the network of every shared
// sandbox still running.
func TestLinuxHullNeverStopsASharedGatewayItCannotAsk(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := startHelperGateway(t, sock, true, false)
	if noInstancesHull(t).PruneSharedNetwork() {
		t.Error("PruneSharedNetwork reported the shared gateway stopped")
	}
	if !ownsGateway(pid, sock) || !gatewayReachable(sock) {
		t.Error("the shared gateway was stopped under the guests it may still serve")
	}
}

// Negative, for the same reason: a boot on hull on Linux reuses an old shared
// gateway it cannot see guests on, rather than stopping it as one nothing is
// on, which the macOS hull does.
func TestALinuxHullBootKeepsAnOldSharedGateway(t *testing.T) {
	sock := scratchSharedGateway(t)
	old := startHelperGateway(t, sock, false, false)
	bin, _ := fakeRuntime(t)
	got, release, err := ensureSharedGateway(bin, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got != sock || !ownsGateway(old, sock) {
		t.Errorf("the old shared gateway was not kept: %s, still up %v", got, ownsGateway(old, sock))
	}
}

// hull on Linux puts a networked sandbox on a gateway whatever BRIG_HYPERVISOR
// says, so a sandbox whose isolated gateway is up reads as isolated with no
// hypervisor named, and as stale when shared is asked for. The macOS hull
// with no hypervisor is on vz, where there is no gateway to compare.
func TestLinuxHullNetworkStaleWhateverTheHypervisor(t *testing.T) {
	scratchIsolatedDir(t)
	isolatedGatewayUp(t, "brig-s")
	for _, hv := range []string{"", "vz", "hvi"} {
		if linuxHull().NetworkStale("brig-s", hv, "isolated", Egress{}) {
			t.Errorf("%q: an isolated sandbox on its own gateway reads as stale", hv)
		}
		if !linuxHull().NetworkStale("brig-s", hv, "shared", Egress{}) {
			t.Errorf("%q: a sandbox on its own gateway reads as shared", hv)
		}
	}
	if (&hull{bin: "hull"}).NetworkStale("brig-s", "", "shared", Egress{}) {
		t.Error("the macOS hull on vz compared a gateway it does not use")
	}
}

// hull on Linux prints the gateway's control socket for a sandbox on one. The
// sandbox's own gateway, recorded as serving it, is isolated; the shared one is
// shared. A sandbox gateway with no record could be either, and so is unknown.
func TestLinuxHullSandboxNetworkReadsTheGatewaySocket(t *testing.T) {
	const name = "brig-legacy"
	scratchIsolatedDir(t)
	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	own := isolatedGatewayUp(t, name)
	unrecorded := mustSocket(t, "brig-other")
	record := func(sock string) string {
		fixture, _ := json.Marshal(map[string]any{"id": name, "status": "running", "cmdLine": nil,
			"network": "gateway", "gatewaySocket": sock})
		return string(fixture)
	}
	for _, tt := range []struct{ name, sock, want string }{
		{"its own gateway", own, "isolated"},
		{"the shared gateway", shared, "shared"},
		{"a sandbox gateway with no record", unrecorded, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &hull{bin: networkInspectFixture(t, record(tt.sock), "", 0, "", 0), linux: true}
			got, err := h.SandboxNetwork(name)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("SandboxNetwork = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// brig policy check asks through EgressServed, and answers as the run does:
// an egress-only policy is served on hull on Linux on hvi, from its own row,
// and refused as unenforced on any other monitor. A route mode is still
// refused there, and the macOS rows answer as before.
func TestEgressServedOnHullOnLinuxOnlyOnHvi(t *testing.T) {
	spec := RunSpec{Net: "isolated", Egress: Egress{Default: "deny"}}
	for _, hv := range []string{"", "hvi", "vz"} {
		if err := EgressServed(linuxHull(), hv, spec); err != nil {
			t.Errorf("%q: an egress policy was refused on hull on Linux: %v", hv, err)
		}
		if got := RoutePath(linuxHull(), hv); got != linuxHullPath {
			t.Errorf("%q: brig policy check reads %s, not %s", hv, got, linuxHullPath)
		}
	}
	for _, hv := range []string{"qemu", "cloud-hypervisor"} {
		var ce *CapabilityError
		if err := EgressServed(linuxHull(), hv, spec); !errors.As(err, &ce) || ce.Path != linuxHullPath {
			t.Errorf("%q: an egress policy was not refused on %s: %v", hv, linuxHullPath, err)
		}
	}
	if err := EgressServed(&hull{bin: "hull"}, "hvi", spec); err != nil {
		t.Errorf("the macOS hull on hvi: %v", err)
	}
	var refusal *CapabilityError
	if err := EgressServed(&hull{bin: "hull"}, "vz", spec); !errors.As(err, &refusal) {
		t.Errorf("the macOS hull on vz: %v, want a capability refusal", err)
	}
	if err := EgressServed(&hull{bin: "hull"}, "hvi", spec); err != nil {
		t.Errorf("the macOS hull on hvi: %v", err)
	}
	// Route mode stays refused on hull on Linux.
	if err := RequireRouteMode(v1alpha1.ModeEndpoint, linuxHullPath); !errors.As(err, &refusal) {
		t.Errorf("route mode on hull on Linux: %v, want a capability refusal", err)
	}
}

// bootedOn reports whether the double was asked to boot a guest, which is a
// `hull run` line. Other lines merely contain the word.
func bootedOn(d *hullDouble) bool {
	return strings.Contains("\n"+d.calls(), "\nrun ")
}

// Negative, for the network promise: a policy-bound sandbox on hull on Linux
// is on a gateway of its own that holds the rules, as on hvi. The gateway was
// asked whether it takes the rules before the boot, and BRIG_HYPERVISOR does
// not change any of it, since the guest boots on urunc whatever it says.
func TestLinuxHullBootsAPolicyOnItsOwnGateway(t *testing.T) {
	for _, hv := range []string{"", "vz", "hvi"} {
		t.Run("hypervisor="+hv, func(t *testing.T) {
			h, d := newLinuxHullDouble(t, egressHelp, "0")
			shared, err := gatewaySocket()
			if err != nil {
				t.Fatal(err)
			}
			spec := linuxBootSpec()
			spec.Net, spec.Hypervisor = "shared", hv
			spec.Egress = Egress{Default: "deny", Allow: []Rule{{Host: "api.anthropic.com"}}}
			if err := h.Run(spec); err != nil {
				t.Fatalf("a policy on hull on Linux did not boot: %v", err)
			}
			if !bootedOn(d) {
				t.Fatalf("hull was never asked to boot: %s", d.calls())
			}
			for _, want := range []string{"version --json", "network-gateway --help"} {
				if !d.called(want) {
					t.Errorf("the gateway was not asked %q before the boot: %s", want, d.calls())
				}
			}
			var start, run string
			for _, line := range strings.Split(strings.TrimSpace(d.calls()), "\n") {
				switch {
				case strings.HasPrefix(line, "network-gateway ") && !strings.Contains(line, "--help"):
					start = line
				case strings.HasPrefix(line, "run "):
					run = line
				}
			}
			for _, want := range []string{"--egress-default deny", "--egress-allow host=api.anthropic.com"} {
				if !strings.Contains(start, want) {
					t.Errorf("the gateway was started without %q: %q", want, start)
				}
			}
			sock, _ := gatewayOnLine(t, run)
			if sock == shared {
				t.Errorf("a policy-bound sandbox joined the shared gateway %s", sock)
			}
			if !strings.HasPrefix(filepath.Base(sock), "sandbox-") || !strings.HasSuffix(sock, ".sock") {
				t.Errorf("the sandbox joined %s, want a gateway of its own (sandbox-*.sock)", sock)
			}
		})
	}
}
