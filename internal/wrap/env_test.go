package wrap

import "testing"

func TestEnvPrecedence(t *testing.T) {
	env := map[string]string{
		"BRIG_CLAUDE_CODE_WORKSPACE": "per-agent",
		"BRIG_WORKSPACE":             "global",
		"URUNC_CLAUDE_WORKSPACE":     "legacy",
	}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	e := NewEnv("claude-code", get)

	if got := e.String("WORKSPACE", "fallback"); got != "per-agent" {
		t.Errorf("per-agent setting did not win: %q", got)
	}
	delete(env, "BRIG_CLAUDE_CODE_WORKSPACE")
	if got := e.String("WORKSPACE", "fallback"); got != "global" {
		t.Errorf("global setting did not win over legacy: %q", got)
	}
	// The name the Homebrew wrapper used still works, so an existing shell
	// profile survives the cutover.
	delete(env, "BRIG_WORKSPACE")
	if got := e.String("WORKSPACE", "fallback"); got != "legacy" {
		t.Errorf("legacy setting was ignored: %q", got)
	}
	delete(env, "URUNC_CLAUDE_WORKSPACE")
	if got := e.String("WORKSPACE", "fallback"); got != "fallback" {
		t.Errorf("fallback = %q", got)
	}
}

// An agent with no Homebrew ancestor reads no legacy names, so one agent's
// setting cannot leak into another's.
func TestEnvLegacyIsPerAgent(t *testing.T) {
	env := map[string]string{"URUNC_CLAUDE_WORKSPACE": "legacy"}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if got := NewEnv("codex", get).String("WORKSPACE", "fallback"); got != "fallback" {
		t.Errorf("codex picked up a claude setting: %q", got)
	}
}

func TestEnvBoolAndFields(t *testing.T) {
	env := map[string]string{
		"BRIG_GIT_CONFIG":  "1",
		"BRIG_TRUST":       "0",
		"BRIG_FORWARD_ENV": " A  B\tC ",
		"BRIG_EMPTY":       "",
	}
	e := NewEnv("codex", func(k string) (string, bool) { v, ok := env[k]; return v, ok })

	if !e.Bool("GIT_CONFIG", false) {
		t.Error("1 did not read as on")
	}
	if e.Bool("TRUST", true) {
		t.Error("0 did not read as off")
	}
	if !e.Bool("MISSING", true) {
		t.Error("an absent setting did not fall back")
	}
	if !e.Bool("EMPTY", true) {
		t.Error("an empty setting did not fall back")
	}
	got := e.Fields("FORWARD_ENV", nil)
	if len(got) != 3 || got[0] != "A" || got[2] != "C" {
		t.Errorf("Fields = %v", got)
	}
	if n := e.Int("MEM", 4096); n != 4096 {
		t.Errorf("Int fallback = %d", n)
	}
}
