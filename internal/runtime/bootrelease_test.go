package runtime

import (
	"os"
	"testing"
)

// orphanedIsolatedGateway leaves an isolated gateway up for name, recorded
// with a policy, in the gateway directory sharedGatewayAt set up. That is what
// a restart onto shared finds when the stop or removal of the policy-isolated
// instance before it failed and never released its gateway.
func orphanedIsolatedGateway(t *testing.T, name string) (int, string) {
	t.Helper()
	sock := mustSocket(t, name)
	index, err := sandboxNet(name)
	if err != nil {
		t.Fatal(err)
	}
	listenAt(t, sock)
	pid := fakeGateway(t, sock)
	writeGatewayRecord(sock, pid, gatewaySpec(index, Egress{Default: "deny", Allow: []Rule{{Host: "a.example"}}}))
	return pid, sock
}

// A sandbox booted on the shared network must not leave an isolated gateway
// answering under its name. NetworkStale reads that gateway as the posture of
// the guest that is up, so brig info would name `isolated` for a sandbox with
// no isolation at all, and nothing but `brig reset` would ever correct it.
func TestASharedBootStopsTheIsolatedGatewayLeftUnderItsName(t *testing.T) {
	const sandbox = "brig-claude-web"
	sharedGatewayAt(t, sandbox, 5)
	pid, sock := orphanedIsolatedGateway(t, sandbox)

	h := &hull{bin: stubRuntimeBin(t, "", 0)}
	if !h.NetworkStale(sandbox, "hvi", "shared", Egress{}) {
		t.Fatal("the leftover gateway does not read as isolated, so this test proves nothing")
	}
	if err := h.Run(RunSpec{Name: sandbox, Image: "img", Hypervisor: "hvi"}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	waitGone(t, pid, sock)
	if h.NetworkStale(sandbox, "hvi", "shared", Egress{}) {
		t.Error("a sandbox booted shared still reads as isolated")
	}
}

// Released only once the boot has worked. A `hull run` that fails can be one
// refused because the old instance is still up under the name, after a `hull
// rm` that failed, and that guest is still behind the gateway.
func TestAFailedSharedBootLeavesTheIsolatedGatewayUp(t *testing.T) {
	const sandbox = "brig-claude-web"
	sharedGatewayAt(t, sandbox, 5)
	pid, sock := orphanedIsolatedGateway(t, sandbox)

	h := &hull{bin: stubRuntimeBin(t, "Error: an instance named brig-claude-web exists", 1)}
	if err := h.Run(RunSpec{Name: sandbox, Image: "img", Hypervisor: "hvi"}); err == nil {
		t.Fatal("a runtime that exited non-zero must fail the boot")
	}
	if !ownsGateway(pid, sock) {
		t.Error("a failed boot stopped the gateway another guest may still be behind")
	}
	if _, err := os.Stat(gatewayPIDPath(sock)); err != nil {
		t.Errorf("a failed boot dropped the gateway's record: %v", err)
	}
}
