package profile

import (
	"reflect"
	"strings"
	"testing"
)

// forward: is deprecated but still works, and translates mechanically -- so
// nothing below the parser ever sees two mechanisms for getting a variable into
// the guest. Same treatment normaliseKind gives shell: and gui:.
func TestForwardTranslatesToEnvBindings(t *testing.T) {
	p, err := Parse([]byte(bindingBase + "forward:\n  - GH_TOKEN\n  - CI\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Forward != nil {
		t.Errorf("Forward survived translation: %v", p.Forward)
	}
	want := []EnvBinding{
		{Name: "GH_TOKEN", Ref: "env.GH_TOKEN"},
		{Name: "CI", Ref: "env.CI"},
	}
	if len(p.Env) != len(want) {
		t.Fatalf("Env = %+v, want %+v", p.Env, want)
	}
	for i := range want {
		if !reflect.DeepEqual(p.Env[i], want[i]) {
			t.Errorf("Env[%d] = %+v, want %+v", i, p.Env[i], want[i])
		}
	}
}

// Order is the profile's own: env: entries first, then the translated forward:
// ones, so a report reads the way the file does.
func TestForwardAppendsAfterExplicitEnv(t *testing.T) {
	p, err := Parse([]byte(bindingBase +
		"env:\n  - name: A\n    ref: env.A\nforward:\n  - B\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Env) != 2 || p.Env[0].Name != "A" || p.Env[1].Name != "B" {
		t.Fatalf("Env = %+v", p.Env)
	}
}

// Both spellings for one variable is the same contradiction kind: and shell:
// already refuse: either precedence rule surprises whoever wrote both.
func TestForwardAndEnvForOneVariableIsAnError(t *testing.T) {
	_, err := Parse([]byte(bindingBase +
		"secrets:\n  - a\nenv:\n  - name: A\n    ref: secrets.a\nforward:\n  - A\n"))
	if err == nil {
		t.Fatal("a variable bound twice was accepted")
	}
	if !strings.Contains(err.Error(), "A") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

// The pre-existing forward+deny contradiction still fails, under its new
// spelling. This is the regression the translation could most easily lose, and
// it is the billing guard.
func TestForwardOnTheDenylistStillFails(t *testing.T) {
	_, err := Parse([]byte(bindingBase + "deny:\n  - K\nforward:\n  - K\n"))
	if err == nil {
		t.Fatal("a variable in both forward and deny was accepted")
	}
	if !strings.Contains(err.Error(), "K") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

// A repeated name within forward: alone is harmless today and must stay that
// way: registry.go falls back silently to the stock built-in on a parse
// failure, so turning this into an error would swap a user's image with no
// error shown at all. It has no env: block to collide with, so a naive
// "already bound" check would misfire on it.
func TestForwardDedupesRepeatedNames(t *testing.T) {
	p, err := Parse([]byte(bindingBase + "forward:\n  - A\n  - A\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Env) != 1 || p.Env[0].Name != "A" {
		t.Fatalf("Env = %+v, want exactly one binding for A", p.Env)
	}
}

// Every built-in profile ships forward: today, so the translation is exercised
// by the real specs rather than only by fixtures.
func TestBuiltInProfilesTranslateCleanly(t *testing.T) {
	reset(t)
	for _, tmpl := range All() {
		if tmpl.Forward != nil {
			t.Errorf("%s: Forward survived translation: %v", tmpl.Name, tmpl.Forward)
		}
		if len(tmpl.Env) == 0 {
			t.Errorf("%s: translation produced no bindings", tmpl.Name)
		}
		for _, b := range tmpl.Env {
			if b.Ref == "" && b.Value == "" {
				t.Errorf("%s: binding %s has no source", tmpl.Name, b.Name)
			}
		}
	}
}
