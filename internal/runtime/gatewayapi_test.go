package runtime

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// forwardAPI is a gateway's API socket with the /forwards endpoint on it, so
// the client and the reconciler can be driven without a runtime. It is only
// the endpoint: the gateway a pruning test wants is the process, and that one
// is fakeGateway in prune_test.go.
type forwardAPI struct {
	mu       sync.Mutex
	forwards []gatewayForward
	// missing serves a gateway too old to have the endpoint: every path
	// answers 404.
	missing bool
	// sock is the control socket the client is given; the API socket is
	// derived from it the way startGateway derives the one it passes.
	sock string
	// outside are local addresses a process outside the gateway holds. A
	// POST for one is refused the way hull refuses a bind that failed.
	outside map[string]bool
}

func newFakeGateway(t *testing.T, missing bool) *forwardAPI {
	t.Helper()
	// A unix socket path is capped at 104 bytes, which t.TempDir's test-named
	// directories exceed.
	dir, err := os.MkdirTemp("/tmp", "brig-gw-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	g := &forwardAPI{sock: filepath.Join(dir, "gw.sock"), missing: missing}
	l, err := net.Listen("unix", gatewayAPISocket(g.sock))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return g
}

func (g *forwardAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.missing {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(g.forwards)
	case http.MethodPost:
		var f gatewayForward
		_ = json.NewDecoder(r.Body).Decode(&f)
		if g.outside[f.Local] {
			http.Error(w, "a forward already listens there: "+f.Local+" is held outside this "+
				"gateway: listen tcp "+f.Local+": bind: address already in use", http.StatusConflict)
			return
		}
		for _, have := range g.forwards {
			if have.Local == f.Local && have.Protocol == f.Protocol {
				http.Error(w, "a forward already listens there: "+have.Protocol+"/"+
					have.Local+" carries "+have.Remote, http.StatusConflict)
				return
			}
		}
		g.forwards = append(g.forwards, f)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		local, proto := r.URL.Query().Get("local"), r.URL.Query().Get("protocol")
		for i, have := range g.forwards {
			if have.Local == local && have.Protocol == proto {
				g.forwards = append(g.forwards[:i], g.forwards[i+1:]...)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		http.Error(w, "no forward listens there", http.StatusNotFound)
	}
}

func (g *forwardAPI) locals() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, f := range g.forwards {
		out = append(out, f.Protocol+" "+f.Local+"="+f.Remote)
	}
	return out
}

func TestReconcileMakesTheGatewayServeExactlyWhatIsRecorded(t *testing.T) {
	g := newFakeGateway(t, false)
	const guest = "198.18.0.5"

	want, err := ParsePublications([]string{"8080:80", "3000"})
	if err != nil {
		t.Fatalf("ParsePublications: %v", err)
	}
	if err := reconcilePublications(g.sock, guest, want); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := g.locals(); len(got) != 2 {
		t.Fatalf("after the first reconcile: %v", got)
	}

	// Running it again installs nothing further: this is what every boot of an
	// unchanged sandbox does.
	if err := reconcilePublications(g.sock, guest, want); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := g.locals(); len(got) != 2 {
		t.Fatalf("a repeat reconcile changed the set: %v", got)
	}

	// A port dropped from the record is withdrawn.
	if err := reconcilePublications(g.sock, guest, want[:1]); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	got := g.locals()
	if len(got) != 1 || !strings.Contains(got[0], ":80") {
		t.Fatalf("after dropping one: %v", got)
	}
}

// A shared gateway carries other sandboxes' forwards, and reconciling one
// sandbox must not touch them.
func TestReconcileLeavesOtherGuestsAlone(t *testing.T) {
	g := newFakeGateway(t, false)
	other := gatewayForward{Protocol: "tcp", Local: "127.0.0.1:9999", Remote: "198.18.0.9:9999"}
	g.forwards = append(g.forwards, other)

	want, _ := ParsePublications([]string{"8080:80"})
	if err := reconcilePublications(g.sock, "198.18.0.5", want); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// And again with nothing wanted, which is the case that would sweep.
	if err := reconcilePublications(g.sock, "198.18.0.5", nil); err != nil {
		t.Fatalf("reconcile to empty: %v", err)
	}
	got := g.locals()
	if len(got) != 1 || !strings.Contains(got[0], "198.18.0.9:9999") {
		t.Fatalf("the other guest's forward: %v", got)
	}
}

func TestPublishedOnReadsBackOnlyThisGuestsPorts(t *testing.T) {
	g := newFakeGateway(t, false)
	g.forwards = []gatewayForward{
		{Protocol: "tcp", Local: "127.0.0.1:8080", Remote: "198.18.0.5:80"},
		{Protocol: "udp", Local: "127.0.0.1:5353", Remote: "198.18.0.5:53"},
		{Protocol: "tcp", Local: "127.0.0.1:9999", Remote: "198.18.0.9:9999"},
	}
	got, err := publishedOn(g.sock, "198.18.0.5")
	if err != nil {
		t.Fatalf("publishedOn: %v", err)
	}
	want := []Publication{
		{HostPort: 8080, GuestPort: 80},
		{Protocol: "udp", HostPort: 5353, GuestPort: 53},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publishedOn = %+v, want %+v", got, want)
	}
}

// A host address something else already publishes is a conflict, not a
// silently replaced forward.
func TestPublishOnReportsAConflict(t *testing.T) {
	g := newFakeGateway(t, false)
	g.forwards = []gatewayForward{
		{Protocol: "tcp", Local: "127.0.0.1:8080", Remote: "198.18.0.9:80"},
	}
	p, _ := ParsePublication("8080:3000")
	err := publishOn(g.sock, "198.18.0.5", p)
	var conflict *forwardConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("publishOn: %v is not a conflict", err)
	}
	// The guest address in the way is recoverable from the message, which is
	// what lets the error name the sandbox rather than only the port.
	if got := conflictGuest(conflict.detail); got != "198.18.0.9" {
		t.Fatalf("conflictGuest = %q", got)
	}
}

// Withdrawing a port nothing publishes is the state being asked for.
func TestWithdrawIsIdempotent(t *testing.T) {
	g := newFakeGateway(t, false)
	p, _ := ParsePublication("8080:80")
	if err := withdrawFrom(g.sock, "198.18.0.5", p); err != nil {
		t.Fatalf("withdrawing what was never published: %v", err)
	}
}

// A gateway without the endpoint is named as such rather than reported as a
// missing forward -- and a run that publishes nothing does not mention it at
// all, because there is nothing to correct.
func TestAGatewayWithoutTheEndpointIsNamed(t *testing.T) {
	g := newFakeGateway(t, true)
	if err := reconcilePublications(g.sock, "198.18.0.5", nil); err != nil {
		t.Fatalf("reconcile with nothing to publish: %v", err)
	}
	want, _ := ParsePublications([]string{"8080:80"})
	err := reconcilePublications(g.sock, "198.18.0.5", want)
	if !errors.Is(err, ErrNoForwardAPI) {
		t.Fatalf("reconcile against an old gateway: %v", err)
	}
}

func TestGatewayAPISocketSitsBesideTheControlSocket(t *testing.T) {
	if got := gatewayAPISocket("/tmp/brig/gateway-198-18-0-0_24.sock"); got != "/tmp/brig/gateway-198-18-0-0_24.api" {
		t.Fatalf("api socket: %q", got)
	}
	// Shorter than the QEMU socket, which is what the sockaddr length check in
	// isolatedSocket is made against.
	sock := "/tmp/brig/sandbox-web.sock"
	if len(gatewayAPISocket(sock)) > len(qemuGatewaySocket(sock)) {
		t.Fatal("the api socket is longer than the qemu socket, so the length check misses it")
	}
}

// A gateway with no API socket at all is the case a 404 does not cover: a
// stub runtime, a gateway brig did not start, anything predating --api. A
// boot that publishes nothing must not care, and one that publishes something
// must say how to get a gateway with the socket rather than quote a dial
// error. Upgrading the runtime would not help: every hull takes --api.
func TestAGatewayWithNoAPISocketFailsOnlyARunThatPublishes(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "brig-noapi-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// The control socket is named but nothing listens on its API sibling.
	sock := filepath.Join(dir, "gw.sock")

	if err := reconcilePublications(sock, "198.18.0.5", nil); err != nil {
		t.Fatalf("a boot that publishes nothing was refused: %v", err)
	}

	want, _ := ParsePublications([]string{"8080:80"})
	err = reconcilePublications(sock, "198.18.0.5", want)
	if err == nil {
		t.Fatal("a boot that publishes a port was allowed past a gateway that cannot")
	}
	if !strings.Contains(err.Error(), "started by an older brig") ||
		strings.Contains(err.Error(), "Upgrade the runtime") {
		t.Fatalf("the refusal does not say what to do: %v", err)
	}
}

// sharedGatewayAt makes the forward endpoint reachable as the SHARED gateway
// of a BRIG_GATEWAY_DIR, with one sandbox holding an address on it.
//
// Three files make that true, and all three are what the code under test
// reads: the control socket under the name gatewaySocket derives from the
// subnet, the QEMU socket gatewayReachable dials, and the address map
// sharedIPs keeps.
func sharedGatewayAt(t *testing.T, sandbox string, host int) *forwardAPI {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "brig-shared-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("BRIG_GATEWAY_DIR", dir)

	sock, err := gatewaySocket()
	if err != nil {
		t.Fatalf("gatewaySocket: %v", err)
	}
	g := &forwardAPI{sock: sock}
	api, err := net.Listen("unix", gatewayAPISocket(sock))
	if err != nil {
		t.Fatalf("listen on the api socket: %v", err)
	}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(api) }()
	t.Cleanup(func() { _ = srv.Close() })

	// Only has to accept, which is the whole of gatewayReachable's question.
	qemu, err := net.Listen("unix", qemuGatewaySocket(sock))
	if err != nil {
		t.Fatalf("listen on the qemu socket: %v", err)
	}
	t.Cleanup(func() { _ = qemu.Close() })

	alloc, err := sharedIPs()
	if err != nil {
		t.Fatalf("sharedIPs: %v", err)
	}
	if err := alloc.write(map[string]int{sandbox: host}); err != nil {
		t.Fatalf("write the address map: %v", err)
	}
	return g
}

