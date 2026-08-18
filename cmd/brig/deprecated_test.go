package main

import (
	"strings"
	"testing"
)

// The old spellings keep working for one release. Anyone who scripted against
// brig template should get a working command and a note, not a failure.
func TestDeprecatedVerbsStillWork(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{{"agents"}, {"template", "ls"}} {
		if err := run(args); err != nil {
			t.Errorf("brig %s: %v", strings.Join(args, " "), err)
		}
	}
}

// There is no brig template edit: the deprecated group carries only the verbs
// it already had. A command that never existed under the old name does not
// need a deprecated spelling, and adding one would extend the vocabulary this
// change is retiring.
func TestNoDeprecatedEditVerb(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := run([]string{"template", "edit", "mine"}); err == nil {
		t.Error("brig template edit exists")
	}
}
