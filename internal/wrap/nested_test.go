package wrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// joinRuntime is a sandbox that is already up, booted nested or not as hull's
// instance record says, on a host whose answer to the nested question is host.
// It counts the questions, so a case can tell that a run asked nothing it did
// not need to.
type joinRuntime struct {
	*livenessRuntime
	nested      bool
	inspectErr  error
	host        runtime.NestedSupport
	inspections int
	probes      int
}

func (r *joinRuntime) RunningNestedVirt(string) (bool, error) {
	r.inspections++
	return r.nested, r.inspectErr
}

func (r *joinRuntime) NestedVirt() runtime.NestedSupport {
	r.probes++
	return r.host
}

func joinConfig(t *testing.T, nested bool, caps []string) (*Config, *joinRuntime) {
	t.Helper()
	rt := &joinRuntime{livenessRuntime: &livenessRuntime{running: true}, nested: nested,
		host: runtime.NestedSupport{Supported: true, Backend: "hvi"}}
	c := livenessConfig(t, rt.livenessRuntime)
	c.Runtime = rt
	c.Profile.Capabilities = caps
	return c, rt
}

// The gap this closes: a profile that no longer asks for kvm, joining a guest
// booted with it, printed no CAPABILITIES row over a guest that still has
// /dev/kvm. It must not join silently. It restarts, the way a changed network
// posture does, so the guest that ends up running is the one the envelope
// describes.
func TestJoiningANestedSandboxWithoutKVMRestartsIt(t *testing.T) {
	c, rt := joinConfig(t, true, nil)
	errb := &bytes.Buffer{}
	c.Err = errb

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if rt.stops == 0 || rt.removes == 0 || rt.boots != 1 {
		t.Fatalf("a nested guest was joined by a profile without kvm (%d stops, %d removes, %d boots)",
			rt.stops, rt.removes, rt.boots)
	}
	if rt.spec.NestedVirt {
		t.Error("the restart booted the guest nested again")
	}
	if !strings.Contains(errb.String(), "started with nested virtualization") {
		t.Errorf("the restart was not explained:\n%s", errb.String())
	}
	// Turning nesting off needs nothing of the host.
	if rt.probes != 0 {
		t.Errorf("the host was asked about nesting for a restart that turns it off (%d)", rt.probes)
	}
}

// The other direction: a kvm profile joining a guest booted without it would
// print a kvm row over a guest with no /dev/kvm. The host can nest, so it
// restarts.
func TestJoiningAPlainSandboxWithKVMRestartsIt(t *testing.T) {
	c, rt := joinConfig(t, false, []string{profile.CapabilityKVM})
	errb := &bytes.Buffer{}
	c.Err = errb

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if rt.boots != 1 || !rt.spec.NestedVirt {
		t.Errorf("the guest was not restarted nested: %d boots, NestedVirt=%v", rt.boots, rt.spec.NestedVirt)
	}
	if !strings.Contains(errb.String(), "started without nested virtualization") {
		t.Errorf("the restart was not explained:\n%s", errb.String())
	}
	if rt.probes != 1 {
		t.Errorf("the host was asked %d times before the restart, want once", rt.probes)
	}
}

// The failure this guards: the restart that turns nesting on used to stop and
// remove the running sandbox and only then learn, from hull's refusal at boot,
// that the host cannot nest. The sandbox was gone and nothing replaced it. The
// host is asked first now, and a "no" leaves the sandbox exactly as it was.
func TestRestartingIntoKVMIsRefusedBeforeTheSandboxIsStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		host runtime.NestedSupport
		want string
	}{
		{"host without EL2", runtime.NestedSupport{Backend: "hvi", Detail: "Hypervisor.framework reports no EL2"},
			"nested virtualization requested but not supported by this host: Hypervisor.framework reports no EL2"},
		{"outdated hull", runtime.NestedSupport{Outdated: true,
			Detail: "this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
			"this hull (0.1.0-rc29) predates nested virtualization; upgrade hull"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rt := joinConfig(t, false, []string{profile.CapabilityKVM})
			rt.host = tc.host
			err := c.EnsureRunning(creds.Set{})
			if err == nil {
				t.Fatal("a restart into kvm on a host that cannot nest was not refused")
			}
			for _, want := range []string{tc.want, "was left as it is"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			if rt.stops != 0 || rt.removes != 0 || rt.boots != 0 {
				t.Errorf("the running sandbox was touched (%d stops, %d removes, %d boots)",
					rt.stops, rt.removes, rt.boots)
			}
		})
	}
}

// When the guest matches the profile it is joined as before: a restart nobody
// needed would disconnect every other session on it. The host is not asked.
func TestJoiningASandboxThatMatchesKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nested bool
		caps   []string
	}{
		{"both plain", false, nil},
		{"both nested", true, []string{profile.CapabilityKVM}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rt := joinConfig(t, tc.nested, tc.caps)
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatalf("the run failed: %v", err)
			}
			if rt.boots != 0 || rt.stops != 0 || rt.removes != 0 {
				t.Errorf("a matching sandbox was restarted (%d stops, %d removes, %d boots)",
					rt.stops, rt.removes, rt.boots)
			}
			if rt.inspections != 1 || rt.probes != 0 {
				t.Errorf("asked %d inspections and %d host probes, want 1 and 0", rt.inspections, rt.probes)
			}
		})
	}
}

// A sandbox that exited between Running and the question about how it booted
// is not running. The run boots a new one; refusing would turn an ordinary
// race into an error the user cannot act on.
func TestJoiningASandboxThatExitedBootsANewOne(t *testing.T) {
	c, rt := joinConfig(t, false, nil)
	rt.inspectErr = fmt.Errorf("hull inspect %s: %w", c.VMName, runtime.ErrSandboxGone)
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("a sandbox that exited before the question was refused: %v", err)
	}
	if rt.boots != 1 || rt.stops != 0 {
		t.Errorf("want one fresh boot and no stop, got %d boots, %d stops", rt.boots, rt.stops)
	}
}

