package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

func denyPolicy() policy.Egress {
	return policy.Egress{
		Default: "deny",
		Allow:   []policy.Rule{{Host: "api.anthropic.com"}, {CIDR: "10.0.0.0/8"}},
		Deny:    []policy.Rule{{Host: "metadata.google.internal"}},
	}
}

// The envelope is where a reader is told what the sandbox they are about to
// trust may reach. A run under a policy that said nothing about it would leave
// the only report of the boundary silent on the half this feature moves.
func TestEnvelopeNamesThePolicies(t *testing.T) {
	c := envelopeConfig()
	c.Policies = []string{"no-net", "corp"}
	c.Egress = denyPolicy()

	var out bytes.Buffer
	c.Err = &out
	c.PrintPreRunEnvelope(creds.Set{})

	got := out.String()
	if !strings.Contains(got, "POLICY") {
		t.Fatalf("no POLICY row in the envelope:\n%s", got)
	}
	for _, want := range []string{"no-net", "corp", "deny"} {
		if !strings.Contains(got, want) {
			t.Errorf("the POLICY row does not name %q:\n%s", want, got)
		}
	}
}

// A row reading "none" on every run trains the eye to skip the line that
// matters on the runs where there is one.
func TestEnvelopeOmitsThePolicyRowWhenNoneApplies(t *testing.T) {
	c := envelopeConfig()
	var out bytes.Buffer
	c.Err = &out
	c.PrintPreRunEnvelope(creds.Set{})

	if strings.Contains(out.String(), "POLICY") {
		t.Errorf("a run with no policy printed a POLICY row:\n%s", out.String())
	}
}

// The rules live on the gateway serving the sandbox and cover every member of
// its network, so a sandbox answering to rules of its own cannot be sharing
// one. The posture follows the policy, and the envelope must say the posture
// the sandbox actually gets rather than the one that was asked for.
func TestAPolicyTakesTheNetworkPostureWithIt(t *testing.T) {
	dir := t.TempDir()
	writeTestPolicy(t, dir, "no-net")
	t.Setenv("BRIG_POLICY_DIR", dir)

	c := loadWithPolicy(t, "no-net")
	if c.Network != NetIsolated {
		t.Errorf("network = %q, want %q: a policy needs a network of its own",
			c.Network, NetIsolated)
	}
	if !strings.Contains(c.Network.Line(), "isolated") {
		t.Errorf("the envelope would report %q", c.Network.Line())
	}
}

// Offline is stricter than any rule set, so a policy must not quietly promote
// it to isolated -- that would give the sandbox a way out it was told not to
// have.
func TestAPolicyDoesNotPromoteOfflineToIsolated(t *testing.T) {
	dir := t.TempDir()
	writeTestPolicy(t, dir, "no-net")
	t.Setenv("BRIG_POLICY_DIR", dir)
	t.Setenv("BRIG_NETWORK", "offline")

	c := loadWithPolicy(t, "no-net")
	if c.Network != NetOffline {
		t.Errorf("network = %q, want %q: offline outranks a policy", c.Network, NetOffline)
	}
}

// The rules have to survive the trip from the document to the request the
// runtime is handed. A rule dropped here is a rule nothing enforces, and
// nothing downstream could notice.
func TestRuntimeEgressCarriesEveryRule(t *testing.T) {
	got := runtimeEgress(denyPolicy())

	if got.Default != "deny" {
		t.Errorf("default = %q, want deny", got.Default)
	}
	if len(got.Allow) != 2 || len(got.Deny) != 1 {
		t.Fatalf("rules were lost in translation: %+v", got)
	}
	if got.Allow[0].Host != "api.anthropic.com" || got.Allow[1].CIDR != "10.0.0.0/8" {
		t.Errorf("allow rules did not survive: %+v", got.Allow)
	}
	if got.Deny[0].Host != "metadata.google.internal" {
		t.Errorf("deny rules did not survive: %+v", got.Deny)
	}
}

// No policy must reach the runtime as no filtering rather than as an empty
// rule set: --egress-default is what turns filtering on, and an empty one
// under "deny" is a sandbox that reaches nothing at all.
func TestRuntimeEgressOfNoPolicyIsUnfiltered(t *testing.T) {
	if got := runtimeEgress(policy.Egress{}); got.Filtered() {
		t.Errorf("a run with no policy asked for filtering: %+v", got)
	}
}

