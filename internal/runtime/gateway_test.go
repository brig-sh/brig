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
	want := gatewaySpec(0, Egress{})
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
	if gatewaySpec(0, Egress{}) == gatewaySpec(1, Egress{}) {
		t.Error("two networks produced the same spec")
	}
	if !strings.Contains(gatewaySpec(1, Egress{}), sandboxSubnet(1)) {
		t.Errorf("the spec does not name its subnet: %q", gatewaySpec(1, Egress{}))
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

// The policy reaches the gateway as flags, and the gateway reads them once at
// startup. A rule dropped here is a rule nothing enforces, which the sandbox
// has no way to notice.
func TestEgressArgsCarryEveryRule(t *testing.T) {
	got := strings.Join(Egress{
		Default: "deny",
		Allow:   []Rule{{Host: "*.anthropic.com"}, {CIDR: "10.0.0.0/8"}},
		Deny:    []Rule{{Host: "metadata.google.internal"}},
	}.args(), " ")

	for _, want := range []string{
		"--egress-default deny",
		"--egress-allow host=*.anthropic.com",
		"--egress-allow cidr=10.0.0.0/8",
		"--egress-deny host=metadata.google.internal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from the gateway command line: %s", want, got)
		}
	}
}

// No policy must mean no flags rather than an empty one: --egress-default is
// what turns filtering on, and passing it with no rules under `deny` would
// give a sandbox that reaches nothing at all.
func TestEgressArgsAreEmptyWithoutAPolicy(t *testing.T) {
	if got := (Egress{}).args(); len(got) != 0 {
		t.Errorf("an unfiltered sandbox passed egress flags: %v", got)
	}
	if got := (Egress{Allow: []Rule{{Host: "example.com"}}}).args(); len(got) != 0 {
		t.Errorf("rules with no default were passed to the gateway: %v", got)
	}
}

// The default is open, and stays open.
//
// A sandbox nobody attached a policy to is unfiltered and on the shared
// network: the same sandbox it was before any of this existed. That is the
// whole of what an ordinary `brig run` gets, and it is worth a test of its own
// rather than an implication of several -- a tool people cannot use is not
// safer than one they can, and the failure mode of getting this wrong is every
// sandbox on the host losing its network at once.
func TestASandboxWithNoPolicyIsUnfilteredAndShared(t *testing.T) {
	// The zero value of the spec, which is what a run with nothing attached
	// builds: no rules, and the empty posture that orDefault reads as shared.
	spec := RunSpec{Name: "brig-s", Image: "img"}

	if spec.Egress.Filtered() {
		t.Error("a sandbox with no policy asked for filtering")
	}
	if got := spec.Egress.args(); len(got) != 0 {
		t.Errorf("a sandbox with no policy passed egress flags to the gateway: %v", got)
	}
	if isolatedNet(orDefault(spec.Net, "shared"), spec.Egress) {
		t.Error("a sandbox with no policy was given a network of its own")
	}
	for _, hv := range []string{"vz", "hvi", "qemu"} {
		if err := supports(spec, hv); err != nil {
			t.Errorf("a sandbox with no policy was refused on %s: %v", hv, err)
		}
	}
}

// And an empty rule list is not a policy either. A default of "" is what turns
// filtering off; reading an empty allow list as a policy would give the
// strictest sandbox there is -- one that reaches nothing -- to anyone who
// attached a document and left it blank.
func TestAnEmptyRuleListIsNotAPolicy(t *testing.T) {
	for _, e := range []Egress{
		{},
		{Allow: []Rule{}, Deny: []Rule{}},
		{Allow: []Rule{{Host: "example.com"}}},
	} {
		if e.Filtered() {
			t.Errorf("%+v was read as a policy", e)
		}
		if got := e.args(); len(got) != 0 {
			t.Errorf("%+v passed flags to the gateway: %v", e, got)
		}
	}
}