// A stopped sandbox must not go on holding a host port. Its forwards are on
// the shared gateway, which serves every other sandbox and is not stopped with
// it, so nothing else on the machine could take that port.
func TestStoppingASandboxReleasesItsHostPorts(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)

	want, _ := ParsePublications([]string{"18080:80"})
	if _, err := RecordPublications(sandbox, want); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	if err := reconcilePublications(g.sock, "198.18.0.5", want); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := g.locals(); len(got) != 1 {
		t.Fatalf("after the boot: %v", got)
	}

	withdrawPublications(sandbox)
	if got := g.locals(); len(got) != 0 {
		t.Fatalf("a stopped sandbox still holds %v", got)
	}
	// The record is left, so the next run publishes the same port again.
	if got := published(t, sandbox); len(got) != 1 || got[0].HostPort != 18080 {
		t.Fatalf("stopping forgot what the sandbox publishes: %+v", got)
	}
	if err := reconcilePublications(g.sock, "198.18.0.5", published(t, sandbox)); err != nil {
		t.Fatalf("reconcile on the next boot: %v", err)
	}
	if got := g.locals(); len(got) != 1 {
		t.Fatalf("the next boot did not republish: %v", got)
	}
}

// And a sandbox with nothing published asks the gateway nothing.
func TestStoppingASandboxThatPublishesNothingIsQuiet(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	other := gatewayForward{Protocol: "tcp", Local: "127.0.0.1:9999", Remote: "198.18.0.9:9999"}
	g.forwards = append(g.forwards, other)

	withdrawPublications(sandbox)
	if got := g.locals(); len(got) != 1 {
		t.Fatalf("stopping took another sandbox's forward: %v", got)
	}
}

