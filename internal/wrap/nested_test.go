package wrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

const capabilitiesRow = "CAPABILITIES  kvm (nested virtualization: the guest can run VMs of its own; " +
	"brig's view of the guest does not extend into them)"

// nestedChecker is a runtime that boots like livenessRuntime and refuses a
// kvm run the way hull does on a host without EL2, recording every spec it was
// asked about.
type nestedChecker struct {
	*livenessRuntime
	refuse error
	asked  []runtime.RunSpec
}

func (r *nestedChecker) CanRun(spec runtime.RunSpec) error {
	r.asked = append(r.asked, spec)
	if spec.NestedVirt {
		return r.refuse
	}
	return nil
}

// probingRuntime is fakeRuntime with an answer to the nested question.
type probingRuntime struct {
	fakeRuntime
	s runtime.NestedSupport
}

func (p probingRuntime) NestedVirt() runtime.NestedSupport { return p.s }

// The envelope is where a reader is told what a run is about to trust, and a
// guest with a hypervisor of its own is the one run where brig's view of the
// guest stops covering everything in it. Said before the boot, in one row.
func TestEnvelopeNamesTheCapabilityWhenAsked(t *testing.T) {
	c := envelopeConfig()
	c.Profile.Capabilities = []string{profile.CapabilityKVM}
	out := &bytes.Buffer{}
	c.renderEnvelope(out, creds.Set{})
	if !strings.Contains(out.String(), capabilitiesRow) {
		t.Errorf("no CAPABILITIES row, or not the agreed wording:\n%s", out.String())
	}
}

// And on every other run it says nothing: a row reading "none" on every run
// trains the eye to skip the line on the run where it matters.
func TestEnvelopeOmitsCapabilitiesByDefault(t *testing.T) {
	c := envelopeConfig()
	out := &bytes.Buffer{}
	c.renderEnvelope(out, creds.Set{})
	if strings.Contains(out.String(), "CAPABILITIES") {
		t.Errorf("a run that asked for nothing printed a CAPABILITIES row:\n%s", out.String())
	}
}

// The spec a backend is asked about carries the capability, so the refusal can
// happen; and a profile that did not ask carries nothing.
func TestBackendSpecCarriesNestedVirt(t *testing.T) {
	c := envelopeConfig()
	if c.backendSpec("hvi").NestedVirt {
		t.Error("a profile with no capability asked the backend for nested virtualization")
	}
	c.Profile.Capabilities = []string{profile.CapabilityKVM}
	if !c.backendSpec("hvi").NestedVirt {
		t.Error("a kvm profile did not ask the backend for nested virtualization")
	}
}

// Refused before the workspace is prepared, and nothing booted. The backend
// refusal is the one brig makes itself; whether the host can nest is hull's to
// refuse at boot. The marker is
// the first thing PrepareWorkspace writes, so its absence is the evidence.
func TestEnsureRunningRefusesNestedBeforePreparingTheWorkspace(t *testing.T) {
	rt := &nestedChecker{livenessRuntime: &livenessRuntime{},
		refuse: errors.New(`nested virtualization (capability kvm) needs the hvi backend (BRIG_HYPERVISOR is "vz")`)}
	c := livenessConfig(t, rt.livenessRuntime)
	c.Runtime = rt
	c.Profile.Capabilities = []string{profile.CapabilityKVM}

	err := c.EnsureRunning(creds.Set{})
	if err == nil || !strings.Contains(err.Error(), "needs the hvi backend") {
		t.Fatalf("a kvm run its backend cannot give was not refused: %v", err)
	}
	if rt.boots != 0 {
		t.Errorf("booted %d sandboxes after the refusal", rt.boots)
	}
	if _, statErr := os.Stat(filepath.Join(c.Workspace, markerFile)); !os.IsNotExist(statErr) {
		t.Errorf("the workspace was prepared before the refusal: marker present (%v)", statErr)
	}
}

// The path that boots nothing. A sandbox already up is joined and Run is never
// called, so a refusal that lived only in Run would be waved past here, and
// the envelope would print a CAPABILITIES row over a guest that may have none.
func TestEnsureRunningRefusesNestedOnThePathThatBootsNothing(t *testing.T) {
	rt := &nestedChecker{livenessRuntime: &livenessRuntime{running: true},
		refuse: errors.New(`nested virtualization (capability kvm) needs the hvi backend (BRIG_HYPERVISOR is "vz")`)}
	c := livenessConfig(t, rt.livenessRuntime)
	c.Runtime = rt
	c.Profile.Capabilities = []string{profile.CapabilityKVM}

	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("a running sandbox was joined for a kvm run the backend refuses")
	}
	if rt.boots != 0 || rt.stops != 0 || rt.removes != 0 {
		t.Errorf("the refusal touched the sandbox: %d boots, %d stops, %d removes",
			rt.boots, rt.stops, rt.removes)
	}
}

