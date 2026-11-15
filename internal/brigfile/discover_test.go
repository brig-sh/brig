package brigfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// insideGit skips a test whose "outside Git" premise is false on this
// machine, because a temporary directory sits inside a checkout.
func insideGit(t *testing.T, dir string) {
	t.Helper()
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			t.Skipf("%s is inside a Git checkout at %s", dir, d)
		}
		if filepath.Dir(d) == d {
			return
		}
	}
}

func TestDiscoverFindsTheBrigfileAtTheWorktreeRoot(t *testing.T) {
	for _, form := range []string{"directory", "file"} {
		t.Run(form, func(t *testing.T) {
			files := map[string]string{FileName: minimal, "a/b/c/keep": ""}
			if form == "file" {
				// A linked worktree has a .git file that points at the main
				// checkout's git directory.
				files[".git"] = "gitdir: /elsewhere/.git/worktrees/x\n"
			} else {
				files[".git/HEAD"] = "ref: refs/heads/main\n"
			}
			dir := project(t, files)
			got, err := Discover(filepath.Join(dir, "a", "b", "c"))
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got != filepath.Join(dir, FileName) {
				t.Errorf("Discover = %q, want %q", got, filepath.Join(dir, FileName))
			}
		})
	}
}

// A Brigfile in a directory between the start and the worktree root is not
// the project's, and is not picked up.
func TestDiscoverIgnoresABrigfileBelowTheRoot(t *testing.T) {
	dir := project(t, map[string]string{
		".git/HEAD":         "",
		FileName:            minimal,
		"a/" + FileName:     "planted",
		"a/b/" + FileName:   "planted",
		"a/b/c/placeholder": "",
	})
	for _, start := range []string{"a", "a/b", "a/b/c"} {
		got, err := Discover(filepath.Join(dir, start))
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(dir, FileName) {
			t.Errorf("from %s: Discover = %q", start, got)
		}
	}

	// With no Brigfile at the root, one below it is still not found.
	dir = project(t, map[string]string{".git/HEAD": "", "a/" + FileName: "planted", "a/b/placeholder": ""})
	if got, err := Discover(filepath.Join(dir, "a", "b")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Discover = %q, %v; want ErrNotFound", got, err)
	}
}

// The walk stops at the first .git, and never looks above it: a Brigfile in
// a directory that holds the repository is not the repository's.
func TestDiscoverNeverLooksAboveTheRoot(t *testing.T) {
	dir := project(t, map[string]string{
		FileName:           minimal,
		"repo/.git/HEAD":   "",
		"repo/sub/keep":    "",
		"other/.git":       "gitdir: x\n",
		"other/inner/keep": "",
	})
	for _, start := range []string{"repo", "repo/sub", "other/inner"} {
		got, err := Discover(filepath.Join(dir, start))
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("from %s: Discover = %q, %v; want ErrNotFound", start, got, err)
		}
	}
}

// Outside Git the start directory is the only place looked at.
func TestDiscoverOutsideGitReadsOnlyTheStartDirectory(t *testing.T) {
	dir := project(t, map[string]string{FileName: minimal, "sub/keep": ""})
	insideGit(t, dir)
	if got, err := Discover(dir); err != nil || got != filepath.Join(dir, FileName) {
		t.Errorf("Discover(dir) = %q, %v", got, err)
	}
	got, err := Discover(filepath.Join(dir, "sub"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Discover(sub) = %q, %v; want ErrNotFound", got, err)
	}
}

// A start directory reached through a link is walked as the directory it
// resolves to, the way git finds its root.
func TestDiscoverWalksTheResolvedStartDirectory(t *testing.T) {
	dir := project(t, map[string]string{".git/HEAD": "", FileName: minimal, "a/keep": ""})
	links := project(t, nil)
	link := filepath.Join(links, "link")
	if err := os.Symlink(filepath.Join(dir, "a"), link); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(link)
	if err != nil || got != filepath.Join(dir, FileName) {
		t.Errorf("Discover(link) = %q, %v", got, err)
	}
}

// Root is the directory Discover looks in, whether or not a Brigfile is
// there, so that brig init writes where the other verbs will look.
func TestRootIsWhereDiscoverLooks(t *testing.T) {
	dir := project(t, map[string]string{".git/HEAD": "", "a/b/keep": ""})
	for _, start := range []string{"", "a", "a/b"} {
		got, err := Root(filepath.Join(dir, start))
		if err != nil || got != dir {
			t.Errorf("Root(%q) = %q, %v; want %q", start, got, err, dir)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := Discover(filepath.Join(dir, "a", "b"))
	if err != nil || filepath.Dir(found) != dir {
		t.Errorf("Discover = %q, %v; want a Brigfile in %s", found, err, dir)
	}

	outside := project(t, map[string]string{"sub/keep": ""})
	insideGit(t, outside)
	if got, err := Root(filepath.Join(outside, "sub")); err != nil || got != filepath.Join(outside, "sub") {
		t.Errorf("Root outside Git = %q, %v; want the start directory", got, err)
	}
}

// Discover and Load together: the path Discover returns loads.
func TestDiscoverThenLoad(t *testing.T) {
	dir := project(t, map[string]string{".git/HEAD": "", FileName: minimal, "agent.yaml": agentPolicy, "x/keep": ""})
	path, err := Discover(filepath.Join(dir, "x"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != dir {
		t.Errorf("root %q, want %q", l.Root, dir)
	}
}

// Load of an explicit path takes that file's directory as the project root,
// so references resolve beside it and not beside the working directory.
func TestLoadAnExplicitPathRootsAtItsDirectory(t *testing.T) {
	dir := project(t, map[string]string{
		"ci/Brigfile.ci": minimal,
		"ci/agent.yaml":  agentPolicy,
		"agent.yaml":     "not this one",
	})
	l, err := Load(filepath.Join(dir, "ci", "Brigfile.ci"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != filepath.Join(dir, "ci") || string(l.Files["agent.yaml"]) != agentPolicy {
		t.Errorf("root %q, files %v", l.Root, l.Files)
	}
}
