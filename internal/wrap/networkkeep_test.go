package wrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// postureRuntime is a sandbox running on the network it booted with, and
// stale on any other: the answer hull gives on hvi when the gateway a run asks
// for is not the one the sandbox is behind.
type postureRuntime struct {
	*livenessRuntime
	booted string
}

func (r *postureRuntime) NetworkStale(_, _, net string, _ runtime.Egress) bool {
	return net != r.booted
}

// Stop takes the isolated gateway down with the sandbox, the way hull's Stop
// does through releaseGateway. Without it, a read of the posture after the
// restart still sees the old gateway, and
// TestAFlaglessRunAfterADetachRestartsOntoShared makes that read.
func (r *postureRuntime) Stop(name string) error {
	r.booted = "shared"
	return r.livenessRuntime.Stop(name)
}

// blindRuntime answers that nothing is stale, whatever it is asked. That is
// hull's answer for offline on hvi and for everything on vz.
type blindRuntime struct {
	*livenessRuntime
}

func (r *blindRuntime) NetworkStale(string, string, string, runtime.Egress) bool { return false }

// booted records what a boot of this run records, without a runtime: the
// session entry and the posture.
func booted(c *Config) {
	c.rememberSession()
	c.recordPosture()
}

// mustBootedNet is the runtime's record for this sandbox.
func mustBootedNet(t *testing.T, name string) string {
	t.Helper()
	got, err := runtime.BootedNet(name)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The report in #340. A sandbox started with --network isolated, then a verb
// that names no posture: `brig sh` resolved shared from the flag, the setting
// and the profile, read the running sandbox as stale, and restarted it onto
// the shared network -- its guest state gone and its isolation with it.
func TestAPostureGivenOnceIsFoundAgainWithoutTheFlag(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	booted(mustLoad(t, Options{Name: "video", Workspace: home, Network: "isolated"}))

	next := mustLoad(t, Options{Name: "video"})
	if next.Network != NetIsolated {
		t.Fatalf("a flagless verb resolved %q, want the posture the sandbox was started with",
			next.Network)
	}
	next.Runtime = &postureRuntime{livenessRuntime: &livenessRuntime{}, booted: "isolated"}
	if next.postureChanged() || next.networkStale() {
		t.Error("a flagless verb read the isolated sandbox as stale, so it would restart it")
	}
}

// Offline is kept the same way. A bare `brig run` on a stopped offline sandbox
// boots it offline again rather than giving it a route out.
func TestAnOfflineSandboxStaysOffline(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	booted(mustLoad(t, Options{Workspace: home, Network: "offline"}))

	if got := mustLoad(t, Options{}).Network; got != NetOffline {
		t.Errorf("a bare run resolved %q, want offline", got)
	}
}

// Asking for a different posture is still allowed, restart and all, by either
// spelling. The warning then says which posture the sandbox is leaving, which
// it is going to, and what asked for the change.
func TestAnExplicitPostureBeatsTheRememberedOne(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	booted(mustLoad(t, Options{Workspace: home, Network: "isolated"}))

	byFlag := mustLoad(t, Options{Network: "shared"})
	if byFlag.Network != NetShared {
		t.Fatalf("--network shared resolved %q", byFlag.Network)
	}
	if !byFlag.postureChanged() {
		t.Error("--network shared on an isolated sandbox was not read as a change")
	}
	got := byFlag.networkChange()
	for _, want := range []string{"isolated", "shared", "--network"} {
		if !strings.Contains(got, want) {
			t.Errorf("the restart warning does not name %q: %s", want, got)
		}
	}

	t.Setenv("BRIG_NETWORK", "shared")
	bySetting := mustLoad(t, Options{})
	if bySetting.Network != NetShared {
		t.Fatalf("BRIG_NETWORK=shared resolved %q", bySetting.Network)
	}
	if got := bySetting.networkChange(); !strings.Contains(got, "BRIG_NETWORK") {
		t.Errorf("the restart warning does not name the setting: %s", got)
	}
}

// The profile's network: is a default for a new sandbox. A sandbox already
// started with another posture keeps it, and the profile applies again once
// nothing is recorded.
func TestARememberedPostureBeatsTheProfile(t *testing.T) {
	isolateState(t)
	home := t.TempDir()
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Network = "offline"

	fresh, err := Load(p, Options{Workspace: home}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Network != NetOffline {
		t.Fatalf("with nothing recorded the profile's network: gave %q, want offline", fresh.Network)
	}

	started, err := Load(p, Options{Workspace: home, Network: "isolated"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	booted(started)

	next, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Network != NetIsolated {
		t.Errorf("the profile's network: moved a recorded sandbox to %q, want isolated", next.Network)
	}
}

// A sandbox booted before postures were recorded resolves its posture the way
// it always did, and the restart warning keeps its general wording because
// there is no recorded posture to name.
func TestASessionWithNoRecordedPostureKeepsTheOldResolution(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: t.TempDir(), Sandbox: "brig-claude-code"},
	}); err != nil {
		t.Fatal(err)
	}

	c := mustLoad(t, Options{})
	if c.Network != NetShared {
		t.Errorf("an old entry resolved %q, want shared", c.Network)
	}
	if c.postureChanged() {
		t.Error("a sandbox with no recorded posture was read as changed")
	}
	if got := c.networkChange(); !strings.Contains(got, "different network policy") {
		t.Errorf("an old entry changed the restart warning: %s", got)
	}
}

// A posture recorded for a sandbox whose session entry names another one is
// not this session's, the same rule the home and the project follow. That is
// also what an older release's `brig rm` leaves: the entry gone and the
// posture record still there.
func TestAnotherSandboxesPostureIsNotInherited(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: t.TempDir(), Sandbox: "brig-elsewhere"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", "isolated"); err != nil {
		t.Fatal(err)
	}

	if got := mustLoad(t, Options{}).Network; got != NetShared {
		t.Errorf("inherited %q through an entry naming another sandbox", got)
	}
}

