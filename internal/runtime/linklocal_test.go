package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// The answers of the two hull gateways to the probe, as each prints them: one
// that refuses a link-local divert while it parses its flags, and one that
// takes it and stops on the second, identical divert.
const (
	oldHullAnswer = `error: invalid --divert "169.254.170.2:80=/x/probe.sock": the gateway resets link-local ` +
		`169.254.0.0/16 before it checks diverts`
	newHullAnswer = `error: invalid --divert "169.254.170.2:80=/x/probe.sock": 169.254.170.2:80 is diverted twice`
)

// answeringHull writes a hull that records its arguments in calls, prints
// answer and exits with code.
func answeringHull(t *testing.T, calls, answer, code string) string {
	t.Helper()
	return scriptHull(t, "printf '%s\\n' \"$*\" >> '"+calls+"'\n"+
		"cat <<'EOF'\n"+answer+"\nEOF\nexit "+code+"\n")
}

// A hull whose gateway parses a link-local divert stops on the second one
// with "diverted twice", and is taken to divert it. The probe hands the
// gateway the same divert twice and a socket under a directory of its own, so
// a hull that parses its flags exits before it serves anything.
func TestTheLinkLocalProbeTakesAHullThatParsesTheDivert(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	bin := answeringHull(t, calls, newHullAnswer, "1")
	if err := gatewayDivertsLinkLocal(bin); err != nil {
		t.Fatalf("gatewayDivertsLinkLocal = %v, want nil", err)
	}
	blob, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(blob))
	if len(args) != 7 || args[0] != "network-gateway" || args[1] != "--socket" || args[3] != "--divert" ||
		args[5] != "--divert" || args[4] != args[6] || !strings.HasPrefix(args[4], "169.254.170.2:80=/") {
		t.Fatalf("the probe ran hull with %q", blob)
	}
	dir, err := gatewayDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{args[2], strings.TrimPrefix(args[4], "169.254.170.2:80=")} {
		if strings.HasPrefix(p, dir) {
			t.Errorf("the probe names %s, in the gateway directory", p)
		}
		if _, err := os.Stat(filepath.Dir(p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the probe's directory %s outlived it: %v", filepath.Dir(p), err)
		}
	}
}

// A hull whose gateway refuses a link-local divert cannot serve a route that
// vends, and the run is refused before boot with exit 7 and the remedy.
func TestTheLinkLocalProbeRefusesAHullThatResetsLinkLocal(t *testing.T) {
	bin := answeringHull(t, filepath.Join(t.TempDir(), "calls"), oldHullAnswer, "1")
	err := gatewayDivertsLinkLocal(bin)
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.State != CannotEnforce || ce.Property != RouteModeProperty(v1alpha1.ModeEndpoint) ||
		ce.Path != (RunPath{"hull", "hvi"}) {
		t.Fatalf("gatewayDivertsLinkLocal = %#v, want a refusal of the endpoint mode on hull on hvi", err)
	}
	for _, want := range []string{"needs a hull whose gateway can divert 169.254.170.2", bin, "Upgrade hull"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// Any other answer confirms nothing, so the run is refused as unknown: a
// binary that does not run, a gateway that exits 0, and an error the probe
// does not know. Exit 0 is not taken as a yes: no hull that parses its flags
// gets past two identical diverts.
func TestTheLinkLocalProbeConcludesNothingFromAnotherAnswer(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	for name, bin := range map[string]string{
		"not a runtime": filepath.Join(t.TempDir(), "not-a-runtime"),
		"exit 0":        answeringHull(t, calls, newHullAnswer, "0"),
		"another error": answeringHull(t, calls, "flag provided but not defined: -divert", "1"),
	} {
		err := gatewayDivertsLinkLocal(bin)
		var ce *CapabilityError
		if !errors.As(err, &ce) || ce.State != Unknown || !strings.Contains(err.Error(), bin) ||
			!strings.Contains(err.Error(), "detach the policy") {
			t.Errorf("%s: gatewayDivertsLinkLocal = %v, want an unknown refusal that names %s", name, err, bin)
		}
	}
}

// hull answers the probe once per binary, as it does ProbeBrigInternal: brig
// info and a run ask more than once.
func TestHullProbeLinkLocalAsksOncePerBinary(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	h := &hull{bin: answeringHull(t, calls, oldHullAnswer, "1")}
	for range 2 {
		var ce *CapabilityError
		if err := h.ProbeLinkLocal(); !errors.As(err, &ce) || ce.State != CannotEnforce {
			t.Fatalf("ProbeLinkLocal = %v", err)
		}
	}
	if blob, _ := os.ReadFile(calls); strings.Count(string(blob), "network-gateway") != 1 {
		t.Errorf("the probe ran %d times: %q", strings.Count(string(blob), "network-gateway"), blob)
	}
}

// A gateway is asked about a link-local divert only when it is handed one. A
// boot that diverts brig.internal alone pays for no such probe. Both boots
// fail past the probes: the gateway here answers and cannot be stopped.
func TestABootWithNoLinkLocalDivertRunsNoLinkLocalProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		divert Divert
		probed bool
	}{
		{"brig.internal alone", Divert{Socket: "/b/x.sock", BrigInternal: true}, false},
		{"the link-local address", Divert{Routes: []DivertRoute{LinkLocalDivert}, Socket: "/b/x.sock",
			BrigInternal: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratchIsolatedDir(t)
			const name = "brig-probe"
			sock, err := isolatedSocket(name)
			if err != nil {
				t.Fatal(err)
			}
			listenAt(t, sock)

			calls := filepath.Join(t.TempDir(), "calls")
			bin := scriptHull(t, "echo \"$*\" >> '"+calls+"'\n"+
				"case \"$*\" in *--help) echo '   --divert string [ --divert string ]   divert'; exit 0;; esac\n"+
				"echo '"+newHullAnswer+"'\nexit 1\n")
			if _, err := ensureIsolatedGateway(bin, name, 0, Egress{Divert: tc.divert}); err == nil {
				t.Fatal("a gateway that could not be replaced was reported ready")
			}
			blob, _ := os.ReadFile(calls)
			probed := strings.Contains(string(blob), "--divert 169.254.170.2:80=")
			if probed != tc.probed {
				t.Errorf("probed = %t, want %t; the runtime was called with: %q", probed, tc.probed, blob)
			}
		})
	}
}

// An answer that confirms nothing, such as a probe that timed out under load,
// is asked again on the next run. A yes and a no are kept.
func TestHullProbeLinkLocalAsksAgainAfterAnUnknownAnswer(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	h := &hull{bin: answeringHull(t, calls, "flag provided but not defined: -divert", "1")}
	for range 2 {
		var ce *CapabilityError
		if err := h.ProbeLinkLocal(); !errors.As(err, &ce) || ce.State != Unknown {
			t.Fatalf("ProbeLinkLocal = %v, want unknown", err)
		}
	}
	if blob, _ := os.ReadFile(calls); strings.Count(string(blob), "network-gateway") != 2 {
		t.Errorf("the probe ran %d times, want 2: %q", strings.Count(string(blob), "network-gateway"), blob)
	}
	yes := &hull{bin: answeringHull(t, calls+"-yes", newHullAnswer, "1")}
	for range 2 {
		if err := yes.ProbeLinkLocal(); err != nil {
			t.Fatalf("ProbeLinkLocal = %v", err)
		}
	}
	if blob, _ := os.ReadFile(calls + "-yes"); strings.Count(string(blob), "network-gateway") != 1 {
		t.Errorf("a yes was asked %d times, want 1", strings.Count(string(blob), "network-gateway"))
	}
}
