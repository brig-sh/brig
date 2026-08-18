package wrap

import (
	"os"
	"strings"
)

// Env is the settings lookup, resolved in one place so every knob follows the
// same rule.
//
// A key is looked up per-agent first, then globally. Per-agent wins so one
// shell can carry different settings for two agents:
//
//	BRIG_CLAUDE_CODE_WORKSPACE  this agent only
//	BRIG_WORKSPACE              every agent
type Env struct {
	get    func(string) (string, bool)
	prefix string // BRIG_CLAUDE_CODE
}

// NewEnv builds the lookup for a profile.
func NewEnv(profileName string, get func(string) (string, bool)) Env {
	if get == nil {
		get = os.LookupEnv
	}
	upper := strings.ToUpper(strings.ReplaceAll(profileName, "-", "_"))
	return Env{get: get, prefix: "BRIG_" + upper}
}

// Get returns the first setting present under any of the prefixes.
func (e Env) Get(key string) (string, bool) {
	for _, n := range []string{e.prefix + "_" + key, "BRIG_" + key} {
		if v, ok := e.get(n); ok {
			return v, true
		}
	}
	return "", false
}

// String returns the setting or a fallback.
func (e Env) String(key, fallback string) string {
	if v, ok := e.Get(key); ok && v != "" {
		return v
	}
	return fallback
}

// Bool reads a setting as an on/off switch. Anything but "0" is on, matching
// the shell idiom the wrapper used, so URUNC_CLAUDE_GIT_CONFIG=1 and =true
// both enable it and =0 is the only way off.
func (e Env) Bool(key string, fallback bool) bool {
	v, ok := e.Get(key)
	if !ok || v == "" {
		return fallback
	}
	return v != "0"
}

// Int reads a setting as a number, falling back on anything unparseable
// rather than failing the run over a typo in a tuning knob.
func (e Env) Int(key string, fallback int) int {
	v, ok := e.Get(key)
	if !ok || v == "" {
		return fallback
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return fallback
	}
	return n
}

// Fields reads a whitespace-separated list.
func (e Env) Fields(key string, fallback []string) []string {
	v, ok := e.Get(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.Fields(v)
}