// A recorded value that is not a posture is brig's own bookkeeping gone bad,
// not something anybody typed, so it is ignored rather than refusing every
// command on the session.
func TestARecordedPostureThatIsNotOneIsIgnored(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: t.TempDir(), Sandbox: "brig-claude-code"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordBootedNet("brig-claude-code", "bogus"); err != nil {
		t.Fatal(err)
	}

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatalf("a bad recorded posture refused the run: %v", err)
	}
	if c.Network != NetShared {
		t.Errorf("a bad recorded posture resolved %q, want shared", c.Network)
	}
}

// A policy isolates the sandbox it binds to, but that is the policy's doing,
// not a posture anybody asked for. The record keeps what was asked, so the
// sandbox goes back to shared once the policy is detached.
func TestAPolicyIsNotRecordedAsThePosture(t *testing.T) {
	isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	c := loadWithPolicy(t, "no-net")
	if c.Network != NetIsolated {
		t.Fatalf("the policy did not isolate the sandbox: %q", c.Network)
	}
	booted(c)
	if got := mustBootedNet(t, c.VMName); got != "shared" {
		t.Errorf("the record holds %q, the policy's posture rather than the one asked for", got)
	}
}

// The restart itself, when one is asked for: the warning names both postures
// and the running sandbox is torn down and booted again on the new one.
func TestChangingThePostureRestartsAndSaysSo(t *testing.T) {
	live := &livenessRuntime{running: true}
	c := livenessConfig(t, live)
	c.Runtime = &postureRuntime{livenessRuntime: live, booted: "isolated"}
	c.Network, c.askedNetwork = NetShared, NetShared
	c.recordedNet, c.networkSource = NetIsolated, "--network"
	var errOut bytes.Buffer
	c.Err, c.Verbosity = &errOut, Normal

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.stops != 1 || live.boots != 1 {
		t.Errorf("%d stops and %d boots, want one of each", live.stops, live.boots)
	}
	want := "this sandbox was started with the isolated posture and --network asks for shared"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("the warning does not say what changed:\n%s", errOut.String())
	}
	if got := mustBootedNet(t, c.VMName); got != "shared" {
		t.Errorf("after the restart the record holds %q, want shared", got)
	}
}

