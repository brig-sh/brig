package runtime

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// linuxPublishRun is a run of linuxBootSpec on hull on Linux that publishes
// host port 18080 to guest port 80, on the shared gateway that
// sharedGatewayAt fakes, where the sandbox has the address 198.18.0.5. runExit
// is what `hull run` exits with.
func linuxPublishRun(t *testing.T, runExit string) (*hull, *hullDouble, *forwardAPI, RunSpec) {
	t.Helper()
	// The double first: it points the gateway directory at a home of its own,
	// and sharedGatewayAt then points it at the gateway it fakes.
	h, d := newRoutingHullDouble(t, true, runExit)
	spec := linuxBootSpec()
	g := sharedGatewayAt(t, spec.Name, 5)
	want, err := ParsePublications([]string{"18080:80"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Publish = want
	return h, d, g, spec
}

// booted reports whether the double was asked to boot: a `hull run` line in
// its log.
func (d *hullDouble) booted() bool {
	for _, line := range strings.Split(d.calls(), "\n") {
		if strings.HasPrefix(line, "run ") {
			return true
		}
	}
	return false
}

// hull on Linux publishes a port on the sandbox's gateway as hull on hvi
// does: the forward is on the gateway when the boot is over, carried to the
// address the run line hands the guest.
func TestLinuxHullPublishesAPortOnTheGateway(t *testing.T) {
	h, d, g, spec := linuxPublishRun(t, "0")
	if err := h.Run(spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !d.booted() {
		t.Fatalf("nothing booted: %q", d.calls())
	}
	live := g.locals()
	if len(live) != 1 || live[0] != "tcp 127.0.0.1:18080=198.18.0.5:80" {
		t.Fatalf("the gateway holds %v, want 18080 carried to the guest's port 80", live)
	}
	if !strings.Contains(d.calls(), "--gateway-cidr 198.18.0.5/") {
		t.Errorf("the guest was not handed the address its forward carries to: %q", d.calls())
	}
}

// A boot on hull on Linux that fails after its forwards were installed takes
// them off the shared gateway again. No sandbox was created, so stop and rm
// have nothing to withdraw them from, and the host port would stay held until
// the gateway restarts.
//
// Negative: the forward another sandbox on the same gateway holds stays. The
// withdrawal goes by this sandbox's address, and the gateway deletes a
// forward by protocol and host address alone, so a withdrawal that went by
// the record or by the port would close the other sandbox's.
func TestALinuxHullBootThatFailsReleasesItsHostPorts(t *testing.T) {
	h, d, g, spec := linuxPublishRun(t, "1")
	theirs := gatewayForward{Protocol: "tcp", Local: "127.0.0.1:19090", Remote: "198.18.0.9:80"}
	g.forwards = append(g.forwards, theirs)
	if err := h.Run(spec); err == nil {
		t.Fatal("a boot hull failed was reported as booted")
	}
	if !d.booted() {
		t.Fatalf("the boot was never asked for, so its failure was not what was tested: %q", d.calls())
	}
	live := g.locals()
	if len(live) != 1 || live[0] != "tcp 127.0.0.1:19090=198.18.0.9:80" {
		t.Fatalf("after the failed boot the gateway holds %v, want only the other sandbox's forward", live)
	}
}

// Negative: a run on hull on Linux that publishes through a shared gateway
// with no API socket is refused before the boot, and told what works on
// Linux. Then the remedy it names is followed: `brig rm --all` stops that
// gateway once hull lists no sandbox on it, so the next boot starts one.
func TestALinuxHullRunBehindAGatewayThatCannotPublishIsToldWhatWorks(t *testing.T) {
	h, d := newRoutingHullDouble(t, true, "0")
	sock := scratchSharedGateway(t)
	old := runHelperGateway(t, sock, true, "BRIG_TEST_GATEWAY_DEAF=1")
	spec := linuxBootSpec()
	spec.Publish, _ = ParsePublications([]string{"18080:80"})

	err := h.Run(spec)
	if err == nil {
		t.Fatal("a run was allowed past a gateway that cannot publish")
	}
	if d.booted() {
		t.Errorf("hull was asked to boot behind a gateway that cannot publish: %q", d.calls())
	}
	msg := err.Error()
	for _, want := range []string{"has no socket a port is published through", "`brig rm --all`",
		"once hull lists no sandbox on it", "`brig network unpublish`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q: %s", want, msg)
		}
	}
	// The macOS advice, none of which works on hull on Linux.
	for _, not := range []string{"older brig", "Upgrade the runtime", "replaces such a gateway",
		"Stop every sandbox"} {
		if strings.Contains(msg, not) {
			t.Errorf("the refusal still says %q: %s", not, msg)
		}
	}
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("the refusal lost the gateway's own error: %v", err)
	}

	// The remedy. `brig rm --all` removes the sandboxes, then prunes.
	if !noInstancesHull(t).PruneSharedNetwork() || !gatewayGone(old, sock) {
		t.Fatal("brig rm --all left the gateway the refusal sent the user to it for")
	}
}