// A runtime that cannot say how the guest booted is refused, and the sandbox is
// left alone: joining would print an envelope about a guest nobody checked,
// and restarting on a guess would disconnect sessions over a question that
// went unanswered.
func TestJoiningIsRefusedWhenTheNestedStateIsUnknown(t *testing.T) {
	c, rt := joinConfig(t, false, nil)
	rt.inspectErr = errors.New("hull inspect brig-liveness: exit status 1")
	err := c.EnsureRunning(creds.Set{})
	if err == nil {
		t.Fatal("a sandbox whose nested state is unknown was joined")
	}
	for _, want := range []string{"nested virtualization", "left as it is", "hull inspect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if rt.boots != 0 || rt.stops != 0 || rt.removes != 0 {
		t.Errorf("the refusal touched the sandbox (%d stops, %d removes, %d boots)",
			rt.stops, rt.removes, rt.boots)
	}
}

// A fresh boot of a kvm profile asks the host nothing: there is no running
// sandbox to lose. The note in runtime's supports says why a boot does not.
func TestAFreshKVMBootDoesNotAskTheHost(t *testing.T) {
	c, rt := joinConfig(t, false, []string{profile.CapabilityKVM})
	rt.running = false
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if rt.probes != 0 || rt.inspections != 0 {
		t.Errorf("a fresh boot asked %d host probes and %d inspections, want none", rt.probes, rt.inspections)
	}
	if rt.boots != 1 || !rt.spec.NestedVirt {
		t.Errorf("the fresh boot: %d boots, NestedVirt=%v", rt.boots, rt.spec.NestedVirt)
	}
}

// infoRuntime answers what brig info asks about a sandbox that may be running:
// the report questions of fakeRuntime, whether it is up, and how hull booted it.
type infoRuntime struct {
	fakeRuntime
	running     bool
	nested      bool
	inspectErr  error
	inspections int
}

func (r *infoRuntime) Running(string) (bool, error) { return r.running, nil }
func (r *infoRuntime) RunningNestedVirt(string) (bool, error) {
	r.inspections++
	return r.nested, r.inspectErr
}
func (r *infoRuntime) NestedVirt() runtime.NestedSupport {
	return runtime.NestedSupport{Supported: true, Backend: "hvi"}
}

// brig info describes the profile, which is what the next run boots. When the
// sandbox running now was booted the other way, it says so in one line, in
// either direction, and the envelope rows stay the profile's.
func TestInfoSaysWhenTheRunningGuestDiffersFromTheProfile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nested bool
		caps   []string
		want   string
	}{
		{"nested guest, plain profile", true, nil,
			"the running sandbox was booted with nested virtualization; the profile no longer asks for it, so the next run restarts it"},
		{"plain guest, kvm profile", false, []string{profile.CapabilityKVM},
			"the running sandbox was booted without nested virtualization; the profile asks for it, so the next run restarts it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := bindingConfig(t, "")
			c.Profile.Capabilities = tc.caps
			out := &bytes.Buffer{}
			c.Out = out
			c.Runtime = &infoRuntime{running: true, nested: tc.nested}
			c.Info(creds.Set{})
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out.String())
			}
			if tc.nested && strings.Contains(out.String(), "CAPABILITIES") {
				t.Errorf("the envelope took the running guest's capability, not the profile's:\n%s", out.String())
			}
		})
	}
}

// No line when the guest matches the profile, when nothing is running, or when
// hull could not say: info reports, and must not fail or guess.
func TestInfoSaysNothingWhenTheRunningGuestMatchesOrCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *infoRuntime
		caps []string
	}{
		{"both plain", &infoRuntime{running: true}, nil},
		{"both nested", &infoRuntime{running: true, nested: true}, []string{profile.CapabilityKVM}},
		{"not running", &infoRuntime{nested: true}, nil},
		{"inspect fails", &infoRuntime{running: true, nested: true, inspectErr: errors.New("hull inspect: exit status 1")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := bindingConfig(t, "")
			c.Profile.Capabilities = tc.caps
			out := &bytes.Buffer{}
			c.Out = out
			c.Runtime = tc.rt
			c.Info(creds.Set{})
			if strings.Contains(out.String(), "the running sandbox was booted") {
				t.Errorf("a line about the running guest was printed:\n%s", out.String())
			}
		})
	}
	// A sandbox that is not running is not inspected at all.
	rt := &infoRuntime{}
	c := bindingConfig(t, "")
	c.Runtime = rt
	c.Info(creds.Set{})
	if rt.inspections != 0 {
		t.Errorf("info inspected a sandbox that is not running (%d)", rt.inspections)
	}
}

// The JSON carries the running guest's state beside hull's answer, and leaves
// it out when no sandbox is running.
func TestInfoDataCarriesTheRunningGuestsNestedState(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *infoRuntime
		want string
	}{
		{"nested", &infoRuntime{running: true, nested: true}, `"runningNested":true`},
		{"plain", &infoRuntime{running: true}, `"runningNested":false`},
		{"not running", &infoRuntime{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := envelopeConfig()
			c.Runtime = tc.rt
			blob, err := json.Marshal(c.InfoData(creds.Set{}))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(string(blob), "runningNested") {
					t.Errorf("runningNested present with nothing running:\n%s", blob)
				}
				return
			}
			if !strings.Contains(string(blob), tc.want) {
				t.Errorf("want %s in:\n%s", tc.want, blob)
			}
		})
	}
}