// Asking for a different posture restarts the sandbox even when the runtime
// cannot see the difference. hull reports nothing stale for offline, vz reports
// nothing stale at all, and nerdctl is never asked. Kept running, the sandbox
// stays online while every later command reads offline from the record.
func TestAPostureChangeRestartsWhenTheRuntimeCannotTell(t *testing.T) {
	for name, wrap := range map[string]func(*livenessRuntime) runtime.Runtime{
		"a runtime that sees nothing stale": func(l *livenessRuntime) runtime.Runtime { return &blindRuntime{l} },
		"a runtime that is never asked":     func(l *livenessRuntime) runtime.Runtime { return l },
	} {
		t.Run(name, func(t *testing.T) {
			live := &livenessRuntime{running: true}
			c := livenessConfig(t, live)
			c.Runtime = wrap(live)
			c.Network, c.askedNetwork = NetOffline, NetOffline
			c.recordedNet, c.networkSource = NetShared, "--network"

			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if live.stops != 1 || live.boots != 1 {
				t.Errorf("%d stops and %d boots, want one of each", live.stops, live.boots)
			}
			if live.spec.Net != "none" {
				t.Errorf("booted on %q, want none", live.spec.Net)
			}
			if got := mustBootedNet(t, c.VMName); got != "none" {
				t.Errorf("the record holds %q, want none", got)
			}
		})
	}
}

// Reusing a running sandbox records no posture. The runtime was not told a
// network by this command, so what the command asked for says nothing about
// the one the sandbox has. A sandbox booted before postures were recorded
// stays unrecorded until its next boot.
func TestReusingASandboxRecordsNoPosture(t *testing.T) {
	live := &livenessRuntime{running: true}
	c := livenessConfig(t, live)
	c.Network, c.askedNetwork = NetShared, NetShared

	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.boots != 0 {
		t.Fatalf("a running sandbox was booted %d times", live.boots)
	}
	if got := mustBootedNet(t, c.VMName); got != "" {
		t.Errorf("reuse recorded %q for a sandbox it did not boot", got)
	}
}

// An older release reads the session index into the fields it knows and writes
// it back. The posture is not one of them, so it lives in a file of its own
// and survives the rewrite.
func TestAnOlderReleaseRewritingTheIndexKeepsThePosture(t *testing.T) {
	dir := isolateState(t)
	home := t.TempDir()
	booted(mustLoad(t, Options{Name: "video", Workspace: home, Network: "isolated"}))

	type olderEntry struct {
		Home    string `json:"home"`
		Sandbox string `json:"sandbox"`
	}
	path := filepath.Join(dir, sessionIndexName)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]olderEntry{}
	if err := json.Unmarshal(blob, &old); err != nil {
		t.Fatal(err)
	}
	if blob, err = json.Marshal(old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := mustLoad(t, Options{Name: "video"}).Network; got != NetIsolated {
		t.Errorf("after an older release rewrote the index a flagless verb resolved %q", got)
	}
}

// Removing the sandbox drops its posture, so the next sandbox to take the name
// starts from the flag, the setting and the profile.
func TestRemoveDropsThePosture(t *testing.T) {
	isolateState(t)
	c := mustLoad(t, Options{Workspace: t.TempDir(), Network: "isolated"})
	booted(c)
	c.Runtime = &removingRuntime{}

	_ = c.Remove()
	if got := mustBootedNet(t, c.VMName); got != "" {
		t.Errorf("rm left %q recorded", got)
	}
}

// reportRuntime is a postureRuntime that also answers what `brig info` asks of
// a runtime, so the envelope and the JSON can be read over it.
type reportRuntime struct {
	*postureRuntime
}

func (reportRuntime) Bin() string { return "hull" }
func (reportRuntime) Isolation(hv string) runtime.Isolation {
	return fakeRuntime{}.Isolation(hv)
}

// runningAs is a sandbox that is up behind the gateway a boot on net gives it.
func runningAs(net string) *postureRuntime {
	return &postureRuntime{livenessRuntime: &livenessRuntime{running: true}, booted: net}
}

// detachedAfterBoot is the setup in #368: a sandbox booted while a policy
// isolated it, then a command resolved after the policy was detached. The
// record holds shared, the posture that was asked for, while the sandbox that
// is up is still behind its isolated gateway.
func detachedAfterBoot(t *testing.T, o Options) *Config {
	t.Helper()
	isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	booted(loadWithPolicy(t, "no-net"))
	c := mustLoad(t, o)
	if c.recordedNet != NetShared {
		t.Fatalf("the record holds %q, want shared", c.recordedNet)
	}
	return c
}

