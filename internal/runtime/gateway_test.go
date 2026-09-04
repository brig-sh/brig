package runtime

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The socket a gateway is reached on names the network it serves, because
// ensureGateway reuses whatever is already listening there without asking what
// it is serving. Moving the subnet while a gateway from the old one is still
// running would otherwise reuse it: brig would hand new guests an address on
// the new network while the process on the other end still routed the old one,
// and the sandbox would come up with no working network and nothing to point
// at. A subnet in the name means a different network is a different socket.
func TestGatewaySocketNamesTheSubnet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("BRIG_GATEWAY_SOCK", "")

	sock, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(sock), socketTag(gatewaySubnet)) {
		t.Errorf("the socket name does not carry the subnet %s: %s", gatewaySubnet, sock)
	}
	// The property that matters: a different network is a different socket, so
	// a gateway serving the old one is never picked up for guests on the new.
	if socketTag("10.87.0.0/24") == socketTag(gatewaySubnet) {
		t.Error("two different subnets produce the same socket name")
	}
	// And the tag has to survive being a filename.
	if strings.ContainsAny(socketTag(gatewaySubnet), "/") {
		t.Errorf("the socket tag is not filename-safe: %s", socketTag(gatewaySubnet))
	}
	// An explicit override still wins, unchanged: it is how a test, or someone
	// running two brigs, points at a gateway of their own.
	t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(dir, "mine.sock"))
	if sock, err = gatewaySocket(); err != nil || filepath.Base(sock) != "mine.sock" {
		t.Errorf("BRIG_GATEWAY_SOCK was not honoured: %s (%v)", sock, err)
	}
}