// A run that fails between the reconcile and the boot, here on boot assets it
// cannot find, must not leave its forwards on the shared gateway. No sandbox
// was created, so stop and rm have nothing to withdraw them from.
func TestARunThatFailsBeforeTheBootReleasesItsHostPorts(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())

	want, _ := ParsePublications([]string{"18080:80"})
	h := &hull{bin: "hull"}
	err := h.Run(RunSpec{Name: sandbox, Image: "ubuntu:latest", Hypervisor: "hvi",
		GenericBoot: true, Publish: want})
	if err == nil || !strings.Contains(err.Error(), bootKernelName()) {
		t.Fatalf("the run did not fail on the missing boot assets: %v", err)
	}
	if got := g.locals(); len(got) != 0 {
		t.Fatalf("a run that never booted left %v on the gateway", got)
	}
}

// Moving a host port on a sandbox that is already up must move the one
// listener, not ask the gateway for a second on an address it already has.
//
// The boot reconcile withdraws before it publishes; the live path has to do
// the same, or a move comes back as a conflict with the sandbox's own forward
// while the record has already been rewritten to the port that never landed.
func TestPublishMovesAHostPortOnALiveSandbox(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	h := &hull{bin: "hull"}

	first, _ := ParsePublication("18080:80")
	if err := h.Publish(sandbox, first); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	moved, _ := ParsePublication("18080:90")
	if err := h.Publish(sandbox, moved); err != nil {
		t.Fatalf("moving a live host port: %v", err)
	}
	live := g.locals()
	if len(live) != 1 {
		t.Fatalf("the gateway holds %v, want one listener on 18080", live)
	}
	if !strings.Contains(live[0], ":90") {
		t.Fatalf("the listener still carries the old guest port: %v", live)
	}
	// And the record agrees with the gateway rather than running ahead of it.
	rec := published(t, sandbox)
	if len(rec) != 1 || rec[0].GuestPort != 90 {
		t.Fatalf("the record says %+v", rec)
	}
}

