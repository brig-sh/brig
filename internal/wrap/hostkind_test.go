package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/verify"
)

// hostKindRuntime answers what the envelope and the verify step ask of the
// host kind. Everything else panics through the embedded nil interface.
type hostKindRuntime struct{ runtime.Runtime }

func (hostKindRuntime) Kind() string { return runtime.HostKind }
func (hostKindRuntime) Isolation(string) runtime.Isolation {
	return runtime.Isolation{Boundary: runtime.BoundaryProcess, Detail: "shared kernel"}
}

// The host kind boots no image, so the rows must not name one as if it ran,
// and isolated must not read as a network of the sandbox's own.
func TestEnvelopeOnTheHostKind(t *testing.T) {
	c := envelopeConfig()
	c.Runtime = hostKindRuntime{}
	c.Profile.Binary = "claude"
	c.Network = NetIsolated
	block := &bytes.Buffer{}
	c.renderEnvelope(block, creds.Set{})
	got := block.String()
	for _, want := range []string{
		"SANDBOX      brig-claude-code-review (host)",
		"ISOLATION    process (shared kernel)",
		"IMAGE        none: the host kind runs claude from PATH",
		"VERIFY       nothing to check: the host kind boots no image",
		"NETWORK      isolated (process sandbox: no IP egress but loopback",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, c.Image) || strings.Contains(got, "a network of this sandbox's own") {
		t.Errorf("the block claims an image or a network the host kind does not have:\n%s", got)
	}
}

// No signature check runs for an image that never boots.
func TestVerifyImageSkipsTheHostKind(t *testing.T) {
	old := checkTag
	checkTag = func(verify.Policy, string) verify.Result {
		t.Fatal("the host kind checked an image it does not boot")
		return verify.Result{}
	}
	t.Cleanup(func() { checkTag = old })
	c := envelopeConfig()
	c.Runtime = hostKindRuntime{}
	c.Verify = verify.Require
	c.Profile.GenericBoot = true
	if err := c.verifyImage(); err != nil {
		t.Fatal(err)
	}
	if err := c.verifyBootAssets(); err != nil {
		t.Fatal(err)
	}
}

// The host kind refuses a profile that needs a tmpfs, so the check before
// the workspace is prepared has to carry it. hull mounts its own.
func TestBackendSpecCarriesTheTmpfsToTheHostKind(t *testing.T) {
	c := envelopeConfig()
	c.Profile.GuestHome = "/root"
	c.Profile.Volumes = []profile.Volume{{Kind: profile.VolumeTmpfs, Path: ".claude"}}
	if got := c.backendSpec("").Tmpfs; len(got) != 0 {
		t.Errorf("hull's check carries a tmpfs: %q", got)
	}
	c.Runtime = hostKindRuntime{}
	if got := c.backendSpec("").Tmpfs; len(got) != 1 || !strings.HasPrefix(got[0], "/root/.claude:") {
		t.Errorf("the host kind's check carries %q, want the .claude tmpfs", got)
	}
}

// A policy whose default is allow still runs isolated, which the process
// sandbox holds to loopback. The row says so.
func TestPolicyRowOnTheHostKindSaysWhatHolds(t *testing.T) {
	c := envelopeConfig()
	c.Runtime = hostKindRuntime{}
	c.Network = NetIsolated
	c.Policies = []string{"open"}
	c.Egress.Default = "allow"
	if got := c.policyLine(); !strings.Contains(got, "only loopback is reachable") {
		t.Errorf("the POLICY row is %q", got)
	}
	c.Runtime = fakeRuntime{}
	if got := c.policyLine(); strings.Contains(got, "loopback") {
		t.Errorf("a microVM's POLICY row names the process sandbox: %q", got)
	}
}
