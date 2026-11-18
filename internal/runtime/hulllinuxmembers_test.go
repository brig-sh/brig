package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// instancesHull is a hull on Linux whose `ps -a` lists instances, one "name
// state" per entry, and whose `hull inspect <name>` answers with records[name]
// or, for a name with no record, fails the way hull does for an instance it
// cannot find. An empty instances prints what hull prints for an empty store,
// and a nil one makes ps fail.
func instancesHull(t *testing.T, instances []string, records map[string]string) *hull {
	t.Helper()
	dir := t.TempDir()
	ps := "exit 1"
	if instances != nil {
		listing := "ID STATUS\n" + strings.Join(instances, "\n") + "\n"
		if len(instances) == 0 {
			listing = "No instances found\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(listing), 0o600); err != nil {
			t.Fatal(err)
		}
		ps = "cat '" + filepath.Join(dir, "ps") + "'"
	}
	for name, record := range records {
		if err := os.WriteFile(filepath.Join(dir, "inspect-"+name), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "hull")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"ps) " + ps + " ;;\n" +
		"inspect) f='" + dir + "/inspect-'\"$2\"; [ -f \"$f\" ] || { echo 'instance not found' >&2; exit 1; }; cat \"$f\" ;;\n" +
		"*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin, linux: true}
}

// noInstancesHull is a hull on Linux with an empty store, so the gateway's
// own answer is all that decides.
func noInstancesHull(t *testing.T) *hull { return instancesHull(t, []string{}, nil) }

// inspectRecord is what `hull inspect` on Linux says of an instance's network.
func inspectRecord(name, network, sock string) string {
	blob, _ := json.Marshal(map[string]any{"id": name, "status": "running", "network": network, "gatewaySocket": sock})
	return string(blob)
}

// gatewayGone waits for the gateway pid on sock to stop, and reports whether
// it did.
func gatewayGone(pid int, sock string) bool {
	for range 100 {
		if !ownsGateway(pid, sock) && !gatewayReachable(sock) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// `brig rm --all` on hull on Linux stops the shared gateway once the gateway
// says no guest is on it and hull lists no instance on it. The process table
// cannot say so there, and the gateway stayed up for good.
func TestLinuxHullStopsTheSharedGatewayNoGuestIsOn(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := runHelperGateway(t, sock, true, "BRIG_TEST_GATEWAY_MEMBERS=[]")
	if !noInstancesHull(t).PruneSharedNetwork() {
		t.Error("PruneSharedNetwork kept a shared gateway that lists no guest")
	}
	if !gatewayGone(pid, sock) {
		t.Error("the shared gateway is still up")
	}
}

// Negative: the shared gateway on hull on Linux stays up while it lists a
// guest, and while it cannot say whether one is on it, though hull lists no
// instance on it. Stopping it would cut the network of every shared sandbox
// still running.
func TestLinuxHullKeepsTheSharedGatewayAGuestMayBeOn(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		api  bool
	}{
		{"a guest is listed", []string{`BRIG_TEST_GATEWAY_MEMBERS=[{"transport":"qemu","joinedAt":"2026-11-16T12:00:00Z"}]`}, true},
		{"no /members", nil, true},
		{"/members fails", []string{"BRIG_TEST_GATEWAY_MEMBERS=[]", "BRIG_TEST_GATEWAY_MEMBERS_STATUS=503"}, true},
		{"no API", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock := scratchSharedGateway(t)
			pid := runHelperGateway(t, sock, c.api, c.env...)
			if noInstancesHull(t).PruneSharedNetwork() {
				t.Error("PruneSharedNetwork reported the shared gateway stopped")
			}
			if !ownsGateway(pid, sock) || !gatewayReachable(sock) {
				t.Error("the shared gateway was stopped under the guests it may still serve")
			}
		})
	}
}

// Negative: the shared gateway on hull on Linux stays up while hull lists an
// instance on it, though the gateway lists no guest. hull run returns before
// hvi has connected, and the gateway lists a guest only once it has, so in
// between the instance is the one sign of it. An instance hull cannot list or
// inspect, or whose network it does not name, keeps the gateway too. A
// stopped one, an offline one and one on another gateway do not.
func TestLinuxHullKeepsTheSharedGatewayASandboxIsOn(t *testing.T) {
	for _, c := range []struct {
		name      string
		instances []string
		records   func(sock string) map[string]string
		keep      bool
	}{
		{"running on it", []string{"brig-a running"}, func(sock string) map[string]string {
			return map[string]string{"brig-a": inspectRecord("brig-a", "gateway", sock)}
		}, true},
		{"ps fails", nil, nil, true},
		{"inspect fails", []string{"brig-a running"}, nil, true},
		{"no network named", []string{"brig-a running"}, func(sock string) map[string]string {
			return map[string]string{"brig-a": inspectRecord("brig-a", "", "")}
		}, true},
		{"another instance answers", []string{"brig-a running"}, func(sock string) map[string]string {
			return map[string]string{"brig-a": inspectRecord("brig-b", "none", "")}
		}, true},
		{"stopped on it", []string{"brig-a stopped"}, func(sock string) map[string]string {
			return map[string]string{"brig-a": inspectRecord("brig-a", "gateway", sock)}
		}, false},
		{"offline and on another gateway", []string{"brig-a running", "brig-b running"}, func(sock string) map[string]string {
			return map[string]string{
				"brig-a": inspectRecord("brig-a", "none", ""),
				"brig-b": inspectRecord("brig-b", "gateway", sock+".other"),
			}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock := scratchSharedGateway(t)
			pid := runHelperGateway(t, sock, true, "BRIG_TEST_GATEWAY_MEMBERS=[]")
			var records map[string]string
			if c.records != nil {
				records = c.records(sock)
			}
			stopped := instancesHull(t, c.instances, records).PruneSharedNetwork()
			if c.keep {
				if stopped || !ownsGateway(pid, sock) {
					t.Error("the shared gateway was stopped under a sandbox that may be on it")
				}
				return
			}
			if !stopped || !gatewayGone(pid, sock) {
				t.Error("the shared gateway was kept for a sandbox that is not on it")
			}
		})
	}
}

// Negative: a boot on its way onto the shared gateway on hull on Linux keeps
// it up, though neither the gateway nor hull lists it yet. Its lease is what
// says so.
func TestLinuxHullKeepsTheSharedGatewayABootIsJoining(t *testing.T) {
	sock := scratchSharedGateway(t)
	pid := runHelperGateway(t, sock, true, "BRIG_TEST_GATEWAY_MEMBERS=[]")
	h := noInstancesHull(t)
	_, release, err := leaseSharedGateway(sock)
	if err != nil {
		t.Fatal(err)
	}
	if h.PruneSharedNetwork() || !ownsGateway(pid, sock) {
		t.Fatal("the shared gateway was stopped under a boot joining it")
	}
	release()
	if !h.PruneSharedNetwork() || !gatewayGone(pid, sock) {
		t.Error("the shared gateway outlived the boot that kept it")
	}
}

// Negative: a gateway that answers null has said nothing about its guests,
// and is not taken for one with none. Nor is a 404, a failure, or an API that
// does not answer.
func TestGatewayMembersReadsOnlyAListAsKnown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "brig-gm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := dir + "/gw.sock"
	if _, known := gatewayMembers(sock); known {
		t.Error("a gateway with no API listed its guests")
	}
	body := "null"
	l, err := net.Listen("unix", gatewayAPISocket(sock))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch body {
		case "404":
			http.NotFound(w, r)
		case "500":
			http.Error(w, "[]", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(body))
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	for _, b := range []string{"null", "404", "500", "{}"} {
		body = b
		if _, known := gatewayMembers(sock); known {
			t.Errorf("an answer of %s was read as a list of guests", b)
		}
	}
	body = "[]"
	if got, known := gatewayMembers(sock); !known || got != 0 {
		t.Errorf("[] = %v, %v, want no guests, known", got, known)
	}
}

// Negative: a shared gateway named by a relative BRIG_GATEWAY_SOCK stays up
// while hull lists an instance on it. hull records the socket made absolute,
// and compared as given the instance matched nothing, so `brig rm --all`
// stopped the gateway under a guest whose hvi had not connected yet.
func TestLinuxHullKeepsTheSharedGatewayItNamesRelatively(t *testing.T) {
	abs := scratchSharedGateway(t)
	t.Chdir(filepath.Dir(abs))
	rel := filepath.Base(abs)
	t.Setenv("BRIG_GATEWAY_SOCK", rel)
	pid := runHelperGateway(t, rel, true, "BRIG_TEST_GATEWAY_MEMBERS=[]")
	h := instancesHull(t, []string{"brig-a running"},
		map[string]string{"brig-a": inspectRecord("brig-a", "gateway", abs)})
	if h.PruneSharedNetwork() || !ownsGateway(pid, rel) {
		t.Error("the shared gateway was stopped under a sandbox hull lists on it")
	}
	// The same gateway with no instance on it goes, so what kept it above
	// was the instance.
	if !noInstancesHull(t).PruneSharedNetwork() || !gatewayGone(pid, rel) {
		t.Error("the shared gateway named relatively was kept with nothing on it")
	}
}

// Negative: hull's line for an empty store is not an instance. Read as one,
// it was an instance named "No" that hull could not inspect, so `brig rm
// --all` kept the shared gateway on a host with nothing on it.
func TestHullListReadsAnEmptyStoreAsNoInstance(t *testing.T) {
	list, err := noInstancesHull(t).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("an empty store listed %v", list)
	}
	list, err = instancesHull(t, []string{"brig-a running"}, nil).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != (Instance{Name: "brig-a", State: "running"}) {
		t.Errorf("one instance listed as %v", list)
	}
}
