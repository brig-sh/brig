package wrap

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

type networkDefaultRuntime struct {
	*livenessRuntime
	kind string
}

func (r *networkDefaultRuntime) Kind() string { return r.kind }

// Exercise resolution through the boot request: a parser-only test would miss
// a profile or run path quietly putting a fresh sandbox back on shared.
func TestANewSandboxBootsOnItsOwnNetworkByDefault(t *testing.T) {
	for _, kind := range []string{"hull", "nerdctl"} {
		t.Run(kind, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_POLICY_DIR", t.TempDir())
			t.Setenv("BRIG_HYPERVISOR", "")
			home := t.TempDir()
			rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{workspace: home}, kind: kind}
			p := testProfile(t, "hypervisor: hvi\n")
			c, err := Load(p, Options{Workspace: home}, rt)
			if err != nil {
				t.Fatal(err)
			}
			c.Verify, c.MacOSVersion = verify.Off, func() string { return "" }
			c.Out, c.Err, c.Progress = io.Discard, io.Discard, io.Discard
			if err := c.EnsureRunning(creds.Set{}); err != nil {
				t.Fatal(err)
			}
			if rt.spec.Net != "isolated" {
				t.Errorf("fresh sandbox booted on %q, want its own network", rt.spec.Net)
			}
			if got := mustBootedNet(t, c.VMName); got != "isolated" {
				t.Errorf("boot recorded %q, want isolated", got)
			}
		})
	}
}

func TestTheVZDefaultIsSharedAndExplained(t *testing.T) {
	for _, hv := range []string{"", "vz"} {
		t.Run("hypervisor="+hv, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_HYPERVISOR", hv)
			rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{}, kind: "hull"}
			c, err := Load(testProfile(t, ""), Options{Workspace: t.TempDir()}, rt)
			if err != nil {
				t.Fatal(err)
			}
			if c.Network != NetShared || !strings.Contains(c.networkLine(), "vz does not support isolated networking") {
				t.Errorf("vz fallback must name its limitation: %s", c.networkLine())
			}
		})
	}
}

func TestLinuxDoesNotInheritTheVZNetworkFallback(t *testing.T) {
	for _, hv := range []string{"vz", "qemu"} {
		t.Run(hv, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_HYPERVISOR", hv)
			rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{}, kind: "nerdctl"}
			c, err := Load(testProfile(t, ""), Options{Workspace: t.TempDir()}, rt)
			if err != nil {
				t.Fatal(err)
			}
			if c.Network != NetIsolated {
				t.Errorf("Linux resolved %q from a macOS-only hypervisor setting", c.Network)
			}
		})
	}
}

func TestQEMUOnlyFallsBackWhenNoPostureWasNamed(t *testing.T) {
	for _, posture := range []string{"", "shared", "isolated", "offline"} {
		t.Run("posture="+posture, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_HYPERVISOR", "qemu")
			p := testProfile(t, "")
			p.Network = posture
			rt := &networkDefaultRuntime{livenessRuntime: &livenessRuntime{}, kind: "hull"}
			c, err := Load(p, Options{Workspace: t.TempDir()}, rt)
			if err != nil {
				t.Fatal(err)
			}
			want := Network(posture)
			if posture == "" {
				want = NetShared
				if !strings.Contains(c.networkLine(), "qemu does not support isolated networking") {
					t.Errorf("qemu fallback was not explained: %s", c.networkLine())
				}
			}
			if c.Network != want {
				t.Errorf("qemu resolved %q as %q, want %q", posture, c.Network, want)
			}
		})
	}
}

