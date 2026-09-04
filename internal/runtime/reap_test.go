package runtime

import (
	"os"
	"path/filepath"
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
	writeGatewayRecord(sock, pid, gatewaySpec(0))

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
	writeGatewayRecord(sock, pid, gatewaySpec(0))
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

	_, err := ensureIsolatedGateway("hull", name, 1)
	if err == nil {
		t.Fatal("a gateway that could not be replaced was reported ready")
	}
	if _, statErr := os.Stat(gatewaySpecPath(sock)); statErr == nil {
		t.Error("a spec was recorded for rules no running gateway received")
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
