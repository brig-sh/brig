package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectKeyCleansPath(t *testing.T) {
	a := ProjectKey("/a/b/")
	if a != ProjectKey("/a/b") || a != ProjectKey("/a/./b") {
		t.Fatalf("equivalent paths gave different keys: %q", a)
	}
	if a == ProjectKey("/a/c") {
		t.Fatal("different paths gave the same key")
	}
	if !strings.HasPrefix(a, "project-") || len(a) != len("project-")+16 {
		t.Fatalf("key = %q", a)
	}
}

func TestValidStoreNameRefusesProjectPrefix(t *testing.T) {
	for _, n := range []string{"project-abc", "project-", "", ".", "..", ".x", "A", "a/b", "../x", strings.Repeat("a", 65)} {
		if err := ValidStoreName(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidStoreName(%q) = %v, want ErrInvalidName", n, err)
		}
	}
	for _, n := range []string{"team", "a.b_c-d", "projects", strings.Repeat("a", 64)} {
		if err := ValidStoreName(n); err != nil {
			t.Errorf("ValidStoreName(%q) = %v", n, err)
		}
	}
	// ValidStoreName refuses the prefix, so a caller cannot name a project
	// store's directory by hand. OpenFiles takes a key, not a name: it
	// opens a well-formed project key and refuses anything else under the
	// prefix.
	if f, err := OpenFiles(t.TempDir(), "project-"+strings.Repeat("0", 16)); err != nil {
		t.Errorf("OpenFiles refused a project-shaped key: %v", err)
	} else {
		_ = f.Close()
	}
	if _, err := OpenFiles(t.TempDir(), "project-xyz"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("OpenFiles(project-xyz) = %v, want ErrInvalidName", err)
	}
}

func TestListAndRemove(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	ctx := context.Background()
	if got, err := List(root); err != nil || len(got) != 0 {
		t.Fatalf("List of a missing root = %+v, %v", got, err)
	}
	pk := ProjectKey("/work/app")
	p, err := OpenFiles(root, pk)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetProject("/work/app"); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, Memory{Name: "a", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, Memory{Name: "b", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	s, err := OpenFiles(root, "team")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put(ctx, Memory{Name: "x", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	// Neither MEMORY.md nor an unparseable file is a memory.
	if err := os.WriteFile(filepath.Join(root, "team", "junk.md"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []StoreInfo{{Key: pk, Project: "/work/app", Memories: 2}, {Key: "team", Memories: 1}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
	if err := Remove(root, "../x"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Remove(../x) = %v, want ErrInvalidName", err)
	}
	if err := Remove(root, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove of a missing store = %v, want ErrNotFound", err)
	}
	if err := Remove(root, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "team")); !os.IsNotExist(err) {
		t.Fatal("store still there")
	}
	got, _ = List(root)
	if len(got) != 1 || got[0].Key != pk {
		t.Fatalf("List after Remove = %+v", got)
	}
}

func TestRemoveRefusesLinkedStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "link"); err == nil {
		t.Error("Remove followed a symlinked store")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file outside the store was removed: %v", err)
	}
	if got, _ := List(root); len(got) != 0 {
		t.Errorf("List listed a symlinked store: %+v", got)
	}
}

// The words a switch is written with are not store names, in any case: a
// user who writes memory off must not get a store called off shared by
// every run that wrote it.
func TestValidStoreNameRefusesSwitchWords(t *testing.T) {
	for _, n := range []string{"true", "false", "yes", "no", "on", "off", "0", "1", "none"} {
		if err := ValidStoreName(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidStoreName(%q) = %v, want ErrInvalidName", n, err)
		}
	}
}

// A setting is off, the project's store or a store name, the same way from
// every source.
func TestParseSetting(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"false", ""}, {"no", ""}, {"off", ""}, {"0", ""}, {"none", ""}, {"OFF", ""}, {"No", ""},
		{"true", "project"}, {"yes", "project"}, {"on", "project"}, {"1", "project"}, {"TRUE", "project"},
		{"project", "project"}, {"team", "team"},
	} {
		if got, err := ParseSetting(tc.in); err != nil || got != tc.want {
			t.Errorf("ParseSetting(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "Team", "project-x", "../x", "a b"} {
		if _, err := ParseSetting(bad); err == nil {
			t.Errorf("ParseSetting(%q) was accepted", bad)
		}
	}
}

// OpenExisting opens a store and never creates one: a store removed under a
// registration stays removed, rather than coming back empty.
func TestOpenExistingDoesNotCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if _, err := OpenExisting(root, "team"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenExisting of a missing root = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("OpenExisting created the root: %v", err)
	}
	f, err := OpenFiles(root, "team")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	g, err := OpenExisting(root, "team")
	if err != nil {
		t.Fatalf("OpenExisting of a store: %v", err)
	}
	_ = g.Close()
	if _, err := OpenExisting(root, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenExisting of a missing store = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(root, "other")); !os.IsNotExist(err) {
		t.Errorf("OpenExisting created the store: %v", err)
	}
	if _, err := OpenExisting(root, "../x"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("OpenExisting(../x) = %v, want ErrInvalidName", err)
	}
}