// The new default must never migrate a recorded shared sandbox: a flagless
// shell after an upgrade is not permission to restart it and lose guest state.
func TestARecordedSharedSandboxKeepsItsNetworkAfterTheDefaultChanges(t *testing.T) {
	isolateState(t)
	p := testProfile(t, "hypervisor: hvi\nnetwork: shared\n")
	before, err := Load(p, Options{Workspace: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	booted(before)
	p.Network = "isolated"
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := runningAs("shared")
	live.workspace = c.Workspace
	c.Runtime = live
	if err := c.PrepareWorkspace(); err != nil {
		t.Fatal(err)
	}
	if c.Network != NetShared || c.postureChanged() || c.networkStale() {
		t.Fatalf("recorded shared sandbox would change network: %+v", c.Network)
	}
	if err := c.EnsureRunning(creds.Set{}); err != nil {
		t.Fatal(err)
	}
	rt := c.Runtime.(*postureRuntime)
	if rt.stops != 0 || rt.boots != 0 {
		t.Errorf("upgrade caused %d stops and %d boots", rt.stops, rt.boots)
	}
}

func TestVZRefusesExplicitIsolationFromEverySource(t *testing.T) {
	for _, source := range []string{"flag", "setting", "profile", "recorded"} {
		t.Run(source, func(t *testing.T) {
			isolateState(t)
			t.Setenv("BRIG_RUNTIME", "hull")
			t.Setenv("BRIG_HYPERVISOR", "vz")
			bin := filepath.Join(t.TempDir(), "hull")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BRIG_RUNTIME_BIN", bin)
			rt, err := runtime.Detect()
			if err != nil {
				t.Fatal(err)
			}
			p := testProfile(t, "")
			o := Options{Workspace: t.TempDir()}
			switch source {
			case "flag":
				o.Network = "isolated"
			case "setting":
				t.Setenv("BRIG_NETWORK", "isolated")
			case "profile":
				p.Network = "isolated"
			case "recorded":
				prior, err := Load(p, Options{Workspace: o.Workspace, Network: "isolated"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				booted(prior)
			}
			c, err := Load(p, o, rt)
			if err != nil {
				t.Fatal(err)
			}
			if c.Network != NetIsolated {
				t.Fatalf("explicit isolated from %s silently became %s", source, c.Network)
			}
			if err := c.checkBackend(c.hypervisor()); err == nil || !strings.Contains(err.Error(), "isolated") {
				t.Errorf("vz did not refuse explicit isolation: %v", err)
			}
		})
	}
}

// The profile's network: is not a flag. BRIG_HYPERVISOR=vz, which is the
// macOS 14 advice, must say so and name the shared-network way through.
// brig info reports the same sentence, because it otherwise shows isolated
// for a run checkBackend will refuse.
func TestProfileIsolationOnVZNamesTheProfileNotTheFlag(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_HYPERVISOR", "vz")
	bin := filepath.Join(t.TempDir(), "hull")
	// Absent, not a failed inspect. A failing stub leaves the posture unknown,
	// and info then has no isolated row to attach the fix to.
	script := "#!/bin/sh\necho \"instance not found: $2\" >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	p := testProfile(t, "network: isolated\n")
	c, err := Load(p, Options{Workspace: t.TempDir()}, rt)
	if err != nil {
		t.Fatal(err)
	}
	err = c.checkBackend(c.hypervisor())
	if err == nil {
		t.Fatal("vz accepted the profile's isolated network")
	}
	got := err.Error()
	for _, want := range []string{
		"the x profile sets network: isolated",
		"the vz backend cannot",
		`BRIG_HYPERVISOR is "vz"`,
		"--network shared",
		"BRIG_NETWORK=shared",
		"run it on hvi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--network isolated gives") {
		t.Errorf("refusal blames a flag the user did not type:\n%s", got)
	}
	if other := c.profileIsolationOnFallback("hvi"); other != "" {
		t.Errorf("hvi was given the vz refusal: %s", other)
	}
	row := ""
	for _, e := range c.envelope(creds.Set{}) {
		if e.label == "NETWORK" {
			row = e.value
		}
	}
	if row != "isolated (a network of this sandbox's own); "+got {
		t.Errorf("NETWORK row = %q", row)
	}
	if json := c.InfoData(creds.Set{}).Network; json != row {
		t.Errorf("JSON network = %q, want %q", json, row)
	}

	explicit, err := Load(p, Options{Workspace: t.TempDir(), Network: "isolated"}, rt)
	if err != nil {
		t.Fatal(err)
	}
	err = explicit.checkBackend(explicit.hypervisor())
	if err == nil || !strings.Contains(err.Error(), "--network isolated gives") {
		t.Fatalf("an explicit --network isolated lost the flag refusal: %v", err)
	}
}

// networkSource is message text. Rewording where Load writes it must not
// turn the profile refusal off, and no wording may turn it on for a posture
// the setting or a recorded posture chose over the profile's network:. Both
// of those profiles name a network, so a Load that credits the profile with
// the posture turns the refusal on. The flag has its own test above.
func TestProfileIsolationRefusalDoesNotReadTheSourceWording(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_HYPERVISOR", "vz")
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\necho \"instance not found: $2\" >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	rt, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}

	fromProfile, err := Load(testProfile(t, "network: isolated\n"), Options{Workspace: t.TempDir()}, rt)
	if err != nil {
		t.Fatal(err)
	}
	fromProfile.networkSource = "reworded"
	if fromProfile.profileIsolationOnFallback("vz") == "" {
		t.Error("rewording the profile source turned its refusal off")
	}

	t.Setenv("BRIG_NETWORK", "isolated")
	fromSetting, err := Load(testProfile(t, "network: isolated\n"), Options{Workspace: t.TempDir()}, rt)
	if err != nil {
		t.Fatal(err)
	}
	fromSetting.networkSource = "the profile's network:"
	if msg := fromSetting.profileIsolationOnFallback("vz"); msg != "" {
		t.Errorf("BRIG_NETWORK=isolated got the profile refusal: %s", msg)
	}

	// This profile says shared, so the profile refusal here would claim an
	// isolation the profile never asked for.
	t.Setenv("BRIG_NETWORK", "")
	shared, ws := testProfile(t, "network: shared\n"), t.TempDir()
	started, err := Load(shared, Options{Workspace: ws, Network: "isolated"}, rt)
	if err != nil {
		t.Fatal(err)
	}
	booted(started)
	fromRecord, err := Load(shared, Options{Workspace: ws}, rt)
	if err != nil {
		t.Fatal(err)
	}
	if fromRecord.askedNetwork != NetIsolated {
		t.Fatalf("the recorded posture did not win over the profile: %q", fromRecord.askedNetwork)
	}
	fromRecord.networkSource = "the profile's network:"
	if msg := fromRecord.profileIsolationOnFallback("vz"); msg != "" {
		t.Errorf("a recorded isolated posture got the profile refusal: %s", msg)
	}
}

// A sandbox an older release started has no posture record, and the runtime
// reports the network it booted with. That posture outranks the profile's
// network: the way a record does, so it must not get the profile refusal
// either: this profile says shared, and the refusal would call it isolated.
func TestARecoveredPostureIsNotCreditedToTheProfile(t *testing.T) {
	isolateState(t)
	t.Setenv("BRIG_POLICY_DIR", t.TempDir())
	t.Setenv("BRIG_HYPERVISOR", "vz")
	home := t.TempDir()
	if err := writeSessionIndex(map[string]sessionEntry{
		sessionKey("x", ""): {Home: home, Sandbox: NamePrefix + "x"},
	}); err != nil {
		t.Fatal(err)
	}
	rt := &legacyNetworkRuntime{livenessRuntime: &livenessRuntime{}, kind: "hull", network: "isolated"}
	c, err := Load(testProfile(t, "network: shared\n"), Options{Workspace: home}, rt)
	if err != nil {
		t.Fatal(err)
	}
	if c.askedNetwork != NetIsolated || c.netRecovery != networkRecovered {
		t.Fatalf("the recovered posture did not win over the profile: asked %q, recovery %v", c.askedNetwork, c.netRecovery)
	}
	c.networkSource = "the profile's network:"
	if msg := c.profileIsolationOnFallback("vz"); msg != "" {
		t.Errorf("a recovered isolated posture got the profile refusal: %s", msg)
	}
}

var _ runtime.Runtime = (*networkDefaultRuntime)(nil)
