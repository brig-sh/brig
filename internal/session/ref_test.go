package session

import (
	"strings"
	"testing"
)

// The refs a command line is allowed to carry, and what they parse to. A bare
// agent is the agent's default session; the label is everything after the one
// '@'.
func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
	}{
		{"claude", Ref{Agent: "claude"}},
		{"claude-code", Ref{Agent: "claude-code"}},
		{"claude@refactor", Ref{Agent: "claude", Label: "refactor"}},
		// The label's character set is Slug's own, so everything Slug leaves
		// alone is a label: digits, dot, dash and underscore included.
		{"claude@my_proj", Ref{Agent: "claude", Label: "my_proj"}},
		{"claude@v1.2", Ref{Agent: "claude", Label: "v1.2"}},
		{"claude@a-b", Ref{Agent: "claude", Label: "a-b"}},
		{"claude@2", Ref{Agent: "claude", Label: "2"}},
		// The reservation is scoped to the profile the agent resolves to.
		// claude is the claude-code profile, so claude@desktop is
		// claude-code-desktop, a workspace no profile owns; codex@desktop is
		// codex-desktop, likewise. Both are accepted.
		{"claude@desktop", Ref{Agent: "claude", Label: "desktop"}},
		{"codex@desktop", Ref{Agent: "codex", Label: "desktop"}},
		// A session workspace is always <agent>-<label>, so a label that is a
		// reserved profile's whole name is codex-claude-desktop here, which
		// nothing owns.
		{"codex@claude-desktop", Ref{Agent: "codex", Label: "claude-desktop"}},
		// Long, and slug-clean at that length. Nothing is cut any more, so a
		// label is refused for what is in it and never for how much of it
		// there is.
		{"claude@a-really-rather-long-refactor-label",
			Ref{Agent: "claude", Label: "a-really-rather-long-refactor-label"}},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
		// String is the spelling ParseRef reads, so every valid ref survives
		// the round trip. Without this the two halves can drift and a ref
		// printed by one command stops being a ref the next one accepts.
		if s := got.String(); s != c.in {
			t.Errorf("ParseRef(%q).String() = %q", c.in, s)
		}
		again, err := ParseRef(got.String())
		if err != nil || again != got {
			t.Errorf("ParseRef(%q.String()) = (%+v, %v), want (%+v, nil)", c.in, again, err, got)
		}
	}
}

// The refusals. A label is refused rather than rewritten, which is what takes
// the collision class away: two labels that differ only in what Slug would
// have thrown out cannot name one sandbox if neither of them parses.
func TestParseRefRefusals(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string // a word the message has to carry
	}{
		// Empty on either side of the '@'. A trailing '@' is a label that was
		// meant to be typed, not a way to ask for the default session.
		{"claude@", "claude@"},
		{"@refactor", "@refactor"},
		{"", ""},
		{"@", "@"},
		// One '@' and no more: a second one is a typo, and picking a side
		// would make `a@b@c` mean whichever side the parser happened to read.
		{"claude@a@b", "claude@a@b"},
		// Long and not slug-clean. It is the uppercase that refuses this, not
		// the length: the same label in lower case parses above.
		{"claude@A-Really-Rather-Long-Refactor-Label", "A-Really-Rather-Long-Refactor-Label"},
		// Not slug-clean: uppercase, a space, a path separator, a leading
		// dash, a label with nothing usable in it, and a non-ASCII label.
		{"claude@Refactor", "Refactor"},
		{"claude@my ref", "my ref"},
		{"claude@a/b", "a/b"},
		{"claude@-foo", "-foo"},
		{"claude@...", "..."},
		{"claude@ünï", "ünï"},
	} {
		got, err := ParseRef(c.in)
		if err == nil {
			t.Errorf("ParseRef(%q) = %+v, want a refusal", c.in, got)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseRef(%q): %v, want it to name %q", c.in, err, c.want)
		}
	}
}

// String is the ref as it is typed, and a ref with no label is the agent on
// its own rather than an agent with an empty label bolted on.
func TestRefString(t *testing.T) {
	for _, c := range []struct {
		ref  Ref
		want string
	}{
		{Ref{Agent: "claude"}, "claude"},
		{Ref{Agent: "claude", Label: "refactor"}, "claude@refactor"},
	} {
		if got := c.ref.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.ref, got, c.want)
		}
	}
}

// A label is refused for what is in it, not for how long it is. Both halves
// are asserted together because they are one fact: Slug does not cut, so there
// is no length at which ParseRef starts refusing -- and a budget put back into
// Slug would make ParseRef refuse long labels again without a word being
// changed here.
func TestALongLabelIsRefusedOnlyForItsCharacters(t *testing.T) {
	long := strings.Repeat("a", 200)
	if got := Slug(long); got != long {
		t.Fatalf("Slug cut a %d-character label to %d, which is what ParseRef would refuse it for",
			len(long), len(got))
	}
	if _, err := ParseRef("claude@" + long); err != nil {
		t.Errorf("a long slug-clean label was refused: %v", err)
	}
	// And length excuses nothing: the character test is the whole of the rule,
	// at any length.
	if _, err := ParseRef("claude@" + strings.ToUpper(long)); err == nil {
		t.Errorf("a long label Slug would rewrite was accepted")
	}
}
