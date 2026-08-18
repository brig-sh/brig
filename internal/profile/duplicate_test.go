package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a profile file in a directory. The basename is deliberately a
// separate argument from the name: the two do not have to agree, and every
// test in this file exists because they do not have to agree.
func write(t *testing.T, dir, basename, name, image string) string {
	t.Helper()
	path := filepath.Join(dir, basename)
	blob := []byte("name: " + name + "\nimage: " + image + "\n" +
		"guestHome: /home/x\nbinary: x\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two files in one directory can declare the same profile, because a file need
// not be named after the profile inside it. One of them then decides nothing,
// and which one is settled by where the names happen to sort -- so the user
// edits a file, sees no change, and has nothing to go on. Load says which.
func TestDuplicateNamesInOneDirectoryAreReported(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	write(t, dir, "codex.yaml", "codex", "docker.io/me/first:latest")
	write(t, dir, "pinned.yaml", "codex", "docker.io/me/second:latest")

	err := Load(dir)
	if err == nil {
		t.Fatal("two files claiming one profile were accepted in silence")
	}
	for _, want := range []string{"codex.yaml", "pinned.yaml", "codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not name %q:\n%v", want, err)
		}
	}
	// Reported, not fatal: brig still runs the profile, and says which file won.
	got, ok := Lookup("codex")
	if !ok {
		t.Fatal("the profile did not load at all")
	}
	if got.Image != "docker.io/me/second:latest" {
		t.Errorf("the winner is not the last file read: %s", got.Image)
	}
	if !strings.Contains(err.Error(), "pinned.yaml wins") {
		t.Errorf("the report does not say which file won:\n%v", err)
	}
}

// A file shadowing a built-in is the supported way to pin your own image, so
// it must not be reported as a duplicate: the two are in different sources.
func TestOverridingABuiltInIsNotADuplicate(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	write(t, dir, "codex.yaml", "codex", "docker.io/me/mine:latest")
	if err := Load(dir); err != nil {
		t.Fatalf("overriding a built-in was reported as a problem: %v", err)
	}
}

// Removing a profile has to take every file that declares it. Taking only the
// one that loaded promotes the file underneath, so the command reports success
// and the profile is still there -- the one outcome an rm must not have.
func TestRemoveTakesEveryFileDeclaringTheName(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	first := write(t, dir, "codex.yaml", "codex", "docker.io/me/first:latest")
	second := write(t, dir, "pinned.yaml", "codex", "docker.io/me/second:latest")
	_ = Load(dir) // duplicates are reported; that is this fixture's point

	removed, err := Remove("codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %v, want both files", removed)
	}
	for _, f := range []string{first, second} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s survived the removal", f)
		}
	}
	// And the built-in is back, which is what makes the removal visible.
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if IsCustom("codex") {
		t.Error("codex is still file-backed after being removed")
	}
}

// A built-in has no file, so Remove reports nothing to remove rather than
// failing on a path that was never there.
func TestRemoveOfABuiltInTakesNothing(t *testing.T) {
	reset(t)
	if files := Files("codex"); files != nil {
		t.Errorf("a built-in reports backing files: %v", files)
	}
	removed, err := Remove("codex")
	if err != nil || len(removed) != 0 {
		t.Errorf("Remove(built-in) = %v, %v", removed, err)
	}
}
