package wrap

import (
	"fmt"
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
	_, v, ok := e.getNamed(key)
	return v, ok
}

// getNamed is Get plus the exact variable name the value came from, so an
// error or a report can quote the setting the user actually wrote -- the
// per-agent BRIG_<AGENT>_<KEY> or the global BRIG_<KEY> -- rather than a
// canonical spelling they never typed. BRIG_ALLOW_DENIED=1 in a message the
// user reads after setting BRIG_CLAUDE_CODE_ALLOW_DENIED=false is two lies in
// one line: wrong name and wrong value.
func (e Env) getNamed(key string) (name, value string, ok bool) {
	for _, n := range []string{e.prefix + "_" + key, "BRIG_" + key} {
		if v, ok := e.get(n); ok {
			return n, v, true
		}
	}
	return "", "", false
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
//
// This is the reading for a tuning knob, where on is the ordinary case and a
// value the helper does not recognise is closer to on than to a failed run.
// StrictBool is the reading for a switch whose off position is the safe one.
func (e Env) Bool(key string, fallback bool) bool {
	v, ok := e.Get(key)
	if !ok || v == "" {
		return fallback
	}
	return v != "0"
}

// StrictBool reads a security switch, where the safe position is off and a
// value the helper cannot read must stop the run rather than be guessed.
//
// Bool treats anything but "0" as on. That is right for a tuning knob and
// wrong for a switch that forwards a denied credential or writes brig's own
// files into the workspace: under that rule BRIG_ALLOW_DENIED=false reads as
// on, so a user spelling "off" the way their shell does turns the guard off by
// turning it on -- the switch fails open for exactly the person trying to close
// it. Here both spellings a user reaches for are understood, case-insensitively,
// and anything outside either set is refused by name instead of falling open.
//
// Absent or empty still falls back, so the switch keeps its default until the
// user says otherwise -- the same meaning Bool gives them.
func (e Env) StrictBool(key string, fallback bool) (bool, error) {
	name, v, ok := e.getNamed(key)
	if !ok || v == "" {
		return fallback, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf(
		"%s=%s is not a value brig understands. Use 1, true, yes or on to turn it "+
			"on, 0, false, no or off to turn it off, or unset it", name, v)
}

// setting renders "NAME=value" for the setting behind key, so a report quotes
// what the user actually set rather than a canonical spelling. When nothing is
// set it names the global variable; reporting callers reach this only for a
// switch they have already found to be on, so in practice a value is present.
func (e Env) setting(key string) string {
	if name, value, ok := e.getNamed(key); ok {
		return name + "=" + value
	}
	return "BRIG_" + key
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
