package runtime

import (
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

var testDivert = Divert{Routes: []DivertRoute{{Host: "api.anthropic.com", Port: 443}, {Host: "example.com", Port: 8443}},
	Socket: "/run/user/1000/brig/broker/brig-s.sock"}

func TestEgressSpecRecordsTheDivert(t *testing.T) {
	policy := Egress{Default: "deny", Allow: []Rule{{Host: "example.com"}}}
	before := egressSpec(policy)
	if before != "default=deny\nallow host=example.com" {
		t.Errorf("a policy with no divert records %q", before)
	}
	with := policy
	with.Divert = testDivert
	want := before + "\ndivert api.anthropic.com:443\ndivert example.com:8443\ndivert-socket " + testDivert.Socket
	if got := egressSpec(with); got != want {
		t.Errorf("egressSpec =\n%s\nwant\n%s", got, want)
	}
	swapped := with
	swapped.Divert.Routes = []DivertRoute{testDivert.Routes[1], testDivert.Routes[0]}
	if egressSpec(swapped) != want {
		t.Error("the order of the routes changes the record")
	}
	if got := egressSpec(Egress{Divert: testDivert}); !strings.HasPrefix(got, "divert api.anthropic.com:443\n") {
		t.Errorf("a divert with no policy records %q", got)
	}
}

func TestDivertArgs(t *testing.T) {
	got := testDivert.args(netip.MustParseAddr("10.4.9.1"))
	want := []string{"--divert", "api.anthropic.com:443", "--divert", "example.com:8443", "--divert-socket", testDivert.Socket}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if (Divert{}).args(netip.MustParseAddr("10.4.9.1")) != nil {
		t.Error("no divert gives flags")
	}
}

// A divert added or dropped since the boot needs a boot.
func TestNerdctlNetworkStaleSeesTheDivert(t *testing.T) {
	scratchIsolatedDir(t)
	n := &nerdctl{bin: "nerdctl"}
	policy := Egress{Default: "deny"}
	dir, _ := egressDir()
	_ = os.MkdirAll(dir, 0o700)
	spec, _ := egressFile("brig-s", ".spec")
	_ = os.WriteFile(spec, []byte(egressSpec(policy)+"\n"), 0o600)
	with := policy
	with.Divert = testDivert
	if !n.NetworkStale("brig-s", "", "isolated", with) {
		t.Error("a divert added since the boot does not read stale")
	}
	if !n.NetworkStale("brig-s", "", "isolated", Egress{Divert: testDivert}) {
		t.Error("a divert in place of a policy does not read stale")
	}
}

func TestARunWithADivertStartsTheResolver(t *testing.T) {
	d := newNerdctlDouble(t)
	defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
	var started Egress
	egressStart = func(_ *nerdctl, _ string, e Egress) (egressBoot, error) {
		started = e
		return egressBoot{resolver: netip.MustParseAddr("10.4.9.1"), up: func() error { return nil }}, nil
	}
	err := (&nerdctl{bin: d.bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: "isolated",
		Egress: Egress{Divert: testDivert}})
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Divert.Routes) != 2 || started.Divert.Socket != testDivert.Socket || started.Filtered() {
		t.Errorf("the resolver was started with %+v", started)
	}
	if !strings.Contains(d.calls(), "run --dns 10.4.9.1 --detach --name brig-s") {
		t.Errorf("the guest was not given the resolver: %q", d.calls())
	}
}

// A divert the run path cannot set up refuses the run before the guest boots.
// Booted without it, a connection to a diverted destination would be routed
// directly.
func TestARunWhoseDivertCannotBeSetUpIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		net    string
		divert Divert
		bin    string
		want   string
	}{
		"shared network":    {"shared", testDivert, "nerdctl", "a network of its own"},
		"relative socket":   {"isolated", Divert{Routes: testDivert.Routes, Socket: "brig-s.sock"}, "nerdctl", "not an absolute path"},
		"IPv6 route":        {"isolated", Divert{Routes: []DivertRoute{{Host: "2001:db8::1", Port: 443}}, Socket: "/s"}, "nerdctl", "no IPv6"},
		"DNS route":         {"isolated", Divert{Routes: []DivertRoute{{Host: "a.test", Port: 53}}, Socket: "/s"}, "nerdctl", "port 53"},
		"docker in nerdctl": {"isolated", testDivert, "docker", "this is docker"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newNerdctlDouble(t)
			bin := d.bin
			if c.bin == "docker" {
				bin = d.dir + "/docker"
				if err := os.Symlink(d.bin, bin); err != nil {
					t.Fatal(err)
				}
			}
			defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
			egressStart = func(*nerdctl, string, Egress) (egressBoot, error) {
				t.Error("the resolver was started")
				return egressBoot{}, nil
			}
			err := (&nerdctl{bin: bin}).Run(RunSpec{Name: "brig-s", Image: "img", Net: c.net, Egress: Egress{Divert: c.divert}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want a refusal naming %q, got %v", c.want, err)
			}
			if strings.Contains(d.calls(), "run ") {
				t.Errorf("a refused run reached `run`: %q", d.calls())
			}
		})
	}
}

// An offline sandbox has nothing to divert, and a sandbox with neither a
// policy nor a divert starts no resolver.
func TestARunWithNothingToDivertStartsNoResolver(t *testing.T) {
	for _, spec := range []RunSpec{
		{Name: "brig-s", Image: "img", Net: "none", Egress: Egress{Divert: testDivert}},
		{Name: "brig-s", Image: "img", Net: "isolated"},
	} {
		d := newNerdctlDouble(t)
		defer func(prev func(*nerdctl, string, Egress) (egressBoot, error)) { egressStart = prev }(egressStart)
		egressStart = func(*nerdctl, string, Egress) (egressBoot, error) {
			t.Errorf("net %s: the resolver was started", spec.Net)
			return egressBoot{}, nil
		}
		if err := (&nerdctl{bin: d.bin}).Run(spec); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(d.calls(), "--dns") {
			t.Errorf("net %s: the guest was given brig's resolver: %q", spec.Net, d.calls())
		}
	}
}

// brig.internal on nerdctl is a record the resolver answers with the
// bridge's own address, and a divert of that address on port 443 alone,
// beside the other diverted destinations.
func TestDivertArgsRouteBrigInternalToTheBridgeAddress(t *testing.T) {
	gw := netip.MustParseAddr("10.4.9.1")
	d := testDivert
	d.BrigInternal = true
	want := []string{"--divert", "api.anthropic.com:443", "--divert", "example.com:8443", "--divert", "10.4.9.1:443",
		"--host", "brig.internal=10.4.9.1", "--divert-socket", testDivert.Socket}
	if got := d.args(gw); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	alone := Divert{BrigInternal: true, Socket: testDivert.Socket}
	if !alone.Requested() {
		t.Fatal("brig.internal alone is not a divert")
	}
	want = []string{"--divert", "10.4.9.1:443", "--host", "brig.internal=10.4.9.1", "--divert-socket", testDivert.Socket}
	if got := alone.args(gw); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if err := checkDivert(alone); err != nil {
		t.Errorf("checkDivert: %v", err)
	}
	// A boot with brig.internal and one without differ, so a sandbox booted
	// before its policy gained an endpoint route is booted again.
	if egressSpec(Egress{Divert: alone}) == egressSpec(Egress{Divert: Divert{Socket: alone.Socket}}) ||
		egressSpec(Egress{Divert: d}) == egressSpec(Egress{Divert: testDivert}) {
		t.Error("the egress record does not hold brig.internal")
	}
}