// Moving a live port from loopback to 0.0.0.0 is a move as well. The two
// listeners have different addresses, but 0.0.0.0 covers loopback.
func TestPublishMovesALivePortOntoEveryAddress(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	h := &hull{bin: "hull"}

	loop, _ := ParsePublication("18080:80")
	if err := h.Publish(sandbox, loop); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	wide, _ := ParsePublication("0.0.0.0:18080:80")
	if err := h.Publish(sandbox, wide); err != nil {
		t.Fatalf("moving the port onto 0.0.0.0: %v", err)
	}
	live := g.locals()
	if len(live) != 1 || !strings.Contains(live[0], "0.0.0.0:18080") {
		t.Fatalf("the gateway holds %v, want one listener on 0.0.0.0:18080", live)
	}
	if rec := published(t, sandbox); len(rec) != 1 || rec[0].Addr() != "0.0.0.0" {
		t.Fatalf("the record says %+v", rec)
	}
}

// A host port another sandbox holds is a conflict, not something to withdraw.
// The gateway keys a forward by protocol and local address alone, so the move
// above must never reach a forward that belongs to somebody else.
func TestPublishWillNotTakeAnotherSandboxsPort(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	theirs := gatewayForward{Protocol: "tcp", Local: "127.0.0.1:18080", Remote: "198.18.0.9:80"}
	g.forwards = append(g.forwards, theirs)

	h := &hull{bin: "hull"}
	p, _ := ParsePublication("18080:90")
	if err := h.Publish(sandbox, p); err == nil {
		t.Fatal("publishing over another sandbox's host port was allowed")
	}
	if live := g.locals(); len(live) != 1 || !strings.Contains(live[0], "198.18.0.9:80") {
		t.Fatalf("the other sandbox's forward was disturbed: %v", live)
	}
	if rec := published(t, sandbox); len(rec) != 0 {
		t.Fatalf("a refused publish was recorded: %+v", rec)
	}
}

// Stopping or removing a sandbox must not take a host port another sandbox
// has since taken.
//
// The record keeps a publication across a stop, and the gateway deletes a
// forward by protocol and local address alone. So withdrawing straight from
// the record closes whatever is on that port now, which need not be ours.
func TestStoppingASandboxLeavesAPortAnotherSandboxTook(t *testing.T) {
	const ours = "brig-claude-a"
	g := sharedGatewayAt(t, ours, 5)

	// A published 18080 and was stopped: the record keeps it, the forward went.
	mine, _ := ParsePublications([]string{"18080:3000"})
	if _, err := RecordPublications(ours, mine); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	// B then took the free host port.
	theirs := gatewayForward{Protocol: "tcp", Local: "127.0.0.1:18080", Remote: "198.18.0.9:3000"}
	g.forwards = append(g.forwards, theirs)

	// A is removed, or started again, which clears the stale instance.
	withdrawPublications(ours)

	live := g.locals()
	if len(live) != 1 || !strings.Contains(live[0], "198.18.0.9:3000") {
		t.Fatalf("the other sandbox lost its port: %v", live)
	}
}

// And the same for an explicit withdrawal.
func TestUnpublishLeavesAPortAnotherSandboxTook(t *testing.T) {
	const ours = "brig-claude-a"
	g := sharedGatewayAt(t, ours, 5)
	mine, _ := ParsePublications([]string{"18080:3000"})
	if _, err := RecordPublications(ours, mine); err != nil {
		t.Fatalf("RecordPublications: %v", err)
	}
	g.forwards = append(g.forwards, gatewayForward{
		Protocol: "tcp", Local: "127.0.0.1:18080", Remote: "198.18.0.9:3000",
	})

	h := &hull{bin: "hull"}
	if err := h.Unpublish(ours, mine[0]); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if live := g.locals(); len(live) != 1 || !strings.Contains(live[0], "198.18.0.9:3000") {
		t.Fatalf("the other sandbox lost its port: %v", live)
	}
	// Ours is forgotten either way: it is not published and will not be.
	if rec := published(t, ours); len(rec) != 0 {
		t.Fatalf("the record kept a withdrawn publication: %+v", rec)
	}
}
