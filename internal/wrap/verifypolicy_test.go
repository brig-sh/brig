package wrap

import (
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/verify"
)

// clearVerifyEnv unsets every variable HostVerifyPolicy reads for the agents
// here, so a developer shell that sets one cannot decide these tests. Unset
// and not empty: a per-agent variable set to "" is found first, and its empty
// value then hides the global one a case sets.
func clearVerifyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VERIFY", "VERIFY_REGISTRY", "VERIFY_IDENTITY", "VERIFY_ISSUER", "COSIGN_BIN"} {
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

// Doctor and run agree only while HostVerifyPolicy and HostVerifyMode read the
// environment the way Load does. Each builds its own lookup, so a change to
// one that misses the other puts doctor back to reporting on a policy the run
// does not use. This pins them to Load itself, with a per-agent and a global
// setting in play.
func TestHostVerifyMatchesLoad(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("BRIG_CLAUDE_CODE_VERIFY_IDENTITY", "^agent$")
	t.Setenv("BRIG_VERIFY_ISSUER", "https://issuer.example")
	t.Setenv("BRIG_VERIFY_REGISTRY", "ghcr.io/me/")
	t.Setenv("BRIG_CLAUDE_CODE_VERIFY", "require")
	t.Setenv("BRIG_VERIFY", "off")

	p, ok := profile.Lookup("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	c, err := Load(p, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := HostVerifyPolicy(p.Name); got != c.VerifyPolicy {
		t.Errorf("HostVerifyPolicy is %+v, Load resolved %+v", got, c.VerifyPolicy)
	}
	mode, from, err := HostVerifyMode(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if mode != c.Verify {
		t.Errorf("HostVerifyMode is %v, Load resolved %v", mode, c.Verify)
	}
	if from != "BRIG_CLAUDE_CODE_VERIFY" {
		t.Errorf("mode came from %q, want the per-agent BRIG_CLAUDE_CODE_VERIFY", from)
	}
}

// A bad per-agent mode is named by the variable the user wrote. Quoting
// BRIG_VERIFY sends them to a setting that is not the one at fault.
func TestHostVerifyModeNamesPerAgentVariable(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("BRIG_VERIFY", "warn")
	t.Setenv("BRIG_CODEX_VERIFY", "requrie")

	_, from, err := HostVerifyMode("codex")
	if err == nil {
		t.Fatal("HostVerifyMode accepted requrie")
	}
	if from != "BRIG_CODEX_VERIFY" || !strings.Contains(err.Error(), "BRIG_CODEX_VERIFY=") {
		t.Errorf("error %q (from %q) does not name BRIG_CODEX_VERIFY", err, from)
	}
	if mode, from, err := HostVerifyMode(""); err != nil || mode != verify.Warn || from != "BRIG_VERIFY" {
		t.Errorf("no-agent mode is %v from %q (err %v), want warn from BRIG_VERIFY", mode, from, err)
	}
}
