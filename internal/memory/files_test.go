package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTest(t *testing.T) (*Files, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "memory")
	f, err := OpenFiles(root, "notes")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, root, filepath.Join(root, "notes")
}

func TestPutGetRoundTrip(t *testing.T) {
	f, _, dir := openTest(t)
	ctx := context.Background()
	want := Memory{Name: "build", Description: "how to build", Body: "run make\n\nthen test\n"}
	if err := f.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get(ctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "build.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "---\nname: build\ndescription: how to build\n---\n\nrun make\n\nthen test\n" {
		t.Fatalf("on-disk form = %q", raw)
	}
	for _, p := range []string{dir, filepath.Join(dir, "build.md")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %#o is open to group/other", p, fi.Mode().Perm())
		}
	}
	// An empty body must survive the round trip too.
	if err := f.Put(ctx, Memory{Name: "empty", Description: ""}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Get(ctx, "empty"); err != nil || got.Name != "empty" || got.Body != "" {
		t.Fatalf("empty memory = %+v, %v", got, err)
	}
}

func TestGetAndDeleteMissing(t *testing.T) {
	f, _, _ := openTest(t)
	ctx := context.Background()
	if _, err := f.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if err := f.Delete(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := f.Put(ctx, Memory{Name: "a", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
	if idx, _ := os.ReadFile(filepath.Join(f.dir, "MEMORY.md")); len(idx) != 0 {
		t.Fatalf("MEMORY.md after the last Delete = %q", idx)
	}
}

func TestIndexRebuiltFromFiles(t *testing.T) {
	f, _, dir := openTest(t)
	ctx := context.Background()
	hand := "---\nname: hand\ndescription: written by hand\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "hand.md"), []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hand-written file that is not in the stored form is skipped, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "junk.md"), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := f.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 1 || idx[0] != (Entry{Name: "hand", Description: "written by hand"}) {
		t.Fatalf("Index = %+v", idx)
	}
	if err := os.Remove(filepath.Join(dir, "MEMORY.md")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := f.Put(ctx, Memory{Name: "zed", Description: "last", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "- hand: written by hand\n- zed: last\n"; string(got) != want {
		t.Fatalf("MEMORY.md = %q, want %q", got, want)
	}
	idx, _ = f.Index(ctx)
	if len(idx) != 2 || idx[0].Name != "hand" || idx[1].Name != "zed" {
		t.Fatalf("Index = %+v", idx)
	}
}

func TestPutRefusesBadNames(t *testing.T) {
	f, root, _ := openTest(t)
	parent := filepath.Dir(root)
	for _, name := range []string{"../x", "a/b", ".hidden", "A", "", strings.Repeat("a", MaxName+1), ".", "..", "memory", "a b", `a\b`} {
		err := f.Put(context.Background(), Memory{Name: name, Description: "d", Body: "b"})
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put(%q) = %v, want ErrInvalidName", name, err)
		}
		if _, err := f.Get(context.Background(), name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Get(%q) = %v, want ErrInvalidName", name, err)
		}
		if err := f.Delete(context.Background(), name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	// Nothing landed outside the store, or in the store.
	for _, dir := range []string{parent, root, f.dir} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".md") || e.Name() == "x" {
				t.Errorf("unexpected %s in %s", e.Name(), dir)
			}
		}
	}
	if err := f.Put(context.Background(), Memory{Name: "ok", Description: "line one\nline two"}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a description with a newline = %v, want ErrInvalidName", err)
	}
}

func TestPlantedSymlinkRefused(t *testing.T) {
	f, _, dir := openTest(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "secret")
	secret := []byte("host secret\n")
	if err := os.WriteFile(outside, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, "evil"); err == nil {
		t.Error("Get followed a planted symlink")
	}
	if err := f.Put(ctx, Memory{Name: "evil", Description: "d", Body: "overwrite"}); err == nil {
		t.Error("Put wrote through a planted symlink")
	}
	if err := f.Delete(ctx, "evil"); err != nil {
		t.Logf("Delete of the link: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
	// The link is not a memory, so Index and Search skip it.
	if err := os.Symlink(outside, filepath.Join(dir, "evil2.md")); err != nil {
		t.Fatal(err)
	}
	if idx, err := f.Index(ctx); err != nil || len(idx) != 0 {
		t.Fatalf("Index listed a link: %+v, %v", idx, err)
	}
	if hits, err := f.Search(ctx, "host"); err != nil || len(hits) != 0 {
		t.Fatalf("Search read through a link: %+v, %v", hits, err)
	}
}

func TestStoreDirRefusedWhenLinkOrOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFiles(root, "linked"); err == nil {
		t.Error("OpenFiles accepted a symlinked store dir")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("a symlinked store dir was written through: %v", ents)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFiles(root, "open"); err == nil {
		t.Error("OpenFiles accepted a 0755 store dir")
	}
	if _, err := OpenFiles(root, "../escape"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("OpenFiles(../escape) = %v, want ErrInvalidName", err)
	}
}

func TestLimits(t *testing.T) {
	f, _, dir := openTest(t)
	ctx := context.Background()
	if err := f.Put(ctx, Memory{Name: "big", Description: "d", Body: strings.Repeat("x", MaxBody+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize body = %v, want ErrTooLarge", err)
	}
	if err := f.Put(ctx, Memory{Name: "big", Description: strings.Repeat("d", MaxDescription+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize description = %v, want ErrTooLarge", err)
	}
	if err := f.Put(ctx, Memory{Name: "max", Description: "d", Body: strings.Repeat("x", MaxBody)}); err != nil {
		t.Fatalf("body of exactly MaxBody: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.md")); !os.IsNotExist(err) {
		t.Fatal("a refused Put left a file")
	}
	if err := f.Delete(ctx, "max"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxMemories; i++ {
		if err := f.Put(ctx, Memory{Name: fmt.Sprintf("m%03d", i), Description: "d", Body: "b"}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if err := f.Put(ctx, Memory{Name: "one-too-many", Description: "d", Body: "b"}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("501st = %v, want ErrStoreFull", err)
	}
	if err := f.Put(ctx, Memory{Name: "m007", Description: "d2", Body: "replaced"}); err != nil {
		t.Fatalf("replace at the limit: %v", err)
	}
	if m, _ := f.Get(ctx, "m007"); m.Body != "replaced" {
		t.Fatalf("m007 = %+v", m)
	}
}

func TestConcurrentPutsBothLand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	a, err := OpenFiles(root, "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenFiles(root, "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		for j, f := range []*Files{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m := Memory{Name: fmt.Sprintf("s%d-%02d", j, i), Description: "d", Body: "b"}
				if err := f.Put(context.Background(), m); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	idx, err := a.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 100 {
		t.Fatalf("Index has %d entries, want 100", len(idx))
	}
	for _, e := range idx {
		if _, err := b.Get(context.Background(), e.Name); err != nil {
			t.Errorf("Get %s: %v", e.Name, err)
		}
	}
	lines, _ := os.ReadFile(filepath.Join(root, "shared", "MEMORY.md"))
	if n := strings.Count(string(lines), "\n"); n != 100 {
		t.Errorf("MEMORY.md has %d lines, want 100", n)
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "shared")); len(ents) != 101 {
		t.Errorf("store holds %d files, want 100 memories and MEMORY.md (a temp file leaked?)", len(ents))
	}
}

func TestSearchCaseInsensitive(t *testing.T) {
	f, _, _ := openTest(t)
	ctx := context.Background()
	for _, m := range []Memory{
		{Name: "zeta", Description: "auth", Body: "the API token lives in env"},
		{Name: "alpha", Description: "TOKEN rotation", Body: "weekly"},
		{Name: "other", Description: "unrelated", Body: "nothing"},
	} {
		if err := f.Put(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := f.Search(ctx, "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Name != "alpha" || hits[1].Name != "zeta" {
		t.Fatalf("Search = %+v, want alpha then zeta", hits)
	}
}

func TestSetProjectAndList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	key := ProjectKey("/work/app")
	f, err := OpenFiles(root, key)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.SetProject("/work/app/"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, key, "project"))
	if string(raw) != "/work/app\n" {
		t.Fatalf("project file = %q", raw)
	}
	got, err := List(root)
	if err != nil || len(got) != 1 || got[0].Key != key || got[0].Project != "/work/app" {
		t.Fatalf("List = %+v, %v, want %s for /work/app", got, err, key)
	}
}

// SetProject records only the folder a project store's key was made from:
// a relative path, a named store and a folder of another key are refused,
// and nothing is written.
func TestSetProjectValidates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	p, err := OpenFiles(root, ProjectKey("/work/app"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	named, err := OpenFiles(root, "team")
	if err != nil {
		t.Fatal(err)
	}
	defer named.Close()
	for _, tc := range []struct {
		f    *Files
		path string
	}{{p, "work/app"}, {p, "/work/other"}, {p, ""}, {named, "/work/app"}} {
		if err := tc.f.SetProject(tc.path); err == nil {
			t.Errorf("SetProject(%q) on %s was accepted", tc.path, tc.f.dir)
		}
	}
	for _, key := range []string{ProjectKey("/work/app"), "team"} {
		if _, err := os.Stat(filepath.Join(root, key, "project")); !os.IsNotExist(err) {
			t.Errorf("a refused SetProject wrote %s: %v", filepath.Join(root, key, "project"), err)
		}
	}
}

func TestLockSharedAcrossRootSpellings(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	rel, err := OpenFiles("memory", "s")
	if err != nil {
		t.Fatal(err)
	}
	defer rel.Close()
	abs, err := OpenFiles(filepath.Join(base, "memory"), "s")
	if err != nil {
		t.Fatal(err)
	}
	defer abs.Close()
	if rel.mu != abs.mu {
		t.Fatal("one store opened by two spellings got two locks")
	}
	ctx := context.Background()
	for i := 0; i < MaxMemories-1; i++ {
		if err := rel.Put(ctx, Memory{Name: fmt.Sprintf("m%03d", i), Description: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, f := range []*Files{rel, abs} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.Put(ctx, Memory{Name: fmt.Sprintf("last%d", i), Description: "d"})
		}()
	}
	wg.Wait()
	full := 0
	for _, e := range errs {
		if errors.Is(e, ErrStoreFull) {
			full++
		}
	}
	if n, _ := rel.count(); n != MaxMemories || full != 1 {
		t.Fatalf("count = %d, ErrStoreFull results = %d, want %d and 1", n, full, MaxMemories)
	}
}

// hiddenRunes are characters a description could carry text in that a
// reader of the instructions does not see: controls, format
// characters, variation selectors, fillers and blanks that render as
// nothing.
var hiddenRunes = []rune{
	'\u0007', '\u0085', // Cc
	'\u200b', '\u202e', '\u2060', '\U000E0041', // Cf
	'\ufe00', '\ufe0f', '\U000E0100', '\U000E01EF', // variation selectors
	'\u115f', '\u1160', '\u3164', '\uffa0', // Hangul fillers
	'\u034f', '\u2800', '\u17b4', '\u17b5', '\u2028', '\u2029',
	'\u180b', '\u180d', '\u180f', // Mongolian variation selectors
}

// A description is visible single-line text: each hidden character is
// refused at Put, and nothing is written.
func TestPutRefusesHiddenRunesInDescription(t *testing.T) {
	f, _, dir := openTest(t)
	for _, r := range hiddenRunes {
		err := f.Put(context.Background(), Memory{Name: "n", Description: "a" + string(r) + "b", Body: "b"})
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("a description with %U = %v, want ErrInvalidName", r, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "n.md")); !os.IsNotExist(err) {
		t.Error("a refused description was written")
	}
	for _, desc := range []string{"café déjà vu", "Ελληνικά σημειώσεις", "日本語のメモ", "tabs\tare not", "€ 5, 50% (ok)!",
		"บันทึกภาษาไทย", "हिन्दी टिप्पणियाँ", "ភាសាខ្មែរ", "שָׁלוֹם עוֹלָם"} {
		err := f.Put(context.Background(), Memory{Name: "v", Description: desc, Body: "b"})
		if desc == "tabs\tare not" {
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("a description with a tab = %v, want ErrInvalidName", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("description %q was refused: %v", desc, err)
		}
	}
}

// An emoji presentation selector and a zero-width joiner are what a hidden
// instruction can ride on, so the sequences that carry them are refused.
func TestPutRefusesEmojiSequencesInDescription(t *testing.T) {
	f, _, _ := openTest(t)
	for _, desc := range []string{"⚠\ufe0f careful", "family \U0001F468\u200d\U0001F469"} {
		err := f.Put(context.Background(), Memory{Name: "n", Description: desc, Body: "b"})
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("description %q = %v, want ErrInvalidName", desc, err)
		}
	}
	if err := f.Put(context.Background(), Memory{Name: "n", Description: "⚠ careful", Body: "b"}); err != nil {
		t.Errorf("a plain symbol was refused: %v", err)
	}
}