// A runtime that cannot say whether its network is stale is treated as
// current: a restart nobody needed costs a boot, and this is not the check
// that should spend one on a guess.
func TestNetworkStaleIsFalseForARuntimeThatCannotAnswer(t *testing.T) {
	c := envelopeConfig()
	c.Egress = denyPolicy()
	if c.networkStale() {
		t.Error("a runtime that does not implement the check reported stale")
	}
}

// And it asks the runtime that can, passing what that runtime needs to answer.
func TestNetworkStaleAsksTheRuntime(t *testing.T) {
	asked := &recordingChecker{stale: true}
	c := envelopeConfig()
	c.Runtime = asked
	c.Network = NetIsolated
	c.Egress = denyPolicy()

	if !c.networkStale() {
		t.Error("the runtime said stale and the answer was dropped")
	}
	if asked.name != c.VMName {
		t.Errorf("asked about %q, want %q", asked.name, c.VMName)
	}
	if asked.net != "isolated" {
		t.Errorf("asked about net %q, want isolated", asked.net)
	}
	if !asked.egress.Filtered() || len(asked.egress.Allow) != 2 {
		t.Errorf("the rules did not reach the runtime: %+v", asked.egress)
	}
}

// The refusal has to reach the path that boots nothing. A sandbox already
// running is joined rather than booted, so a check living only inside Run let
// exactly the runs it exists to stop through: refused on the first `brig run`,
// waved past on the second, printing a POLICY row over a sandbox filtering
// nothing.
func TestCheckBackendRefusesOnThePathThatBootsNothing(t *testing.T) {
	refusing := &refusingChecker{err: errors.New("this backend cannot enforce a policy")}
	c := envelopeConfig()
	c.Runtime = refusing
	c.Egress = denyPolicy()

	if err := c.checkBackend("vz"); err == nil {
		t.Fatal("a backend that refuses the run was not consulted, or its answer was dropped")
	}
	if !refusing.asked.Egress.Filtered() {
		t.Errorf("the rules did not reach the backend: %+v", refusing.asked)
	}
	if refusing.asked.Hypervisor != "vz" {
		t.Errorf("asked about hypervisor %q, want vz", refusing.asked.Hypervisor)
	}
}

// The spec that is checked and the spec that boots come from one derivation,
// so the two cannot answer differently about the same run.
func TestBackendSpecCarriesWhatABackendCanRefuse(t *testing.T) {
	c := envelopeConfig()
	c.Network = NetIsolated
	c.Egress = denyPolicy()

	got := c.backendSpec("hvi")
	if got.Name != c.VMName || got.Hypervisor != "hvi" || got.Net != "isolated" {
		t.Errorf("the decision fields are wrong: %+v", got)
	}
	if !got.Egress.Filtered() || len(got.Egress.Allow) != 2 {
		t.Errorf("the rules did not reach the spec: %+v", got.Egress)
	}
}

// A runtime with no opinion is not asked, and nothing is refused on its behalf.
func TestCheckBackendPassesARuntimeThatCannotAnswer(t *testing.T) {
	c := envelopeConfig()
	c.Egress = denyPolicy()
	if err := c.checkBackend("vz"); err != nil {
		t.Errorf("a runtime that does not implement the check refused the run: %v", err)
	}
}

type refusingChecker struct {
	fakeRuntime
	err   error
	asked runtime.RunSpec
}

func (r *refusingChecker) CanRun(spec runtime.RunSpec) error {
	r.asked = spec
	return r.err
}

type recordingChecker struct {
	fakeRuntime
	stale  bool
	name   string
	net    string
	egress runtime.Egress
}

func (r *recordingChecker) NetworkStale(name, hypervisor, net string, e runtime.Egress) bool {
	r.name, r.net, r.egress = name, net, e
	return r.stale
}

// loadWithPolicy resolves a run of a profile that declares one policy inline,
// which is one of the three ways a policy binds and the one a test can set up
// without writing an attachments file.
func loadWithPolicy(t *testing.T, name string) *Config {
	t.Helper()
	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	p.Policy = []string{name}
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeTestPolicy(t *testing.T, dir, name string) {
	t.Helper()
	doc := "apiVersion: " + policy.APIVersion + "\nname: " + name +
		"\negress:\n  default: deny\n  allow:\n    - host: api.anthropic.com\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}