// The report in #368. A sandbox isolated by its policy, the policy detached,
// then --network offline. The restart line said the sandbox was started with
// the shared posture, which it never ran with: that is the record of what was
// asked for, and the policy had narrowed it.
func TestTheRestartLineDoesNotCallAPolicyIsolatedSandboxShared(t *testing.T) {
	c := detachedAfterBoot(t, Options{Network: "offline"})
	c.Runtime = runningAs("isolated")
	got := c.networkChange()
	if strings.Contains(got, "started with the shared posture") {
		t.Errorf("the restart line calls an isolated sandbox shared: %s", got)
	}
	want := "this sandbox was started with the isolated posture and --network asks for offline"
	if got != want {
		t.Errorf("the restart line is %q, want %q", got, want)
	}

	// The same through the restart itself, with the record written after.
	live := &livenessRuntime{running: true}
	run := livenessConfig(t, live)
	run.Runtime = &postureRuntime{livenessRuntime: live, booted: "isolated"}
	run.Network, run.askedNetwork = NetOffline, NetOffline
	run.recordedNet, run.networkSource = NetShared, "--network"
	var errOut bytes.Buffer
	run.Err, run.Verbosity = &errOut, Normal

	if err := run.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut.String(), "started with the shared posture") {
		t.Errorf("the warning calls an isolated sandbox shared:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("the warning does not name the isolated posture:\n%s", errOut.String())
	}
	if live.stops != 1 || live.boots != 1 {
		t.Errorf("%d stops and %d boots, want one of each", live.stops, live.boots)
	}
	if live.spec.Net != "none" {
		t.Errorf("booted on %q, want none", live.spec.Net)
	}
	if got := mustBootedNet(t, run.VMName); got != "none" {
		t.Errorf("the record holds %q, want none", got)
	}
}

// The other half of #368. After the detach `brig info` printed NETWORK shared
// while the isolated sandbox was still up. The row names the posture the
// running sandbox has, and the one its next boot gets.
func TestInfoDoesNotCallAPolicyIsolatedSandboxSharedAfterADetach(t *testing.T) {
	c := detachedAfterBoot(t, Options{})
	c.Runtime = reportRuntime{runningAs("isolated")}

	var block bytes.Buffer
	c.renderEnvelope(&block, creds.Set{})
	row := envelopeValue(t, block.String(), "NETWORK")
	for form, got := range map[string]string{"the envelope": row, "the JSON": c.InfoData(creds.Set{}).Network} {
		if strings.HasPrefix(got, "shared (one network") {
			t.Errorf("%s calls a sandbox behind its isolated gateway shared: %s", form, got)
		}
		if !strings.Contains(got, NetIsolated.Line()) {
			t.Errorf("%s does not name the isolated posture the sandbox runs with: %s", form, got)
		}
		if !strings.Contains(got, "shared from its next boot") {
			t.Errorf("%s does not say the next boot is shared: %s", form, got)
		}
	}
}

// The reverse. A sandbox booted shared, then a policy attached: the next boot
// isolates it, but the sandbox that is up runs shared and filters nothing, and
// a row calling it isolated overstates the boundary.
func TestInfoDoesNotCallASandboxIsolatedWhenAPolicyWasAttachedAfterItBooted(t *testing.T) {
	isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	booted(mustLoad(t, Options{}))
	c := loadWithPolicy(t, "no-net")
	c.Runtime = runningAs("shared")

	got := c.networkLine()
	if strings.HasPrefix(got, "isolated") {
		t.Errorf("the row calls a sandbox running shared isolated: %s", got)
	}
	if !strings.HasPrefix(got, NetShared.Line()) {
		t.Errorf("the row does not name the shared posture the sandbox runs with: %s", got)
	}
	if !strings.Contains(got, "isolated from its next boot") {
		t.Errorf("the row does not say the next boot is isolated: %s", got)
	}
	if c.Network != NetIsolated {
		t.Errorf("the boot this run asks for moved to %q, want isolated", c.Network)
	}
}

