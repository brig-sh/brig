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

// shell: true and gui: true are the pre-kind spellings. The decoder is strict,
// so without this an existing profile using them would fail to parse rather
// than being carried forward.
func TestDeprecatedShellAndGUIStillParse(t *testing.T) {
	base := "name: x\nimage: i\nguestHome: /home/x\nmem: 1\ncpus: 1\n"

	p, err := Parse([]byte(base + "shell: true\n"))
	if err != nil {
		t.Fatalf("shell: true no longer parses: %v", err)
	}
	if p.Kind != KindShell || !p.IsShell() {
		t.Errorf("shell: true did not become kind: shell (%+v)", p)
	}
	// Zeroed after normalising, so nothing downstream reads two sources of
	// truth and marshalling does not re-emit the old spelling.
	if p.Shell {
		t.Error("the deprecated field survived normalisation")
	}

	p, err = Parse([]byte(base + "gui: true\n"))
	if err != nil {
		t.Fatalf("gui: true no longer parses: %v", err)
	}
	if p.Kind != KindGUI || !p.IsGUI() {
		t.Errorf("gui: true did not become kind: gui (%+v)", p)
	}

	// A conflict is an error rather than a silent precedence rule, because
	// either answer would surprise whoever wrote both.
	if _, err := Parse([]byte(base + "kind: shell\ngui: true\n")); err == nil {
		t.Error("a conflicting kind and gui were accepted")
	}
	if _, err := Parse([]byte(base + "shell: true\ngui: true\n")); err == nil {
		t.Error("both deprecated flags at once were accepted")
	}
	// Agreeing with the new spelling is fine.
	if _, err := Parse([]byte(base + "kind: shell\nshell: true\n")); err != nil {
		t.Errorf("a redundant but consistent pair was rejected: %v", err)
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
		// The effective kind, not the raw field: six of the eight built-ins
		// say nothing and rely on the same default kind() applies everywhere
		// else, so an agent literal's Kind is "" rather than KindAgent.
		if got := p.kind(); got != want {
			t.Errorf("%s kind = %q, want %q", name, got, want)
		}
	}
}