// hull's --net takes none or shared and nothing else. Passing "isolated"
// through was what made the posture a no-op on this backend: hull reads any
// value other than "none" as networked, so the flag was accepted and the
// sandbox joined the shared gateway anyway.
func TestHullNetIsOnlyNoneOrShared(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"none", "none"},
		{"shared", "shared"},
		{"isolated", "shared"},
	} {
		if got := hullNet(tt.in); got != tt.want {
			t.Errorf("hullNet(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The default is unchanged: a sandbox that asked for nothing is on the shared
// network, which is the same sandbox it was before the isolated posture did
// anything. Worth a test of its own rather than an implication of several --
// the failure mode of getting it wrong is every sandbox on the host moving at
// once.
func TestTheDefaultPostureIsStillShared(t *testing.T) {
	spec := RunSpec{Name: "brig-s", Image: "img"}
	if got := orDefault(spec.Net, "shared"); got != "shared" {
		t.Errorf("the default posture is %q, want shared", got)
	}
	if got := hullNet(orDefault(spec.Net, "shared")); got != "shared" {
		t.Errorf("a default run asks hull for %q, want shared", got)
	}
}

// A gateway nobody recorded a spec for never matches, so it is replaced rather
// than reused. A gateway nobody can say what it serves is one nobody can say
// is the right one.
func TestUnrecordedSpecMatchesNothing(t *testing.T) {
	scratchIsolatedDir(t)
	sock, err := isolatedSocket("s")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordedSpec(sock); got != "" {
		t.Fatalf("a gateway with no record reported spec %q", got)
	}
}

func TestGatewayRecordRoundTrips(t *testing.T) {
	scratchIsolatedDir(t)
	sock, err := isolatedSocket("s")
	if err != nil {
		t.Fatal(err)
	}
	want := gatewaySpec(0)
	writeGatewayRecord(sock, 4242, want)

	if got := recordedSpec(sock); got != want {
		t.Errorf("recorded spec = %q, want %q", got, want)
	}
	pid, ok := gatewayPID(sock)
	if !ok || pid != 4242 {
		t.Errorf("recorded pid = %d (ok=%t), want 4242", pid, ok)
	}
}

// A gateway serving another subnet would route the guest nowhere, which is the
// hazard the shared socket carries its subnet in its name to avoid. The
// isolated one carries it in its record instead.
func TestGatewaySpecDistinguishesNetworks(t *testing.T) {
	if gatewaySpec(0) == gatewaySpec(1) {
		t.Error("two networks produced the same spec")
	}
	if !strings.Contains(gatewaySpec(1), sandboxSubnet(1)) {
		t.Errorf("the spec does not name its subnet: %q", gatewaySpec(1))
	}
}

// A recorded pid is not enough to kill on: a gateway that died takes its pid
// back into circulation. pid 1 is the clearest available stand-in for "some
// other process now holds it".
func TestOwnsGatewayRejectsAnUnrelatedProcess(t *testing.T) {
	if ownsGateway(1, "/tmp/sandbox-s.sock") {
		t.Error("pid 1 was mistaken for a gateway")
	}
}

// Shutting down a sandbox that never had an isolated gateway is a no-op, not
// an error: it is on the path of every `brig rm`, including one for a sandbox
// that ran on the shared network.
func TestShutDownGatewayToleratesNoRecord(t *testing.T) {
	scratchIsolatedDir(t)
	shutDownGateway("never-isolated")
}

// A unix socket path is bounded, and the sandbox name is not brig's to bound.
// Over the limit the bind fails, and what the user sees is a gateway that
// never came up -- with nothing about the length in the message.
func TestIsolatedSocketFitsInASockaddr(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", "/Users/somebody/.brig")
	long := "brig-" + strings.Repeat("a-very-long-workspace-name", 8)

	got, err := isolatedSocket(long)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(qemuGatewaySocket(got)); n > sockaddrUnMax {
		t.Fatalf("path is %d bytes, over the %d a unix socket fits: %s", n, sockaddrUnMax, got)
	}
	// The same path every time, or the sandbox would not find the gateway it
	// had a moment ago.
	again, err := isolatedSocket(long)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Errorf("a shortened socket path moved between calls: %s then %s", got, again)
	}
	// And two long names still get two gateways.
	other, err := isolatedSocket(long + "-other")
	if err != nil {
		t.Fatal(err)
	}
	if other == got {
		t.Errorf("two sandboxes were shortened onto one socket: %s", got)
	}
}

// A directory so deep that even a shortened name will not fit is reported,
// rather than left to fail as a gateway that did not come up.
func TestIsolatedSocketReportsAnImpossibleDirectory(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", "/"+strings.Repeat("d", sockaddrUnMax))
	if _, err := isolatedSocket("s"); err == nil {
		t.Fatal("a directory too deep for any socket was accepted")
	} else if !strings.Contains(err.Error(), "BRIG_GATEWAY_DIR") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// One socket per sandbox is what makes the gateways separable at all: two
// sandboxes sharing a path would share the gateway.
func TestIsolatedSocketIsPerSandbox(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_DIR", "/custom")
	first, err := isolatedSocket("one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := isolatedSocket("two")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two sandboxes share a gateway socket: %s", first)
	}
	// And neither is the shared gateway's, which is named for its network.
	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	if first == shared || second == shared {
		t.Error("an isolated sandbox was pointed at the shared gateway's socket")
	}
}

// listenAt makes a socket answer, which is the whole of what gatewayReachable
// asks. A listener rather than a real gateway: what is under test is the
// comparison, and a gateway would only be a slower way to have something
// accept a connection.
func listenAt(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", qemuGatewaySocket(sock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

// The one path that skips the boot is the one where a changed posture would go
// unnoticed: brig finds the sandbox running, returns, and reports a network
// that sandbox is not on.
func TestNetworkStale(t *testing.T) {
	for _, tt := range []struct {
		name string
		// booted is the network index the running sandbox's gateway serves;
		// -1 means it has none, which is the shared network.
		booted int
		net    string
		want   bool
	}{
		{"shared then shared", -1, "shared", false},
		{"shared, then isolated asked for", -1, "isolated", true},
		{"isolated, unchanged", 0, "isolated", false},
		{"isolated, then shared asked for", 0, "shared", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scratchIsolatedDir(t)
			const name = "brig-s"
			if tt.booted >= 0 {
				sock := mustSocket(t, name)
				index, err := sandboxNet(name)
				if err != nil {
					t.Fatal(err)
				}
				listenAt(t, sock)
				writeGatewayRecord(sock, os.Getpid(), gatewaySpec(index))
			}
			h := &hull{bin: "hull"}
			if got := h.NetworkStale(name, "hvi", tt.net); got != tt.want {
				t.Errorf("NetworkStale = %t, want %t", got, tt.want)
			}
		})
	}
}

// Only hvi has a gateway brig owns. On vz the network comes from vmnet, which
// brig neither chose nor can inspect, so it has nothing to compare and must
// not answer "stale" -- that would restart a healthy sandbox on every run.
func TestNetworkStaleSaysNoWhereBrigOwnsNoNetwork(t *testing.T) {
	scratchIsolatedDir(t)
	h := &hull{bin: "hull"}

	for _, hv := range []string{"vz", "qemu", ""} {
		if h.NetworkStale("brig-s", hv, "isolated") {
			t.Errorf("hypervisor %q: reported stale with no network of brig's to compare", hv)
		}
	}
	if h.NetworkStale("brig-s", "hvi", "none") {
		t.Error("an offline sandbox was reported stale")
	}
}

// A sandbox whose gateway died is not "on the wrong network" -- it has no
// gateway at all, and the run that follows starts one. What matters is that
// this does not read the dead gateway's record as current.
func TestNetworkStaleIgnoresARecordWithNothingListening(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-s"
	sock := mustSocket(t, name)
	index, err := sandboxNet(name)
	if err != nil {
		t.Fatal(err)
	}
	writeGatewayRecord(sock, os.Getpid(), gatewaySpec(index))

	h := &hull{bin: "hull"}
	if h.NetworkStale(name, "hvi", "shared") {
		t.Error("a dead gateway's record was read as the sandbox's current network")
	}
}