// Where the backend allows it, the boot carries the capability; and a run that
// did not ask boots without it, which is the default the rest of this holds.
func TestEnsureRunningBootsWithNestedVirtOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		want bool
	}{
		{"default", nil, false},
		{"kvm", []string{profile.CapabilityKVM}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &nestedChecker{livenessRuntime: &livenessRuntime{}}
			c := livenessConfig(t, rt.livenessRuntime)
			c.Runtime = rt
			c.Profile.Capabilities = tc.caps
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatalf("the run was refused: %v", err)
			}
			if rt.boots != 1 {
				t.Fatalf("booted %d times, want 1", rt.boots)
			}
			if rt.spec.NestedVirt != tc.want {
				t.Errorf("the booted spec has NestedVirt=%v, want %v", rt.spec.NestedVirt, tc.want)
			}
		})
	}
}

// brig info answers "could I turn this on here" before anyone edits a profile.
func TestStatusReportsTheRuntimesNestedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    runtime.NestedSupport
		want string
	}{
		{"supported", runtime.NestedSupport{Supported: true, Backend: "hvi"},
			"nested virtualization: supported (backend hvi)"},
		{"unsupported", runtime.NestedSupport{Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"},
			"nested virtualization: not supported on this host: Hypervisor.framework reports no EL2"},
		// A hull too old to know the question is named, and the host is not
		// called unable.
		{"outdated", runtime.NestedSupport{Outdated: true,
			Detail: "this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
			"nested virtualization: this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
		// A hull that cannot answer is not supported, with its reason.
		{"no answer", runtime.NestedSupport{Detail: "hull capabilities --json: exit status 1"},
			"nested virtualization: not supported on this host: hull capabilities --json: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := bindingConfig(t, "")
			out := &bytes.Buffer{}
			c.Out = out
			c.Runtime = probingRuntime{s: tc.s}
			c.Status(creds.Set{})
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out.String())
			}
		})
	}
}

// A runtime with no answer prints no line, and a profile that asks for
// nothing prints no capabilities line.
func TestStatusSaysNothingWithoutAnAnswerOrARequest(t *testing.T) {
	got := statusOutput(t, "", creds.Set{})
	for _, unwanted := range []string{"nested virtualization", "capabilities:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the report printed %q with nothing to say:\n%s", unwanted, got)
		}
	}
}

func TestStatusNamesTheCapabilityAsked(t *testing.T) {
	c := bindingConfig(t, "")
	c.Profile.Capabilities = []string{profile.CapabilityKVM}
	out := &bytes.Buffer{}
	c.Out = out
	c.Runtime = fakeRuntime{}
	c.Status(creds.Set{})
	if !strings.Contains(out.String(), "capabilities: kvm") {
		t.Errorf("the report does not name the capability:\n%s", out.String())
	}
}

// The JSON carries the same two facts, and only when there is something to
// carry: both fields are additive and omitted on every run that has neither.
func TestInfoDataCarriesCapabilitiesAndTheNestedAnswer(t *testing.T) {
	c := envelopeConfig()
	c.Profile.Capabilities = []string{profile.CapabilityKVM}
	c.Runtime = probingRuntime{s: runtime.NestedSupport{Supported: true, Backend: "hvi", Detail: "EL2"}}
	blob, err := json.Marshal(c.InfoData(creds.Set{}))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Capabilities         []string `json:"capabilities"`
		NestedVirtualization *struct {
			Supported bool   `json:"supported"`
			Backend   string `json:"backend"`
			Detail    string `json:"detail"`
		} `json:"nestedVirtualization"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Capabilities) != 1 || doc.Capabilities[0] != "kvm" {
		t.Errorf("capabilities = %v, want [kvm]\n%s", doc.Capabilities, blob)
	}
	n := doc.NestedVirtualization
	if n == nil || !n.Supported || n.Backend != "hvi" || n.Detail != "EL2" {
		t.Errorf("nestedVirtualization = %+v\n%s", n, blob)
	}
}

func TestInfoDataOmitsBothWhenThereIsNothingToSay(t *testing.T) {
	c := envelopeConfig()
	blob, err := json.Marshal(c.InfoData(creds.Set{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"capabilities"`, `"nestedVirtualization"`} {
		if strings.Contains(string(blob), key) {
			t.Errorf("%s is present on a run with nothing to report:\n%s", key, blob)
		}
	}
}
