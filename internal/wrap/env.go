package wrap

import (
	"os"
	"strings"
)

// Env is the settings lookup, resolved in one place so every knob follows the
// same rule.
//
// A key is looked up per-agent first, then globally, then under the name the
// Homebrew wrapper used. Per-agent wins so one shell can carry different
// settings for two agents:
//
//	BRIG_CLAUDE_CODE_WORKSPACE  this agent only
//	BRIG_WORKSPACE              every agent
//	URUNC_CLAUDE_WORKSPACE      what urunc-claude called it
//
// The legacy names are read, never written: an existing setup keeps working
// after the cutover without anyone editing a shell profile.
type Env struct {
	get    func(string) (string, bool)
	prefix string   // BRIG_CLAUDE_CODE
	legacy []string // URUNC_CLAUDE
}

// NewEnv builds the lookup for a template.
func NewEnv(templateName string, get func(string) (string, bool)) Env {
	if get == nil {
		get = os.LookupEnv
	}
	upper := strings.ToUpper(strings.ReplaceAll(templateName, "-", "_"))
	e := Env{get: get, prefix: "BRIG_" + upper}
	switch templateName {
	case "claude-code":
		e.legacy = []string{"URUNC_CLAUDE"}
	case "claude-desktop":
		e.legacy = []string{"URUNC_CLAUDE_DESKTOP"}
	case "ubuntu":
		e.legacy = []string{"URUNC_UBUNTU"}
	}
	return e
}

// Get returns the first setting present under any of the prefixes.
func (e Env) Get(key string) (string, bool) {
	names := append([]string{e.prefix + "_" + key, "BRIG_" + key}, e.legacyNames(key)...)
	for _, n := range names {
		if v, ok := e.get(n); ok {
			return v, true
		}
	}
	return "", false
}

func (e Env) legacyNames(key string) []string {
	out := make([]string, 0, len(e.legacy))
	for _, p := range e.legacy {
		out = append(out, p+"_"+key)
	}
	return out
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
