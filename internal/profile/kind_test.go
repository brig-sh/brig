package profile

import (
	"strings"
	"testing"
)

// An absent kind means agent, so neither the built-in specs nor anyone's own
// file has to spell out the common case.
func TestKindDefaultsToAgent(t *testing.T) {
	p, err := Parse([]byte("name: x\nimage: i\nguestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindAgent {
		t.Errorf("Kind = %q, want %q", p.Kind, KindAgent)
	}
	if p.IsShell() || p.IsGUI() {
		t.Error("an agent profile reports itself as shell or gui")
	}
}

// The strict decoder catches a misspelled field name but not a misspelled
// value. kind: shel silently falling back to agent would then demand a binary
// for no visible reason.
func TestUnrecognisedKindIsRejected(t *testing.T) {
	_, err := Parse([]byte("name: x\nkind: shel\nimage: i\nguestHome: /home/x\nmem: 1\ncpus: 1\n"))
	if err == nil {
		t.Fatal("a misspelled kind was accepted")
	}
	if !strings.Contains(err.Error(), "shel") {
		t.Errorf("the error does not name the bad value: %v", err)
	}
}

// kind: agent is the only one that needs a binary -- there is nothing to pass
// arguments to in the other two.
func TestBinaryIsRequiredOnlyForAgents(t *testing.T) {
	base := "name: x\nimage: i\nguestHome: /home/x\nmem: 1\ncpus: 1\n"
	if _, err := Parse([]byte(base)); err == nil {
		t.Error("an agent profile with no binary was accepted")
	}
	for _, kind := range []string{"shell", "gui"} {
		if _, err := Parse([]byte(base + "kind: " + kind + "\n")); err != nil {
			t.Errorf("kind: %s with no binary was rejected: %v", kind, err)
		}
	}
}

// Every built-in states a kind that matches what it is.
func TestBuiltInKinds(t *testing.T) {
	reset(t)
	for name, want := range map[string]Kind{
		"claude-code":    KindAgent,
		"claude-desktop": KindGUI,
		"codex":          KindAgent,
		"ubuntu":         KindShell,
	} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		// The effective kind, not the raw field: seven of the nine built-ins
		// say nothing and rely on the same default kind() applies everywhere
		// else, so an agent literal's Kind is "" rather than KindAgent.
		if got := p.kind(); got != want {
			t.Errorf("%s kind = %q, want %q", name, got, want)
		}
	}
}