// The spec decides whether a running gateway is reused or replaced. It has to
// see through cosmetic edits and not through real ones.
func TestGatewaySpec(t *testing.T) {
	base := Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}, {Host: "b.example"}}}

	reordered := Egress{Default: "deny", Allow: []Rule{{Host: "b.example"}, {Host: "a.example"}}}
	if gatewaySpec(0, base) != gatewaySpec(0, reordered) {
		t.Error("reordering two allow rules was treated as a different policy")
	}

	for _, tt := range []struct {
		name  string
		index int
		e     Egress
	}{
		{"the default flipped", 0, Egress{Default: "allow", Allow: base.Allow}},
		{"a rule moved to deny", 0, Egress{Default: "deny",
			Allow: []Rule{{Host: "a.example"}}, Deny: []Rule{{Host: "b.example"}}}},
		{"a rule dropped", 0, Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}}}},
		{"no policy at all", 0, Egress{}},
		// The network too, not only the rules: a gateway serving another
		// subnet would route the guest nowhere, which is the hazard the
		// shared socket carries its subnet in its name to avoid.
		{"a different network", 1, base},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if gatewaySpec(0, base) == gatewaySpec(tt.index, tt.e) {
				t.Errorf("%s was treated as the same gateway", tt.name)
			}
		})
	}
}

// The egress flags arrived after hull 0.1.0-rc21, so brig can meet a runtime
// that does not take them. Rules sent to one anyway reach a gateway that
// refuses the flag, and the user sees a network that did not come up.
func TestGatewayEnforcesChecksTheRuntimeTakesRules(t *testing.T) {
	old := fakeHull(t, "OPTIONS:\n   --socket string   control socket path\n")
	err := gatewayEnforces(old)
	if err == nil {
		t.Fatal("a runtime with no egress flags was accepted")
	}
	for _, want := range []string{"--egress-default", "detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say what is missing or what to do: %v", err)
		}
	}

	current := fakeHull(t, "OPTIONS:\n   --egress-default string   verdict\n")
	if err := gatewayEnforces(current); err != nil {
		t.Errorf("a runtime that takes the flags was refused: %v", err)
	}
}

// Nothing is concluded from a probe that could not run. A runtime broken
// enough not to print its own help fails the boot below with better context
// than a guess made here.
func TestGatewayEnforcesConcludesNothingFromAFailedProbe(t *testing.T) {
	if err := gatewayEnforces(filepath.Join(t.TempDir(), "not-a-runtime")); err != nil {
		t.Errorf("a failed probe was read as a missing feature: %v", err)
	}
}

// fakeHull writes a script that answers `network-gateway --help` with the
// given text, which is the whole of what the probe reads.
func fakeHull(t *testing.T, help string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\ncat <<'EOF'\n" + help + "EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// An egress policy on a backend that cannot enforce it is refused rather than
// ignored. Booting anyway would give a sandbox that reports a policy and
// enforces nothing, which is worse than no policy: someone would rely on it.
func TestSupportsRefusesAPolicyOffHvi(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", Egress: Egress{Default: "deny"}}

	for _, hv := range []string{"vz", "qemu"} {
		err := supports(spec, hv)
		if err == nil {
			t.Fatalf("a policy was accepted on %s, which cannot enforce it", hv)
		}
		if !strings.Contains(err.Error(), "hvi") {
			t.Errorf("the error does not say which backend enforces it: %v", err)
		}
	}
	if err := supports(spec, "hvi"); err != nil {
		t.Errorf("a policy was refused on the backend that enforces it: %v", err)
	}
	// And a sandbox with no policy is unaffected on every backend.
	for _, hv := range []string{"vz", "hvi", "qemu"} {
		if err := supports(RunSpec{Name: "s", Image: "img"}, hv); err != nil {
			t.Errorf("a sandbox with no policy was refused on %s: %v", hv, err)
		}
	}
}

// A policy takes the posture with it: the rules live on a gateway and cover
// every member of its network, so a sandbox answering to rules of its own
// cannot be sharing one.
func TestIsolatedNet(t *testing.T) {
	for _, tt := range []struct {
		name   string
		net    string
		policy Egress
		want   bool
	}{
		{"shared, no policy", "shared", Egress{}, false},
		{"isolated, no policy", "isolated", Egress{}, true},
		{"shared, with a policy", "shared", Egress{Default: "deny"}, true},
		{"isolated, with a policy", "isolated", Egress{Default: "allow"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isolatedNet(tt.net, tt.policy); got != tt.want {
				t.Errorf("isolatedNet(%q, %+v) = %t, want %t", tt.net, tt.policy, got, tt.want)
			}
		})
	}
}
