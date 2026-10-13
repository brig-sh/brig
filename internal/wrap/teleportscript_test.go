package wrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGuestPathScript runs guestPathScript with the host's own sh, the way
// prepareGuestHome runs it in the guest, and returns what it said and whether
// it exited 0.
func runGuestPathScript(t *testing.T, home, rel, kind, stdin string) (string, bool) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the guest script with")
	}
	cmd := exec.Command(sh, "-c", guestPathScript, "sh", home, rel, kind, "no-such-brig-user")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// The walk makes what is missing, one component at a time, and the file it
// makes is private and holds what was fed.
func TestGuestPathScriptMakesWhatIsMissing(t *testing.T) {
	home := t.TempDir()
	if out, ok := runGuestPathScript(t, home, "a/b/c", "dir", ""); !ok {
		t.Fatalf("dir: %s", out)
	}
	if fi, err := os.Stat(filepath.Join(home, "a/b/c")); err != nil || !fi.IsDir() {
		t.Errorf("dir was not made: %v", err)
	}
	if out, ok := runGuestPathScript(t, home, "d/f.json", "file", ""); !ok {
		t.Fatalf("file: %s", out)
	}
	if fi, err := os.Stat(filepath.Join(home, "d/f.json")); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
		t.Errorf("file was not made empty and 0600: %v %v", fi, err)
	}
	if out, ok := runGuestPathScript(t, home, ".tp/state.json", "seed", "{\"seeded\":true}\n"); !ok {
		t.Fatalf("seed: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".tp/state.json")); string(b) != "{\"seeded\":true}\n" {
		t.Errorf("seed wrote %q", b)
	}
	// A second run finds it and leaves it alone, the agent's edits included.
	if err := os.WriteFile(filepath.Join(home, ".tp/state.json"), []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, ok := runGuestPathScript(t, home, ".tp/state.json", "seed", "{\"seeded\":true}\n"); !ok {
		t.Fatalf("seed again: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".tp/state.json")); string(b) != "agent" {
		t.Errorf("a second seed overwrote the agent's file: %q", b)
	}
}

// A link the agent planted anywhere on the path is refused, and nothing is
// written where it points: not through a linked parent, not at a linked last
// component, and not when the home itself is a link.
func TestGuestPathScriptRefusesALinkOnThePath(t *testing.T) {
	for _, c := range []struct {
		what, rel, kind string
		plant           func(t *testing.T, home, outside string)
	}{
		{"a linked parent of a seed", ".tp/state.json", "seed", func(t *testing.T, home, outside string) {
			link(t, outside, filepath.Join(home, ".tp"))
		}},
		{"a linked grandparent of a file", "a/b/f", "file", func(t *testing.T, home, outside string) {
			link(t, outside, filepath.Join(home, "a"))
		}},
		{"a linked last component", "a/f", "file", func(t *testing.T, home, outside string) {
			mkdir(t, filepath.Join(home, "a"))
			link(t, filepath.Join(outside, "f"), filepath.Join(home, "a/f"))
		}},
		{"a linked directory target", "d", "dir", func(t *testing.T, home, outside string) {
			link(t, outside, filepath.Join(home, "d"))
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			c.plant(t, home, outside)
			out, ok := runGuestPathScript(t, home, c.rel, c.kind, "seed\n")
			if ok || !strings.Contains(out, "symlink") {
				t.Errorf("not refused as a symlink: ok %v, %q", ok, out)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("wrote through the link: %v", entries)
			}
		})
	}
	t.Run("a linked home", func(t *testing.T) {
		real, outside := t.TempDir(), t.TempDir()
		home := filepath.Join(real, "home")
		link(t, outside, home)
		if out, ok := runGuestPathScript(t, home, "f", "file", ""); ok || !strings.Contains(out, "symlink") {
			t.Errorf("a linked home was not refused: ok %v, %q", ok, out)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("wrote through the linked home: %v", entries)
		}
	})
}

// A path that leaves the home, or whose last component is not the kind the
// profile declared, is refused rather than made or bound from.
func TestGuestPathScriptRefusesAPathThatIsNotWhatTheProfileSays(t *testing.T) {
	home := t.TempDir()
	mkdir(t, filepath.Join(home, "d"))
	if err := os.WriteFile(filepath.Join(home, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ rel, kind string }{
		{"../escape", "dir"},
		{"a/../../escape", "file"},
		{"/abs", "file"},
		{"a//b", "dir"},
		{"d", "file"},
		{"f", "dir"},
		{"f/under", "dir"},
	} {
		if out, ok := runGuestPathScript(t, home, c.rel, c.kind, ""); ok {
			t.Errorf("%s as %s was accepted: %q", c.rel, c.kind, out)
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(home), "escape")); err == nil {
		t.Error("a path with .. was made outside the home")
	}
}

func link(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The hand-over to the guest user runs when the account exists. The host's
// own user stands in for it here: chown -h to yourself needs no privilege.
func TestGuestPathScriptHandsWhatItMakesToAnExistingUser(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the guest script with")
	}
	who, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Skip("no id to name the current user with")
	}
	home := t.TempDir()
	cmd := exec.Command(sh, "-c", guestPathScript, "sh", home, "a/f.json", "seed", strings.TrimSpace(string(who)))
	cmd.Stdin = strings.NewReader("{}\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the seed with a real user failed: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "a/f.json")); string(b) != "{}\n" {
		t.Errorf("seed wrote %q", b)
	}
}
