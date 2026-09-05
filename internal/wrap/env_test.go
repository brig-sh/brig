package wrap

import (
	"strings"
	"testing"
)

// oneVar is a lookup where a single variable is set and nothing else, so a case
// reads as "this is what the environment held" rather than as a map literal.
func oneVar(name, value string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == name {
			return value, true
		}
		return "", false
	}
}

// none is the empty environment: every lookup misses.
func none(string) (string, bool) { return "", false }

func TestEnvPrecedence(t *testing.T) {
	env := map[string]string{
		"BRIG_CLAUDE_CODE_WORKSPACE": "per-agent",
		"BRIG_WORKSPACE":             "global",
	}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	e := NewEnv("claude-code", get)

	if got := e.String("WORKSPACE", "fallback"); got != "per-agent" {
		t.Errorf("per-agent setting did not win: %q", got)
	}
	delete(env, "BRIG_CLAUDE_CODE_WORKSPACE")
	if got := e.String("WORKSPACE", "fallback"); got != "global" {
		t.Errorf("global setting did not win: %q", got)
	}
	delete(env, "BRIG_WORKSPACE")
	if got := e.String("WORKSPACE", "fallback"); got != "fallback" {
		t.Errorf("fallback = %q", got)
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

// strictSwitches is every key read with StrictBool: each one forwards a
// credential or writes brig's own files into the workspace when on, so an
// unrecognised value must refuse rather than fall open the way Bool does.
var strictSwitches = []string{
	"GIT_CONFIG", "TRUST_WORKSPACE", "ALLOW_REFS", "ALLOW_DENIED", "ALLOW_EXPIRED",
}

// The table the issue asks for: every spelling in the true set, the false set,
// an unrecognised value, absent and empty, for all five security switches. The
// old Bool test covered only "1", "0", absent and empty, which is exactly why
// "false" and "off" reading as on went unnoticed.
func TestEnvStrictBool(t *testing.T) {
	on := []string{"1", "true", "yes", "on", "TRUE", "Yes", "ON", "On"}
	off := []string{"0", "false", "no", "off", "FALSE", "No", "OFF", "Off"}

	for _, key := range strictSwitches {
		for _, v := range on {
			e := NewEnv("codex", oneVar("BRIG_"+key, v))
			got, err := e.StrictBool(key, false)
			if err != nil {
				t.Errorf("%s=%s: unexpected refusal: %v", key, v, err)
			}
			if !got {
				t.Errorf("%s=%s did not read as on", key, v)
			}
		}
		for _, v := range off {
			e := NewEnv("codex", oneVar("BRIG_"+key, v))
			got, err := e.StrictBool(key, true)
			if err != nil {
				t.Errorf("%s=%s: unexpected refusal: %v", key, v, err)
			}
			if got {
				t.Errorf("%s=%s did not read as off", key, v)
			}
		}

		// An unrecognised value refuses, naming both the variable and the
		// values it would have accepted, so the user is told what to write
		// rather than having their typo guessed at.
		e := NewEnv("codex", oneVar("BRIG_"+key, "maybe"))
		_, err := e.StrictBool(key, false)
		if err == nil {
			t.Fatalf("%s=maybe was accepted", key)
		}
		for _, want := range []string{"BRIG_" + key, "maybe", "1", "true", "yes", "on", "0", "false", "off"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s=maybe refusal does not name %q: %v", key, want, err)
			}
		}

		// Absent and empty keep the fallback, whichever way it points -- the
		// same meaning Bool gives them, so a switch left alone stays at its
		// default rather than refusing.
		for _, fallback := range []bool{true, false} {
			if got, err := NewEnv("codex", none).StrictBool(key, fallback); err != nil || got != fallback {
				t.Errorf("%s absent: got %v, err %v; want %v", key, got, err, fallback)
			}
			if got, err := NewEnv("codex", oneVar("BRIG_"+key, "")).StrictBool(key, fallback); err != nil || got != fallback {
				t.Errorf("%s empty: got %v, err %v; want %v", key, got, err, fallback)
			}
		}
	}
}

// The per-agent BRIG_<AGENT>_<KEY> beats the global BRIG_<KEY> for a strict
// switch too, the same precedence every other setting follows. It has to hold
// here or a global override left in one shell would quietly re-arm a switch an
// agent was set to turn off, and the refusal has to quote the variable the
// value actually came from rather than the global name the user never set.
func TestEnvStrictBoolPrecedence(t *testing.T) {
	env := map[string]string{
		"BRIG_CLAUDE_CODE_ALLOW_DENIED": "off",
		"BRIG_ALLOW_DENIED":             "on",
	}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	e := NewEnv("claude-code", get)

	got, err := e.StrictBool("ALLOW_DENIED", false)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("the per-agent off did not win over the global on")
	}

	env["BRIG_CLAUDE_CODE_ALLOW_DENIED"] = "maybe"
	_, err = e.StrictBool("ALLOW_DENIED", false)
	if err == nil || !strings.Contains(err.Error(), "BRIG_CLAUDE_CODE_ALLOW_DENIED=maybe") {
		t.Errorf("the refusal did not quote the per-agent setting the user actually set: %v", err)
	}
}
