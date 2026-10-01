package wrap

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// Linux does not implement NetworkChecker. Recovering its persisted network
// must suffice both to keep an old sandbox and to honour a deliberate change.
type legacyNetworkRuntime struct {
	*livenessRuntime
	kind, network string
	inspectErr    error
	inspections   int
}

func (r *legacyNetworkRuntime) Kind() string { return r.kind }
func (r *legacyNetworkRuntime) SandboxNetwork(string) (string, error) {
	r.inspections++
	return r.network, r.inspectErr
}

type legacyHVIRuntime struct{ *legacyNetworkRuntime }

func (r *legacyHVIRuntime) NetworkStale(_, _, net string, _ runtime.Egress) bool {
	return net != r.network
}

type staleLegacyHVIRuntime struct {
	*legacyNetworkRuntime
	checks int
}

func (r *staleLegacyHVIRuntime) NetworkStale(_, _, _ string, _ runtime.Egress) bool {
	r.checks++
	return true
}

func legacySession(t *testing.T, agent string) (profile.Profile, string) {
	t.Helper()
	isolateState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	p, ok := profile.Lookup(agent)
	if !ok || p.Network != "isolated" {
		t.Fatalf("need the shipped isolated profile %s", agent)
	}
	home := t.TempDir()
	if err := writeSessionIndex(map[string]sessionEntry{
		agent: {Home: home, Sandbox: "brig-" + agent},
	}); err != nil {
		t.Fatal(err)
	}
	return p, home
}

func TestAnUnrecordedSandboxKeepsItsActualNetworkOnUpgrade(t *testing.T) {
	for _, kind := range []string{"hull", "nerdctl"} {
		for _, net := range AllNetworks() {
			t.Run(kind+"/"+string(net), func(t *testing.T) {
				p, home := legacySession(t, "ubuntu")
				live := &legacyNetworkRuntime{
					livenessRuntime: &livenessRuntime{running: true, workspace: home},
					kind:            kind, network: net.RuntimeNet(),
				}
				var rt runtime.Runtime = live
				if kind == "hull" {
					rt = &legacyHVIRuntime{live}
				}
				c, err := Load(p, Options{NoProject: true}, rt)
				if err != nil {
					t.Fatal(err)
				}
				c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
				if c.Network != net || !strings.HasPrefix(c.networkLine(), net.Line()) {
					t.Errorf("legacy %s reported %q, resolved %s", net, c.networkLine(), c.Network)
				}
				if live.inspections != 1 {
					t.Errorf("inspected the legacy network %d times, want once", live.inspections)
				}
				if got := mustBootedNet(t, c.VMName); got != "" {
					t.Fatalf("Load or info recorded posture %q", got)
				}
				if err := c.EnsureRunning(creds.Set{}); err != nil {
					t.Fatal(err)
				}
				if live.stops != 0 || live.removes != 0 || live.boots != 0 {
					t.Errorf("upgrade performed %d stops, %d removals, %d boots", live.stops, live.removes, live.boots)
				}
				if got := mustBootedNet(t, c.VMName); got != net.RuntimeNet() {
					t.Errorf("successful reuse recorded %q, want recovered %q", got, net.RuntimeNet())
				}
				live.inspectErr = errors.New("inspection is no longer available")
				again, err := Load(p, Options{NoProject: true}, rt)
				if err != nil || live.inspections != 1 || again.Network != net {
					t.Errorf("later Load did not use the recovered record: %v, inspections=%d", err, live.inspections)
				}
			})
		}
	}
}

func TestAnExplicitChangeFromALegacyNetworkRestartsOnLinux(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{running: true, workspace: home},
		kind:            "nerdctl", network: "shared",
	}
	c, err := Load(p, Options{NoProject: true, Network: "offline"}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.stops != 1 || live.boots != 1 || live.spec.Net != "none" {
		t.Errorf("explicit offline reused a legacy shared guest: stops=%d boots=%d net=%s", live.stops, live.boots, live.spec.Net)
	}
}

