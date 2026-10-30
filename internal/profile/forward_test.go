package profile

import (
	"strings"
	"testing"
)

// v0.5.0 removed forward:, statePaths:, shell: and gui:. The strict decoder
// refuses each as a field it does not know, and the error names the key that
// replaced it, so a profile written against an older release says what to
// change rather than only that something is wrong.
func TestRemovedKeysAreRefusedNamingTheReplacement(t *testing.T) {
	for _, c := range []struct {
		line, want string
	}{
		{"forward:\n  - GH_TOKEN\n", "use env:, with ref: env.<name>"},
		{"forward: [GH_TOKEN]\n", "use env:, with ref: env.<name>"},
		{"statePaths: [.codex]\n", "use volumes:"},
		{"shell: true\n", "use kind: shell"},
		{"gui: true\n", "use kind: gui"},
	} {
		_, err := Parse([]byte(bindingBase + c.line))
		if err == nil {
			t.Errorf("%q was accepted", c.line)
			continue
		}
		if !strings.Contains(err.Error(), "was removed in v0.5.0") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want it to say it was removed and %s", c.line, err, c.want)
		}
	}
	// Any other unknown field is refused as it always was, with no hint that
	// would send the reader after a key they did not write.
	_, err := Parse([]byte(bindingBase + "forwards: [GH_TOKEN]\n"))
	if err == nil || strings.Contains(err.Error(), "removed") {
		t.Errorf("forwards: = %v, want the plain unknown-field refusal", err)
	}
	// The decoder does not say how deep a field sat. A removed key's name
	// inside volumes: is a typo there, not a top-level key to move.
	_, err = Parse([]byte(bindingBase + "volumes:\n  - path: .x\n    shell: true\n"))
	if err == nil || strings.Contains(err.Error(), "removed") {
		t.Errorf("a nested shell: = %v, want the plain unknown-field refusal", err)
	}
}

// The built-in profiles that shipped forward: bind the same variables under
// env:, each from brig's own environment, in the order forward: listed them.
// A variable dropped in that rewrite is a credential that silently stops
// reaching the guest.
func TestBuiltInProfilesBindWhatForwardBound(t *testing.T) {
	reset(t)
	for name, want := range map[string][]string{
		"codex":    {"GH_TOKEN"},
		"cursor":   {"CURSOR_API_KEY", "GH_TOKEN"},
		"gemini":   {"GEMINI_API_KEY", "GH_TOKEN"},
		"grok":     {"XAI_API_KEY", "GH_TOKEN"},
		"opencode": {"OPENROUTER_API_KEY", "GH_TOKEN"},
		"ubuntu":   {"GH_TOKEN"},
	} {
		p, ok := Lookup(name)
		if !ok {
			t.Errorf("%s is not a built-in", name)
			continue
		}
		var got []string
		for _, b := range p.Env {
			if b.Ref != NamespaceEnv+"."+b.Name {
				t.Errorf("%s: %s is bound from %q, want env.%s", name, b.Name, b.Ref, b.Name)
			}
			got = append(got, b.Name)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s binds %v, want %v", name, got, want)
		}
	}
}

// Every built-in binds what it needs from a source, through env: alone.
func TestBuiltInProfilesTranslateCleanly(t *testing.T) {
	reset(t)
	for _, tmpl := range All() {
		if len(tmpl.Env) == 0 {
			t.Errorf("%s: no bindings", tmpl.Name)
		}
		for _, b := range tmpl.Env {
			// len(RefList()) rather than Ref alone: claude-code binds
			// GH_TOKEN through a refs: chain, and a check that only knew
			// about the singular spelling read brig's own built-in as a
			// binding with nothing behind it.
			if len(b.RefList()) == 0 && b.Value == "" {
				t.Errorf("%s: binding %s has no source", tmpl.Name, b.Name)
			}
		}
	}
}
