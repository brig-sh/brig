package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/routemode"
)

// relayRows registers a route mode served on hull on hvi alone, with a note
// on its vz row, and returns a function that removes it again.
func relayRows() (unregister func()) {
	mode := routemode.Register(routemode.Mode{Name: "relay"})
	rows := RegisterModeRecords(
		ModeRecord{Mode: "relay", Path: RunPath{"hull", "hvi"}, State: Enforced, Why: "the gateway relays it",
			Transport: "divert"},
		ModeRecord{Mode: "relay", Path: RunPath{"hull", "vz"}, State: CannotEnforce, Why: "vz has no gateway",
			Note: "relay needs one", Remedy: otherModeRemedy},
	)
	return func() { rows(); mode() }
}

// A mode this build does not serve is refused on every run path, with the
// reason that says so. A registered mode adds its rows to the table, and is
// answered by them. Its rows go when it is removed.
func TestARegisteredModeAddsItsRows(t *testing.T) {
	vz, hvi := RunPath{"hull", "vz"}, RunPath{"hull", "hvi"}
	for _, p := range []RunPath{vz, hvi} {
		err := RequireRouteMode("relay", p)
		var ce *CapabilityError
		if !errors.As(err, &ce) || ce.State != CannotEnforce || ce.Property != "route mode relay" ||
			!strings.Contains(err.Error(), "route mode relay is not part of this build of brig") ||
			!strings.Contains(err.Error(), "Use mode: endpoint") {
			t.Errorf("%s before relay is registered: %v", p, err)
		}
	}
	if len(ModeRecords("relay")) != 0 {
		t.Error("an unregistered mode has rows")
	}

	before := len(modeTable())
	func() {
		defer relayRows()()
		if r := ModeAnswer("relay", hvi); r.State != Enforced || !strings.HasSuffix(r.Line(), "; transport: divert") {
			t.Errorf("relay on hvi: %+v", r)
		}
		if got := ServedModes(hvi); len(got) < 2 || got[0] != v1alpha1.ModeEndpoint || !slices.Contains(got, "relay") {
			t.Errorf("hvi serves %v", got)
		}
		if got := ModeAnswer("relay", vz).Line(); got != "cannot enforce: vz has no gateway; relay needs one" {
			t.Errorf("the vz row is %q", got)
		}
		if err := RequireRouteMode("relay", vz); err == nil || !strings.Contains(err.Error(), "vz has no gateway; relay needs one") {
			t.Errorf("relay on vz: %v", err)
		}
		if r := ModeAnswer("relay", RunPath{"docker", anyBackend}); r.State != Unknown || r.Remedy != otherModeRemedy {
			t.Errorf("relay on a path with no row: %+v", r)
		}
	}()
	if len(modeTable()) != before || routemode.Served("relay") {
		t.Errorf("the table holds %d rows after relay was removed, want %d", len(modeTable()), before)
	}
}

// fakeSetup is the sandbox setup of a route mode registered for a test.
type fakeSetup struct {
	refuse error
	asked  []RunPath
}

func (f *fakeSetup) Check(path RunPath, _ RunSpec) error {
	f.asked = append(f.asked, path)
	return f.refuse
}

func (f *fakeSetup) Args(path RunPath) ([]string, error) {
	if path.Runtime == "hull" {
		return []string{"--relay", "on"}, nil
	}
	return []string{"--annotation", "relay=on"}, nil
}

// A run's setup is asked about the run path before anything boots, and its
// arguments reach the run command line in the runtime's own spelling. A
// refusal of it refuses the run.
func TestASetupIsCheckedAndCarried(t *testing.T) {
	scratchIsolatedDir(t)
	s := &fakeSetup{}
	spec := RunSpec{Name: "s", Image: "img", Net: "isolated", Setup: []Setup{s}}
	if err := supports(spec, "hvi"); err != nil {
		t.Fatalf("hvi: %v", err)
	}
	if got := argv(t, spec, "hvi", "isolated", "/tmp/gw.sock", "198.18.0.2/24"); !strings.Contains(got, " --relay on") {
		t.Errorf("the hull run line does not carry the setup: %s", got)
	}
	if got := argv(t, RunSpec{Name: "s", Image: "img"}, "hvi", "isolated", "", ""); strings.Contains(got, "--relay") {
		t.Errorf("a run with no setup carries it: %s", got)
	}
	args, _, err := (&nerdctl{bin: "nerdctl"}).runArgs(spec)
	if err != nil || !strings.Contains(strings.Join(args, " "), "--annotation relay=on") {
		t.Errorf("the nerdctl run line does not carry the setup: %v, %v", args, err)
	}
	if len(s.asked) != 1 || s.asked[0] != (RunPath{"hull", "hvi"}) {
		t.Errorf("the setup was asked about %v", s.asked)
	}
	s.refuse = errors.New("relay is not here")
	for name, err := range map[string]error{
		"hull":    supports(spec, "hvi"),
		"nerdctl": (&nerdctl{bin: "nerdctl"}).CanRun(spec),
	} {
		if !errors.Is(err, s.refuse) {
			t.Errorf("%s: %v, want the setup's refusal", name, err)
		}
	}
}

// A destination diverted for a route mode is held to that mode's rows: off
// the paths that serve it, and with no network, the run is refused as that
// mode. A refusal at boot names it too.
func TestADivertIsRefusedAsItsRouteMode(t *testing.T) {
	scratchIsolatedDir(t)
	t.Cleanup(relayRows())
	spec := RunSpec{Name: "s", Image: "img", Net: "isolated", Egress: Egress{Divert: Divert{
		Routes: []DivertRoute{{Host: "api.example.com", Port: 443, Mode: "relay"}}, Socket: "/x.sock"}}}
	var ce *CapabilityError
	if err := supports(spec, "vz"); !errors.As(err, &ce) || ce.Property != "route mode relay" ||
		!strings.Contains(err.Error(), "vz has no gateway") {
		t.Errorf("vz: %v", err)
	}
	offline := spec
	offline.Net = "none"
	if err := supports(offline, "hvi"); err == nil || !strings.Contains(err.Error(), "BRIG_NETWORK=offline") {
		t.Errorf("hvi offline: %v", err)
	}
	if err := supports(spec, "hvi"); err != nil {
		t.Errorf("hvi: %v", err)
	}
	err := egressRefusal(spec.Egress, "nft failed", "Install nftables", nil)
	if !errors.As(err, &ce) || ce.Property != "route mode relay" || ce.Remedy != "Install nftables, or use mode: endpoint" {
		t.Errorf("a divert of relay alone: %+v", ce)
	}
}
