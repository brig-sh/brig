package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGateway starts a process whose argv reads as a gateway on sock, and
// returns its pid.
//
// A real process, because that is what the code under test decides on: it
// signals a pid and reads argv back to check the pid is still the gateway it
// recorded. A stub that answered those two questions would be a stub of the
// answer. The script sleeps; nothing here needs it to serve anything.
func fakeGateway(t *testing.T, sock string) int {
	t.Helper()
	return spawnGateway(t, sock, "sleep 60\n")
}

// spawnGateway runs body as a process whose argv reads as a gateway on sock.
func spawnGateway(t *testing.T, sock, body string) int {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	// The words land in argv, which is the whole of what ownsGateway reads.
	cmd := exec.Command("/bin/sh", script, "network-gateway", "--socket", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// ps has to be able to see it before the test asks anything about it.
	for range 50 {
		if ownsGateway(pid, sock) {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the fake gateway (pid %d) never showed the right argv", pid)
	return 0
}

func recordGateway(t *testing.T, name string, pid int) string {
	t.Helper()
	sock, err := isolatedSocket(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	writeGatewayRecord(sock, pid, gatewaySpec(0))
	return sock
}

// A sandbox that dies without a `brig stop` leaves its gateway behind. Stop
// and rm clear their own, and nothing else would, so this is the only thing
// standing between that and a host accumulating 28.7 MB per crashed sandbox.
func TestPruneNetworksStopsTheGatewayOfAGoneSandbox(t *testing.T) {
	scratchIsolatedDir(t)

	livePID := fakeGateway(t, mustSocket(t, "brig-live"))
	gonePID := fakeGateway(t, mustSocket(t, "brig-gone"))
	recordGateway(t, "brig-live", livePID)
	goneSock := recordGateway(t, "brig-gone", gonePID)

	h := &hull{bin: "hull"}
	if got := h.PruneNetworks([]string{"brig-live"}); got != 1 {
		t.Errorf("pruned %d, want 1", got)
	}

	// The live one is untouched: its sandbox is still running, and taking its
	// network away would be the pruner causing the outage it exists to clean
	// up after.
	if !ownsGateway(livePID, mustSocket(t, "brig-live")) {
		t.Error("the gateway of a live sandbox was stopped")
	}
	if _, err := os.Stat(gatewayPIDPath(goneSock)); !os.IsNotExist(err) {
		t.Error("the record of the pruned gateway was left behind")
	}
	waitGone(t, gonePID, goneSock)
}

// The pruner works from the records on disk, so it must not signal a pid that
// is no longer the gateway that was recorded: a dead gateway takes its pid
// back into circulation, and the process holding it next is not brig's to kill.
func TestPruneNetworksLeavesAReusedPidAlone(t *testing.T) {
	scratchIsolatedDir(t)

	// A pid that is running but is not a gateway. The test's own process is
	// the one pid guaranteed to be alive and guaranteed not to be one.
	sock := recordGateway(t, "brig-recycled", os.Getpid())

	h := &hull{bin: "hull"}
	if got := h.PruneNetworks(nil); got != 0 {
		t.Errorf("pruned %d, want 0: the recorded pid is not a gateway any more", got)
	}
	// The stale record still goes, or it would be re-examined on every prune.
	if _, err := os.Stat(gatewaySpecPath(sock)); !os.IsNotExist(err) {
		t.Error("a stale record was left behind")
	}
}

// A host with no isolated sandboxes has nothing to prune, and a missing
// gateway directory is that case on a fresh install rather than an error.
func TestPruneNetworksToleratesAnEmptyHost(t *testing.T) {
	scratchIsolatedDir(t)
	h := &hull{bin: "hull"}
	if got := h.PruneNetworks([]string{"brig-anything"}); got != 0 {
		t.Errorf("pruned %d on a host with no gateways, want 0", got)
	}

	t.Setenv("BRIG_GATEWAY_DIR", filepath.Join(t.TempDir(), "never-created"))
	if got := h.PruneNetworks(nil); got != 0 {
		t.Errorf("pruned %d with no gateway directory, want 0", got)
	}
}

// The shared gateway serves sandboxes this run knows nothing about, so the
// pruner must not touch it. Its socket is named for the network it serves,
// not for a sandbox, which is what keeps it out of the sweep.
func TestPruneNetworksLeavesTheSharedGatewayAlone(t *testing.T) {
	scratchIsolatedDir(t)

	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	sharedPID := fakeGateway(t, shared)
	writeGatewayRecord(shared, sharedPID, "shared")

	h := &hull{bin: "hull"}
	if got := h.PruneNetworks(nil); got != 0 {
		t.Errorf("pruned %d, want 0: the shared gateway is not a sandbox's", got)
	}
	if !ownsGateway(sharedPID, shared) {
		t.Error("the shared gateway was stopped by the per-sandbox pruner")
	}
}

func mustSocket(t *testing.T, name string) string {
	t.Helper()
	sock, err := isolatedSocket(name)
	if err != nil {
		t.Fatal(err)
	}
	return sock
}

func waitGone(t *testing.T, pid int, sock string) {
	t.Helper()
	for range 100 {
		if !ownsGateway(pid, sock) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("pid %d was never stopped", pid)
}

// Each gateway logs to its own file. One file for all of them would interleave
// a gateway per running sandbox, which costs exactly what a log is for -- and
// it is what the "did not come up" error points at, so a shared name would
// send a reader to another sandbox's output.
func TestEachGatewayHasItsOwnLog(t *testing.T) {
	scratchIsolatedDir(t)

	one := mustSocket(t, "brig-one")
	two := mustSocket(t, "brig-two")
	shared, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	logs := map[string]bool{}
	for _, sock := range []string{one, two, shared} {
		path := gatewayLogPath(sock)
		if !strings.HasSuffix(path, ".log") {
			t.Errorf("log path %q is not a log file", path)
		}
		if logs[path] {
			t.Errorf("two gateways share the log %s", path)
		}
		logs[path] = true
	}
}

func TestGatewayPIDIgnoresRubbish(t *testing.T) {
	scratchIsolatedDir(t)
	sock := mustSocket(t, "brig-x")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct{ name, content string }{
		{"not a number", "wat\n"},
		{"empty", ""},
		// pid 1 is launchd, and 0 and -1 are whole process groups. A record
		// holding one of those is a corrupt record, not an instruction.
		{"pid 1", "1\n"},
		{"pid 0", "0\n"},
		{"negative", "-1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(gatewayPIDPath(sock), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if pid, ok := gatewayPID(sock); ok {
				t.Errorf("read %q as pid %d", tt.content, pid)
			}
		})
	}

	// And a good one still reads.
	if err := os.WriteFile(gatewayPIDPath(sock), []byte(strconv.Itoa(4242)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := gatewayPID(sock); !ok || pid != 4242 {
		t.Errorf("gatewayPID = %d (ok=%t), want 4242", pid, ok)
	}
}
