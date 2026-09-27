package wrap

import (
	"os"
	"testing"

	"github.com/brig-sh/brig/internal/verify"
)

// clearVerifyEnv unsets every variable HostVerifyPolicy reads for the agents
// here, so a developer shell that sets one cannot decide these tests. Unset
// and not empty: a per-agent variable set to "" is found first, and its empty
// value then hides the global one a case sets.
func clearVerifyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VERIFY_REGISTRY", "VERIFY_IDENTITY", "VERIFY_ISSUER", "COSIGN_BIN"} {
		for _, name := range []string{"BRIG_" + k, "BRIG_CLAUDE_CODE_" + k, "BRIG_CODEX_" + k, "BRIG__" + k} {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

// The per-agent identity reaches the policy for that agent and for no other.
func TestHostVerifyPolicyPerAgentWins(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("BRIG_VERIFY_IDENTITY", "^global$")
	t.Setenv("BRIG_CLAUDE_CODE_VERIFY_IDENTITY", "^agent$")

	if got := HostVerifyPolicy("claude-code").Identity; got != "^agent$" {
		t.Errorf("claude-code identity is %q, want the per-agent ^agent$", got)
	}
	if got := HostVerifyPolicy("codex").Identity; got != "^global$" {
		t.Errorf("codex identity is %q, want the global ^global$", got)
	}
	if got := HostVerifyPolicy("").Identity; got != "^global$" {
		t.Errorf("no-agent identity is %q, want the global ^global$", got)
	}
}

// With no agent, the lookup does not invent a BRIG__ prefix and read it.
func TestHostVerifyPolicyNoAgentReadsGlobalOnly(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("BRIG__VERIFY_IDENTITY", "^planted$")

	p := HostVerifyPolicy("")
	if p.Identity != verify.DefaultPolicy().Identity {
		t.Errorf("no-agent identity is %q, want the shipped one", p.Identity)
	}
	if p.Replaced() {
		t.Errorf("no-agent policy reads as replaced with nothing global set: %+v", p)
	}
}
