package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record is the only handle anything has on a gateway process, so it is
// removed only once that process is confirmed gone. Removing it unconditionally
// was worse than leaving it: PruneNetworks sweeps by the .pid files, so a
// record dropped while its gateway still ran put that gateway beyond the reach
// of `brig reset`, of a later `brig stop` and of this function.
//
// What makes "gone" a fact rather than a request is the escalation. A gateway
// that ignores SIGTERM stands in for hull outliving the grace period; without
// the SIGKILL behind it, this would report success and leave it running.
func TestStopEscalatesWhenSigtermIsIgnored(t *testing.T) {
	scratchIsolatedDir(t)
	sock := mustSocket(t, "brig-stubborn")
	pid := spawnGateway(t, sock, "trap '' TERM\nsleep 60\n")
	writeGatewayRecord(sock, pid, gatewaySpec(0, Egress{}))

	if !stopGatewayAt(sock) {
		t.Fatal("a gateway that ignores SIGTERM was not stopped")
	}
	if ownsGateway(pid, sock) {
		t.Error("the process survived a stop that reported success")
	}
	if _, err := os.Stat(gatewayPIDPath(sock)); !os.IsNotExist(err) {
		t.Error("the record outlived the gateway it names")
	}
}

// A gateway that stops takes its records and its log with it. Nothing else
// removes the log, so ~/.brig would otherwise grow one file for every sandbox
// that was ever isolated and keep it for ever.
func TestStopClearsTheRecordsAndTheLog(t *testing.T) {
	scratchIsolatedDir(t)
	sock := mustSocket(t, "brig-tidy")
	pid := fakeGateway(t, sock)
	writeGatewayRecord(sock, pid, gatewaySpec(0, Egress{}))
	if err := os.WriteFile(gatewayLogPath(sock), []byte("boot noise\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !stopGatewayAt(sock) {
		t.Fatal("a stoppable gateway was not reported stopped")
	}
	for _, path := range []string{gatewayPIDPath(sock), gatewaySpecPath(sock), gatewayLogPath(sock)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s outlived the gateway", path)
		}
	}
}

// A gateway that must be replaced has to be gone before the replacement is
// started. startGateway judges success on the socket answering, and a surviving
// old process answers it -- so the boot would run under the old rules while the
// new ones sat in the spec file beside it, which NetworkStale then reads as
// current and never corrects.
func TestReplacingAGatewayThatCannotBeStoppedIsRefused(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-live"
	sock := mustSocket(t, name)

	// Something is listening and it is not ours: no record, so shutDownGateway
	// has nothing to signal and the socket keeps answering.
	listenAt(t, sock)

	// A runtime that passes the probe, so the refusal under test is the one
	// for the gateway that stayed up, and not the probe's. A bare "hull" here
	// failed the probe wherever hull is not installed.
	bin := fakeHull(t, "OPTIONS:\n   --egress-default string   verdict\n")
	_, err := ensureIsolatedGateway(bin, name, 0, Egress{Default: "deny"})
	if err == nil {
		t.Fatal("a gateway that could not be replaced was reported ready")
	}
	if strings.Contains(err.Error(), "network-gateway --help") {
		t.Errorf("the boot stopped at the probe, before the replacement: %v", err)
	}
	if _, statErr := os.Stat(gatewaySpecPath(sock)); statErr == nil {
		t.Error("a spec was recorded for rules no running gateway received")
	}
}

// Negative: an isolated gateway that is replaced counts as stopped only once
// it no longer listens. Its argv goes before its sockets do. A stop that took
// the argv as the answer found the socket still accepting, and refused the
// boot as one whose old gateway could not be stopped.
func TestAnIsolatedGatewayThatIsSlowToExitIsStillReplaced(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-slow"
	sock := mustSocket(t, name)
	t.Cleanup(func() { killGatewaysOn(sock) })
	old := startSlowExitGateway(t, sock, true)
	writeGatewayRecord(sock, old, gatewaySpec(0, Egress{Default: "deny"}))
	bin, _ := fakeRuntime(t)

	if _, err := ensureIsolatedGateway(bin, name, 0, Egress{}); err != nil {
		t.Fatalf("replacing a gateway that was exiting failed: %v", err)
	}
	if got, want := recordedSpec(sock), gatewaySpec(0, Egress{}); got != want {
		t.Errorf("the record says %q, want %q", got, want)
	}
	if pid, ok := gatewayPID(sock); !ok || pid == old {
		t.Errorf("the record names pid %d (ok=%t), want the new gateway", pid, ok)
	}
	if gws := gatewaysOn(t, sock); len(gws) != 1 {
		t.Fatalf("want one gateway on the socket, got %q", gws)
	}
}

// Remove frees the sandbox's address and its /30 only when the removal
// succeeded. A `hull rm` that failed leaves a sandbox that may still be
// running, and putting its network back in the pool hands it to the next
// sandbox to boot -- two guests on one address, which is the failure the
// allocator exists to prevent.
func TestRemoveKeepsTheNetworkOfASandboxItCouldNotRemove(t *testing.T) {
	scratchIsolatedDir(t)
	const name = "brig-stuck"

	index, err := sandboxNet(name)
	if err != nil {
		t.Fatal(err)
	}
	cidr, err := gatewayCIDR(name)
	if err != nil {
		t.Fatal(err)
	}

	// A runtime binary that fails every verb, which is what `hull rm` refusing
	// to remove a running sandbox looks like from here.
	h := &hull{bin: filepath.Join(t.TempDir(), "not-a-runtime")}
	if err := h.Remove(name); err == nil {
		t.Fatal("the failing removal reported success")
	}

	if got, ok := lookupSandboxNet(name); !ok || got != index {
		t.Errorf("the network of a sandbox that was not removed was freed: %d (ok=%t), want %d",
			got, ok, index)
	}
	again, err := gatewayCIDR(name)
	if err != nil {
		t.Fatal(err)
	}
	if again != cidr {
		t.Errorf("the address moved after a failed removal: %s then %s", cidr, again)
	}
}
