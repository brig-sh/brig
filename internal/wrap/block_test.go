package wrap

import "testing"

// A block's notes come first as written, and the commands line up in one
// column however long the label before each one is. A row with nothing to
// type is its label alone.
func TestRowsLineUpTheCommands(t *testing.T) {
	got := (&Rows{}).
		Note("a note about %s", "it").
		Do("short", "brig one").
		Do("a longer label", "brig two").
		Do("unset BRIG_X to stop this", "").
		Block("the heading")
	want := "the heading\n" +
		"  ↳ a note about it\n" +
		"  → short:           brig one\n" +
		"  → a longer label:  brig two\n" +
		"  → unset BRIG_X to stop this"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