// Inspection can recover a posture while the current gateway settings point
// elsewhere. Missing allocator state there must not authorize a restart of
// the guest that still uses its original gateway.
func TestARecoveredNetworkNeedsAnExplicitChoiceToReplaceStaleGatewaySettings(t *testing.T) {
	for _, posture := range []Network{NetShared, NetIsolated} {
		for _, explicit := range []bool{false, true} {
			choice := "flagless"
			if explicit {
				choice = "explicit"
			}
			t.Run(string(posture)+"/"+choice, func(t *testing.T) {
				p, home := legacySession(t, "ubuntu")
				live := &staleLegacyHVIRuntime{legacyNetworkRuntime: &legacyNetworkRuntime{
					livenessRuntime: &livenessRuntime{running: true, workspace: home},
					kind:            "hull", network: posture.RuntimeNet(),
				}}
				o := Options{NoProject: true}
				if explicit {
					o.Network = string(posture)
				}
				c, err := Load(p, o, live)
				if err != nil {
					t.Fatal(err)
				}
				c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
				err = c.EnsureRunning(creds.Set{})
				if explicit {
					if err != nil {
						t.Fatal(err)
					}
					if live.stops != 1 || live.removes == 0 || live.boots != 1 || live.spec.Net != posture.RuntimeNet() {
						t.Fatalf("explicit choice did not recreate the guest: stops=%d removes=%d boots=%d network=%q",
							live.stops, live.removes, live.boots, live.spec.Net)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "current gateway configuration") || !strings.Contains(err.Error(), "--network") {
					t.Errorf("stale recovered configuration was not refused with a remedy: %v", err)
				}
				if live.stops != 0 || live.removes != 0 || live.boots != 0 {
					t.Errorf("flagless recovery mutated the guest: stops=%d removes=%d boots=%d", live.stops, live.removes, live.boots)
				}
				if got := mustBootedNet(t, c.VMName); got != "" {
					t.Errorf("refused recovery recorded posture %q", got)
				}
				if live.checks != 1 {
					t.Errorf("checked the stale gateway %d times, want once", live.checks)
				}
			})
		}
	}
}

func TestFailedLegacyReuseDoesNotRecordTheInspectedPosture(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{workspace: home, runningErr: errors.New("runtime went away")},
		kind:            "nerdctl", network: "shared",
	}
	c, err := Load(p, Options{NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if err := c.EnsureRunning(creds.Set{}); err == nil {
		t.Fatal("accepted a guest whose running state could not be checked")
	}
	if got := mustBootedNet(t, c.VMName); got != "" {
		t.Errorf("failed reuse recorded posture %q", got)
	}
}

func TestALegacyNetworkInspectionFailureDoesNotBecomeTheDefault(t *testing.T) {
	p, home := legacySession(t, "claude-code")
	live := &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{running: true, workspace: home},
		kind:            "nerdctl", inspectErr: errors.New("runtime unavailable"),
	}
	c, err := Load(p, Options{}, live)
	if err != nil {
		t.Fatalf("inspection failure blocked info, stop and rm from loading the session: %v", err)
	}
	if got := c.networkLine(); !strings.HasPrefix(got, "unknown") || strings.Contains(got, "isolated") {
		t.Errorf("inspection failure reported the default network: %s", got)
	}
	if err := c.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("inspection failure became a default instead of refusing the run: %v", err)
	}
	if live.stops != 0 || live.removes != 0 || live.boots != 0 {
		t.Error("inspection failure mutated the existing sandbox")
	}
}

func TestAnUnknownLegacyNetworkIsReportedAndCannotBeReusedFlaglessly(t *testing.T) {
	p, _ := legacySession(t, "claude-code")
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.networkLine(); !strings.HasPrefix(got, "unknown") || strings.Contains(got, "isolated") {
		t.Fatalf("uninspected legacy network was claimed as known: %s", got)
	}
	if err := c.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "--network") {
		t.Errorf("uninspected legacy sandbox was reused: %v", err)
	}
}

func TestAnExplicitNetworkCanRecreateAnUninspectableLegacySandbox(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{running: true, workspace: home},
		kind:            "nerdctl", inspectErr: errors.New("unrecognized legacy configuration"),
	}
	c, err := Load(p, Options{NoProject: true, Network: "offline"}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if !strings.HasPrefix(c.networkLine(), "unknown") || !strings.Contains(c.networkChange(), "previous network is unknown") {
		t.Error("an explicit choice invented the unknown sandbox's previous network")
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.stops != 1 || live.boots != 1 || live.spec.Net != "none" {
		t.Errorf("did not recreate on the explicit offline network: stops=%d boots=%d net=%s", live.stops, live.boots, live.spec.Net)
	}
	if got := c.networkLine(); strings.HasPrefix(got, "unknown") {
		t.Errorf("successful boot still reports an unknown network: %s", got)
	}
}

func TestAStaleLegacyEntryDoesNotStopANewSandboxGettingIsolation(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{livenessRuntime: &livenessRuntime{workspace: home}, kind: "nerdctl"}
	c, err := Load(p, Options{NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.spec.Net != "isolated" || live.boots != 1 {
		t.Errorf("confirmed-absent legacy sandbox did not boot isolated: %+v", live.spec)
	}
}

func TestASandboxAppearingAfterLegacyInspectionIsNotSilentlyJoined(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{livenessRuntime: &livenessRuntime{workspace: home}, kind: "nerdctl"}
	c, err := Load(p, Options{NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	live.running, live.network = true, "shared"
	if err := c.EnsureRunning(creds.Set{}); err == nil || !strings.Contains(err.Error(), "appeared") {
		t.Errorf("a newly appeared sandbox was reused as isolated: %v", err)
	}
	if live.stops != 0 || live.boots != 0 {
		t.Error("a sandbox that appeared during resolution was replaced")
	}
}

func TestAStoppedUnrecordedSandboxKeepsItsOfflineNetwork(t *testing.T) {
	p, home := legacySession(t, "ubuntu")
	live := &legacyNetworkRuntime{
		livenessRuntime: &livenessRuntime{workspace: home},
		kind:            "nerdctl", network: "none",
	}
	c, err := Load(p, Options{NoProject: true}, live)
	if err != nil {
		t.Fatal(err)
	}
	c.Verify, c.Out, c.Err = verify.Off, io.Discard, io.Discard
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.spec.Net != "none" || live.boots != 1 {
		t.Error("a stopped legacy offline sandbox gained a network on restart")
	}
}
