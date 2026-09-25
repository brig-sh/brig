package wrap

import (
	"bytes"
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

// The report in #340. A sandbox started with --network isolated, then a verb
// that names no posture: `brig sh` resolved shared from the flag, the setting
// and the profile, read the running sandbox as stale, and restarted it onto
// the shared network -- its guest state gone and its isolation with it.
func TestAPostureGivenOnceIsFoundAgainWithoutTheFlag(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	mustLoad(t, Options{Name: "video", Workspace: home, Network: "isolated"}).rememberSession()

	next := mustLoad(t, Options{Name: "video"})
	if next.Network != NetIsolated {
		t.Fatalf("a flagless verb resolved %q, want the posture the sandbox was started with",
			next.Network)
	}
	next.Runtime = &postureRuntime{livenessRuntime: &livenessRuntime{}, booted: "isolated"}
	if next.networkStale() {
		t.Error("a flagless verb read the isolated sandbox as stale, so it would restart it")
	}
}

// Offline is kept the same way. A bare `brig run` on a stopped offline sandbox
// boots it offline again rather than giving it a route out.
func TestAnOfflineSandboxStaysOffline(t *testing.T) {
	isolateState(t)
	home := t.TempDir()

	mustLoad(t, Options{Workspace: home, Network: "offline"}).rememberSession()

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

	mustLoad(t, Options{Workspace: home, Network: "isolated"}).rememberSession()

	byFlag := mustLoad(t, Options{Network: "shared"})
	if byFlag.Network != NetShared {
		t.Fatalf("--network shared resolved %q", byFlag.Network)
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
	started.rememberSession()

	next, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Network != NetIsolated {
		t.Errorf("the profile's network: moved a recorded sandbox to %q, want isolated", next.Network)
	}
}

// A session recorded before the index held a posture resolves it the way it
// always did, and the restart warning keeps its general wording because there
// is no recorded posture to name.
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
	if got := c.networkChange(); !strings.Contains(got, "different network policy") {
		t.Errorf("an old entry changed the restart warning: %s", got)
	}
}

// A posture recorded for a differently named sandbox is not this one's, the
// same rule the home and the project follow.
func TestAnotherSandboxesPostureIsNotInherited(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: t.TempDir(), Sandbox: "brig-elsewhere", Network: "isolated"},
	}); err != nil {
		t.Fatal(err)
	}

	if got := mustLoad(t, Options{}).Network; got != NetShared {
		t.Errorf("inherited %q from another sandbox", got)
	}
}

// A recorded value that is not a posture is brig's own bookkeeping gone bad,
// not something anybody typed, so it is ignored rather than refusing every
// command on the session.
func TestARecordedPostureThatIsNotOneIsIgnored(t *testing.T) {
	isolateState(t)
	if err := writeSessionIndex(map[string]sessionEntry{
		"claude-code": {Home: t.TempDir(), Sandbox: "brig-claude-code", Network: "bogus"},
	}); err != nil {
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
// not a posture anybody asked for. The index records what was asked, so the
// sandbox goes back to shared once the policy is detached.
func TestAPolicyIsNotRecordedAsThePosture(t *testing.T) {
	dir := isolateState(t)
	policies := t.TempDir()
	writeTestPolicy(t, policies, "no-net")
	t.Setenv("BRIG_POLICY_DIR", policies)

	c := loadWithPolicy(t, "no-net")
	if c.Network != NetIsolated {
		t.Fatalf("the policy did not isolate the sandbox: %q", c.Network)
	}
	c.rememberSession()
	if blob := mustReadIndex(t, dir); !strings.Contains(blob, `"network": "shared"`) {
		t.Errorf("the index recorded the policy's posture rather than the one asked for:\n%s", blob)
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
}
