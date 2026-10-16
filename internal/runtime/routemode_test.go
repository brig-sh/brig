package runtime

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// The endpoint mode is served over brig.internal on the run paths that give
// the guest a gateway brig owns: hull on hvi and nerdctl on urunc. Every
// other path is refused with its reason and serves no mode: hull on vz and
// qemu take their network from vmnet, docker manages its own bridges,
// nerdctl on runc gives the guest the host's own kernel, and a backend the
// table does not know has no answer.
func TestEndpointIsServedOverBrigInternalAndRefusedElsewhere(t *testing.T) {
	for _, p := range []RunPath{{"hull", "hvi"}, {"nerdctl", uruncShim}} {
		r := ModeAnswer(v1alpha1.ModeEndpoint, p)
		if r.State != Enforced || r.Transport != transportBrigInternal ||
			!strings.Contains(r.Line(), "transport: brig.internal") {
			t.Errorf("%s: %+v, want enforced over brig.internal", p, r)
		}
		if err := RequireRouteMode(v1alpha1.ModeEndpoint, p); err != nil {
			t.Errorf("%s: RequireRouteMode(endpoint) = %v", p, err)
		}
		if got := ServedModes(p); len(got) == 0 || got[0] != v1alpha1.ModeEndpoint {
			t.Errorf("%s serves %v", p, got)
		}
	}
	for p, want := range map[RunPath]string{
		{"hull", "vz"}:                       "vz takes its network from vmnet",
		{"hull", "qemu"}:                     "qemu takes its network from vmnet",
		{"docker", anyBackend}:               "docker manages its own bridges",
		{"nerdctl", "io.containerd.runc.v2"}: "brig boots no shim that gives the guest the host's own kernel",
		{"nerdctl", "unknown1"}:              "brig holds no answer for this run path",
	} {
		err := RequireRouteMode(v1alpha1.ModeEndpoint, p)
		var ce *CapabilityError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: RequireRouteMode(endpoint) = %v, want a refusal naming %q", p, err, want)
		}
		if got := ServedModes(p); len(got) != 0 {
			t.Errorf("%s serves %v", p, got)
		}
		if strings.Contains(ModeAnswer(v1alpha1.ModeEndpoint, p).Line(), "transport") {
			t.Errorf("%s: a refused row names a transport", p)
		}
	}
}

// CheckBrigInternal holds where the table answers enforced only for a run
// with a network, on a runtime that reports the brig.internal address, and
// whose probe passes.
func TestCheckBrigInternal(t *testing.T) {
	hvi := RunPath{"hull", "hvi"}
	if err := CheckBrigInternal(kindOnly{kind: "hull"}, RunPath{"hull", "vz"}, "isolated"); err == nil {
		t.Error("vz was checked past the table")
	}
	err := CheckBrigInternal(kindOnly{kind: "hull"}, hvi, "none")
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.State != CannotEnforce || !strings.Contains(err.Error(), "BRIG_NETWORK=offline") {
		t.Errorf("an offline run: %v", err)
	}
	if err := CheckBrigInternal(kindOnly{kind: "hull"}, hvi, "isolated"); !errors.As(err, &ce) || ce.State != Unknown {
		t.Errorf("a runtime that reports no address: %v", err)
	}
	probe := errors.New("no --divert")
	if err := CheckBrigInternal(routerOnly{kindOnly{kind: "hull"}, probe}, hvi, "isolated"); !errors.Is(err, probe) {
		t.Errorf("the probe's answer was not the check's: %v", err)
	}
	if err := CheckBrigInternal(routerOnly{kindOnly{kind: "hull"}, nil}, hvi, "isolated"); err != nil {
		t.Errorf("a runtime that routes it: %v", err)
	}
	// hull takes a gateway of the sandbox's own on any posture; nerdctl
	// needs the isolated one asked for.
	if err := CheckBrigInternal(routerOnly{kindOnly{kind: "hull"}, nil}, hvi, "shared"); err != nil {
		t.Errorf("hull on the shared posture: %v", err)
	}
	urunc := RunPath{"nerdctl", uruncShim}
	if err := CheckBrigInternal(routerOnly{kindOnly{kind: "nerdctl"}, nil}, urunc, "shared"); err == nil ||
		!strings.Contains(err.Error(), "--network isolated") {
		t.Errorf("nerdctl on the shared posture: %v", err)
	}
	if err := CheckBrigInternal(routerOnly{kindOnly{kind: "nerdctl"}, nil}, urunc, "isolated"); err != nil {
		t.Errorf("nerdctl isolated: %v", err)
	}
}

// routerOnly is a runtime that reports a brig.internal address, and probes
// with err.
type routerOnly struct {
	kindOnly
	err error
}

func (routerOnly) BrigInternalAddr(string) (netip.Addr, error) {
	return netip.MustParseAddr("198.18.1.1"), nil
}

func (r routerOnly) ProbeBrigInternal() error { return r.err }

func TestRoutePathNamesTheBackend(t *testing.T) {
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
	for _, c := range []struct {
		rt   Runtime
		hv   string
		want RunPath
	}{
		{kindOnly{kind: "hull"}, "", RunPath{"hull", "vz"}},
		{kindOnly{kind: "hull"}, "hvi", RunPath{"hull", "hvi"}},
		{kindOnly{kind: "nerdctl"}, "", RunPath{"nerdctl", uruncShim}},
	} {
		if got := RoutePath(c.rt, c.hv); got != c.want {
			t.Errorf("RoutePath(%s, %q) = %v, want %v", c.rt.Kind(), c.hv, got, c.want)
		}
	}
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "io.containerd.runc.v2")
	if got := RoutePath(kindOnly{kind: "nerdctl"}, ""); got != (RunPath{"nerdctl", "io.containerd.runc.v2"}) {
		t.Errorf("RoutePath on runc = %v", got)
	}
}

// kindOnly is a runtime that answers its kind. Any other call reaches the nil
// Runtime inside it and panics.
type kindOnly struct {
	Runtime
	kind string
}

func (k kindOnly) Kind() string { return k.kind }
