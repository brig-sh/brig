package notice

import "testing"

// Notes come as written, the inputs of each kind of row line up in one
// column, a row with nothing to type is its label alone, and a hint on a
// missing thing sits under its command.
func TestBlockLinesUpEachColumn(t *testing.T) {
	got := New("the heading").
		Note("a note about %s", "it").
		Missing("gh-token", "brig secret create gh-token").
		Missing("claude-credentials", "brig secret import claude-code", "run `claude` once").
		Do("short", "brig one").
		Do("a longer label", "brig two", "a note on brig two").
		Do("unset BRIG_X to stop this", "").
		String()
	want := "the heading\n" +
		"  ↳ a note about it\n" +
		"  ○ gh-token            → brig secret create gh-token\n" +
		"  ○ claude-credentials  → brig secret import claude-code\n" +
		"                          ↳ run `claude` once\n" +
		"  → short:           brig one\n" +
		"  → a longer label:  brig two\n" +
		"                     ↳ a note on brig two\n" +
		"  → unset BRIG_X to stop this"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Prose is the heading and its notes as sentences, for a line that is not a
// block. Actions are left to the block.
func TestProseJoinsTheNotes(t *testing.T) {
	got := New("cannot verify image x: no cosign").Note("booting it unchecked").Do("to fix", "brew install cosign").Prose()
	if want := "cannot verify image x: no cosign. Booting it unchecked"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Sentence upper-cases a first letter of any width, and leaves an empty
// string empty.
func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"the bundle": "The bundle", "élan": "Élan", "": ""} {
		if got := Sentence(in); got != want {
			t.Errorf("Sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