// Where brig cannot see the sandbox that is up, the row describes the boot
// this run asks for, as it did before. A guess about a sandbox it cannot see
// is the claim the row exists to avoid.
func TestInfoMakesNoClaimAboutASandboxItCannotSee(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) *Config{
		"no runtime": func(t *testing.T) *Config {
			c := detachedAfterBoot(t, Options{})
			c.Runtime = nil
			return c
		},
		"no runtime, a policy attached since the boot": func(t *testing.T) *Config {
			isolateState(t)
			policies := t.TempDir()
			writeTestPolicy(t, policies, "no-net")
			t.Setenv("BRIG_POLICY_DIR", policies)
			booted(mustLoad(t, Options{}))
			c := loadWithPolicy(t, "no-net")
			c.Runtime = nil
			return c
		},
		"the sandbox is stopped": func(t *testing.T) *Config {
			c := detachedAfterBoot(t, Options{})
			rt := runningAs("isolated")
			rt.running = false
			c.Runtime = rt
			return c
		},
		"the runtime cannot say": func(t *testing.T) *Config {
			c := detachedAfterBoot(t, Options{})
			rt := runningAs("isolated")
			rt.runningErr = errors.New("cannot connect")
			c.Runtime = rt
			return c
		},
		"the session entry names another sandbox": func(t *testing.T) *Config {
			isolateState(t)
			if err := writeSessionIndex(map[string]sessionEntry{
				"claude-code": {Home: t.TempDir(), Sandbox: "brig-elsewhere"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.RecordBootedNet("brig-claude-code", "shared"); err != nil {
				t.Fatal(err)
			}
			c := mustLoad(t, Options{})
			c.Runtime = runningAs("isolated")
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := setup(t)
			if got, want := c.networkLine(), c.Network.Line(); got != want {
				t.Errorf("the row is %q, want %q", got, want)
			}
		})
	}
}

// rulesRuntime is a sandbox behind the gateway it booted with and the egress
// rules it booted with, stale when a run asks for either one differently. That
// is how hull on hvi compares an isolated gateway. postureRuntime compares the
// network alone and cannot tell a policy kept from a policy detached.
type rulesRuntime struct {
	*postureRuntime
	rules runtime.Egress
}

func (r *rulesRuntime) NetworkStale(name, hv, net string, egress runtime.Egress) bool {
	if r.postureRuntime.NetworkStale(name, hv, net, egress) {
		return true
	}
	return net == "isolated" && !reflect.DeepEqual(egress, r.rules)
}

// isolatedByNoNet is a sandbox that is up behind the gateway and the rules the
// no-net policy gave it at boot.
func isolatedByNoNet(t *testing.T) *rulesRuntime {
	t.Helper()
	rules := runtimeEgress(loadWithPolicy(t, "no-net").Egress)
	return &rulesRuntime{postureRuntime: runningAs("isolated"), rules: rules}
}

// A sandbox its policy isolated, the policy detached, then --network isolated.
// The sandbox runs with the posture it is asked for, but under rules nothing
// applies now, and that is the change the general wording names. The restart
// line does not name the same posture twice as though it were a change.
func TestTheRestartLineKeepsTheGeneralWordingWhenThePostureIsTheSame(t *testing.T) {
	c := detachedAfterBoot(t, Options{Network: "isolated"})
	c.Runtime = isolatedByNoNet(t)
	if !c.networkStale() {
		t.Fatal("the rules the detached policy left were not read as stale")
	}

	got := c.networkChange()
	if strings.Contains(got, "isolated posture and --network asks for isolated") {
		t.Errorf("the restart line names one posture as a change: %s", got)
	}
	if strings.Contains(got, "started with the shared posture") {
		t.Errorf("the restart line calls an isolated sandbox shared: %s", got)
	}
	if !strings.Contains(got, "different network policy") {
		t.Errorf("the restart line lost the general wording: %s", got)
	}
}

// A sandbox its policy isolated, asked for isolated while the policy is still
// attached. It runs with the posture and the rules this run asks for. It is
// restarted all the same, because the record holds shared, and the restart
// records isolated as the posture it keeps once the policy is detached. The
// general wording claimed the policy had changed, and it had not.
func TestTheRestartLineSaysThePolicyIsolatedTheSandbox(t *testing.T) {
	isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	booted(loadWithPolicy(t, "no-net"))
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Policy = []string{"no-net"}
	c, err := Load(p, Options{Network: "isolated"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Runtime = isolatedByNoNet(t)
	if c.networkStale() {
		t.Fatal("the rules of an attached policy were read as stale")
	}
	if !c.postureChanged() {
		t.Fatal("an explicit isolated over a shared record was not read as a change")
	}

	got := c.networkChange()
	if strings.Contains(got, "different network policy") {
		t.Errorf("the restart line claims the policy changed: %s", got)
	}
	if strings.Contains(got, "isolated posture and --network asks for isolated") {
		t.Errorf("the restart line names one posture as a change: %s", got)
	}
	if strings.Contains(got, "started with the shared posture") {
		t.Errorf("the restart line calls an isolated sandbox shared: %s", got)
	}
	want := "this sandbox is isolated only because a policy narrowed it, and --network " +
		"asks for isolated as the posture it keeps"
	if got != want {
		t.Errorf("the restart line is %q, want %q", got, want)
	}
}

// The same restart through EnsureRunning. The sandbox keeps its network and its
// rules, so the warning does not give "Rules are fixed when a sandbox boots" as
// the reason. The restart is there to record the posture, and the line says so.
func TestKeepingThePostureSaysTheRestartOnlyRecordsIt(t *testing.T) {
	live := &livenessRuntime{running: true}
	run := livenessConfig(t, live)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)
	run.Egress = loadWithPolicy(t, "no-net").Egress
	run.Runtime = &rulesRuntime{
		postureRuntime: &postureRuntime{livenessRuntime: live, booted: "isolated"},
		rules:          runtimeEgress(run.Egress),
	}
	run.Network, run.askedNetwork = NetIsolated, NetIsolated
	run.recordedNet, run.networkSource = NetShared, "--network"
	var errOut bytes.Buffer
	run.Err, run.Verbosity = &errOut, Normal

	if err := run.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut.String(), "Rules are fixed when a sandbox boots") {
		t.Errorf("the warning blames rules that do not change:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "restarted to record that posture") {
		t.Errorf("the warning does not say why the sandbox restarts:\n%s", errOut.String())
	}
	if live.stops != 1 || live.boots != 1 {
		t.Errorf("%d stops and %d boots, want one of each", live.stops, live.boots)
	}
	if got := mustBootedNet(t, run.VMName); got != "isolated" {
		t.Errorf("the record holds %q, want isolated", got)
	}
}

// A sandbox behind an isolated gateway over a shared record, with no policy
// attached, asked for isolated. An older release leaves it that way when it
// boots the sandbox with --network isolated, because it does not update the
// record (see docs/policies.md). No policy narrowed this sandbox, so the
// restart line does not say one did.
func TestTheRestartLineNamesNoPolicyWhenNoneIsAttached(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	booted(mustLoad(t, Options{}))
	c := mustLoad(t, Options{Network: "isolated"})
	c.Runtime = runningAs("isolated")
	if c.Egress.Default != "" {
		t.Fatal("a policy is attached")
	}
	if !c.postureChanged() {
		t.Fatal("an explicit isolated over a shared record was not read as a change")
	}
	if c.networkStale() {
		t.Fatal("an unfiltered isolated gateway was read as stale for an unfiltered run")
	}

	got := c.networkChange()
	if strings.Contains(got, "policy") {
		t.Errorf("the restart line names a policy that is not attached: %s", got)
	}
	want := "this sandbox is isolated but its record says shared, and --network " +
		"asks for isolated as the posture it keeps"
	if got != want {
		t.Errorf("the restart line is %q, want %q", got, want)
	}
}

// A bare `brig run` after the detach. The rule keeps the posture that was
// asked for, which is shared, and the sandbox does not keep running under
// rules nobody applies any more, so it is restarted onto shared. The warning
// keeps the general wording: nobody asked for a different posture.
func TestAFlaglessRunAfterADetachRestartsOntoShared(t *testing.T) {
	c := detachedAfterBoot(t, Options{})
	if c.Network != NetShared {
		t.Fatalf("a flagless run after the detach resolved %q, want shared", c.Network)
	}
	if c.postureChanged() {
		t.Error("a flagless run after the detach was read as a posture change")
	}

	live := &livenessRuntime{running: true}
	run := livenessConfig(t, live)
	run.Runtime = &postureRuntime{livenessRuntime: live, booted: "isolated"}
	run.Network, run.askedNetwork = NetShared, NetShared
	run.recordedNet, run.networkSource = NetShared, "the posture this sandbox was started with"
	var errOut bytes.Buffer
	run.Err, run.Verbosity = &errOut, Normal

	if err := run.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	if live.stops != 1 || live.boots != 1 {
		t.Errorf("%d stops and %d boots, want one of each", live.stops, live.boots)
	}
	if live.spec.Net != "shared" {
		t.Errorf("booted on %q, want shared", live.spec.Net)
	}
	if !strings.Contains(errOut.String(), "different network policy") {
		t.Errorf("the warning does not use the general wording:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "asks for") {
		t.Errorf("the warning names a posture change nobody asked for:\n%s", errOut.String())
	}
	if got := mustBootedNet(t, run.VMName); got != "shared" {
		t.Errorf("the record holds %q, want shared", got)
	}
	// After the restart the isolated gateway is gone, so the sandbox runs
	// shared and the row says so with no second posture.
	if got := run.runningNet(); got != NetShared {
		t.Errorf("after the restart the sandbox reads as %q, want shared", got)
	}
	if got, want := run.networkLine(), NetShared.Line(); got != want {
		t.Errorf("after the restart the row is %q, want %q", got, want)
	}
}

// Only an isolated gateway is visible to the runtime. Offline and isolated
// come from the record as they are, and a shared record with no gateway to
// contradict it stays shared. A runtime that cannot see gateways at all never
// turns a record into isolated.
func TestTheRunningPostureComesFromTheRecordWhenTheRuntimeCannotTell(t *testing.T) {
	for name, rt := range map[string]runtime.Runtime{
		"a runtime that sees nothing stale": &blindRuntime{&livenessRuntime{running: true}},
		"a runtime that is never asked":     &livenessRuntime{running: true},
	} {
		for _, rec := range []Network{NetShared, NetIsolated, NetOffline} {
			t.Run(name+", recorded "+string(rec), func(t *testing.T) {
				c := livenessConfig(t, &livenessRuntime{running: true})
				c.Runtime, c.recordedNet = rt, rec
				if got := c.runningNet(); got != rec {
					t.Errorf("runningNet = %q, want the recorded %q", got, rec)
				}
			})
		}
	}
	c := livenessConfig(t, &livenessRuntime{running: true})
	c.Runtime, c.recordedNet = runningAs("shared"), NetShared
	if got := c.runningNet(); got != NetShared {
		t.Errorf("a shared sandbox with no isolated gateway read as %q", got)
	}
	c.Runtime = runningAs("isolated")
	if got := c.runningNet(); got != NetIsolated {
		t.Errorf("a shared record behind an isolated gateway read as %q, want isolated", got)
	}
	c.recordedNet = ""
	if got := c.runningNet(); got != "" {
		t.Errorf("a sandbox with no record read as %q, want no answer", got)
	}
}

// envelopeValue is the value of the row labelled label in a rendered envelope.
func envelopeValue(t *testing.T, block, label string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if rest, ok := strings.CutPrefix(line, label+" "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no %s row:\n%s", label, block)
	return ""
}

// countingRuntime counts the NetworkStale questions put to it. On hvi each one
// dials the sandbox's gateway with a two-second timeout.
type countingRuntime struct {
	*rulesRuntime
	asked int
}

func (r *countingRuntime) NetworkStale(name, hv, net string, egress runtime.Egress) bool {
	r.asked++
	return r.rulesRuntime.NetworkStale(name, hv, net, egress)
}

// One restart warning looks at the running sandbox once for its posture and
// once for its rules. It asked five times, and a gateway slow to answer made
// every restart for the network wait on each of them.
func TestTheRestartLineAsksTheRuntimeOnceForEachAnswer(t *testing.T) {
	isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	booted(loadWithPolicy(t, "no-net"))
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Policy = []string{"no-net"}
	c, err := Load(p, Options{Network: "isolated"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingRuntime{rulesRuntime: isolatedByNoNet(t)}
	c.Runtime = counted

	got := c.networkRestart()
	if !strings.Contains(got, "It is restarted to record that posture") {
		t.Fatalf("not the keep-the-posture warning, so this test proves nothing: %s", got)
	}
	if counted.asked > 2 {
		t.Errorf("one restart warning asked the runtime %d times, want at most 2", counted.asked)
	}
}
