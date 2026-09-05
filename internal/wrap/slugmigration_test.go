package wrap

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The session the migration notice exists for: a name longer than the ten
// characters an older release cut it to, whose old home is still on disk. Its
// sandbox and its home both move, so the notice has to name both directories
// -- the old one so it can be moved or deleted, the new one so the reader can
// see where the session went.
func TestALongNameIsToldItsHomeHasMoved(t *testing.T) {
	isolateState(t)
	base := filepath.Join(t.TempDir(), "ws")
	// The directory the old budget gave "refactoring". Its being here is half
	// of what the notice is decided on.
	mustMkdir(t, base+"-refactorin")

	c := mustLoad(t, Options{Name: "refactoring", Workspace: base})
	if len(c.slugMigration) == 0 {
		t.Fatal("a session whose home has moved was told nothing")
	}
	notice := strings.Join(c.slugMigration, " ")
	for _, want := range []string{base + "-refactorin", base + "-refactoring"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not name %s: %s", want, notice)
		}
	}
}

// Nothing orphaned, nothing to say. The name slugs differently than it used
// to, but no directory was ever created under the old slug, so there is
// nothing for the reader to move and the notice would be noise -- which is how
// people learn to skip the ones that do apply to them.
func TestNoMigrationNoticeWhenTheOldHomeIsNotThere(t *testing.T) {
	isolateState(t)
	base := filepath.Join(t.TempDir(), "ws")

	c := mustLoad(t, Options{Name: "refactoring", Workspace: base})
	if len(c.slugMigration) != 0 {
		t.Errorf("a session with nothing orphaned was warned: %v", c.slugMigration)
	}
}

// A name the old budget left alone slugs exactly as it always did, so its home
// has not moved and its directory being there means nothing. Ten characters is
// the boundary, and it is on the silent side of it.
func TestNoMigrationNoticeForANameTheOldBudgetLeftAlone(t *testing.T) {
	isolateState(t)
	base := filepath.Join(t.TempDir(), "ws")
	mustMkdir(t, base+"-exactlyten")

	c := mustLoad(t, Options{Name: "exactlyten", Workspace: base})
	if len(c.slugMigration) != 0 {
		t.Errorf("a name that was never cut was warned: %v", c.slugMigration)
	}
}

// The notice reaches the user, and it reaches them from BuildEnv with the env
// warnings rather than from Load: both are decisions Load made and neither is
// a reason to fail, so they are said together, once, on the way to the run
// that is about to create the new directory.
func TestBuildEnvSaysTheMigrationNotice(t *testing.T) {
	c := bindingConfig(t, "env:\n  - name: NOPE\n    ref: env.NOPE\n")
	c.slugMigration = []string{"the home has moved", "the guest state has not"}
	warned := &bytes.Buffer{}
	c.Err = warned

	if _, err := c.BuildEnv(); err != nil {
		t.Fatal(err)
	}
	for _, want := range c.slugMigration {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the notice did not reach stderr: %q", warned.String())
		}
	}
}
