package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeSecretFile writes value at dir/name with mode, set after the write so
// the umask does not decide it.
func writeSecretFile(t *testing.T, dir, name string, value []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, value, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func openFiles(t *testing.T, dir string) *Files {
	t.Helper()
	f, err := OpenFiles(dir)
	if err != nil {
		t.Fatalf("OpenFiles(%s): %v", dir, err)
	}
	return f
}

func listNames(t *testing.T, f *Files) []string {
	t.Helper()
	list, err := f.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names
}

func TestFilesReadsAValueVerbatim(t *testing.T) {
	dir := t.TempDir()
	want := []byte("ghp_fake-token\n")
	writeSecretFile(t, dir, "gh-token", want, 0o400)
	f := openFiles(t, dir)
	if f.Kind() != "files" {
		t.Fatalf("Kind() = %q, want files", f.Kind())
	}
	got, err := f.Read("gh-token")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Read = %q, want %q byte for byte", got, want)
	}
}

func TestFilesReadOfAMissingSecretIsNotFound(t *testing.T) {
	f := openFiles(t, t.TempDir())
	if _, err := f.Read("gh-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read of a missing secret: %v, want ErrNotFound", err)
	}
}

func TestFilesRefusesEveryWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretFile(t, dir, "gh-token", []byte("before"), 0o400)
	f := openFiles(t, dir)
	for what, err := range map[string]error{
		"Create":         f.Create("new-token", []byte("x")),
		"Update":         f.Update("gh-token", []byte("after")),
		"Delete":         f.Delete("gh-token"),
		"Create journal": f.Create(JournalName("gh-token"), []byte("{}")),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", what, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before" {
		t.Fatalf("the file holds %q after the refused writes", got)
	}
	for _, name := range []string{"new-token", JournalName("gh-token")} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused write left %s behind: %v", name, err)
		}
	}
}

func TestFilesRefusesAWorldReadableFileAtOpen(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretFile(t, dir, "gh-token", []byte("x"), 0o644)
	_, err := OpenFiles(dir)
	if err == nil {
		t.Fatal("a world-readable secret opened")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("the error does not name the file and its mode: %v", err)
	}

	group := t.TempDir()
	writeSecretFile(t, group, "gh-token", []byte("x"), 0o640)
	openFiles(t, group)
}

func TestFilesSkipsNamesOutsideTheGrammar(t *testing.T) {
	dir := t.TempDir()
	writeSecretFile(t, dir, "gh-token", []byte("x"), 0o400)
	writeSecretFile(t, dir, "ca.crt", []byte("pem"), 0o400)
	writeSecretFile(t, dir, "9lives", []byte("x"), 0o400)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "..2026_01_01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_01_01", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}

	f := openFiles(t, dir)
	if got, want := f.Skipped(), []string{"9lives", "ca.crt", "sub"}; !slices.Equal(got, want) {
		t.Fatalf("Skipped() = %q, want %q", got, want)
	}
	for _, name := range listNames(t, f) {
		if ValidName(name) != nil {
			t.Fatalf("List names %q, which is outside the grammar", name)
		}
	}
	if _, err := f.Read("ca.crt"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(ca.crt) = %v, want the secret-name error", err)
	}
}

// A link in the directory that points outside it is refused when it is read,
// and the file it points at is neither read nor changed. The kubelet's own
// links stay inside the directory; this one was planted after the store
// opened.
func TestFilesRefusesALinkOutOfTheDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeSecretFile(t, outside, "gh-token", []byte("ghp_outside"), 0o440)
	before, err := os.Stat(filepath.Join(outside, "gh-token"))
	if err != nil {
		t.Fatal(err)
	}
	f := openFiles(t, dir)
	if err := os.Symlink(filepath.Join(outside, "gh-token"), filepath.Join(dir, "gh-token")); err != nil {
		t.Fatal(err)
	}
	got, err := f.Read("gh-token")
	if err == nil {
		t.Fatalf("Read through a link out of the directory = %q, want an error", got)
	}
	if strings.Contains(err.Error(), "ghp_outside") || string(got) == "ghp_outside" {
		t.Fatalf("the value outside the directory leaked: %v", err)
	}
	after, err := os.Stat(filepath.Join(outside, "gh-token"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("the file outside changed: %v -> %v", before.Mode(), after.Mode())
	}
}

func TestFilesFollowsTheKubeletSymlinkLayout(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "..2026")
	if err := os.Mkdir(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSecretFile(t, stamp, "gh-token", []byte("ghp_fake"), 0o440)
	if err := os.Symlink("..2026", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "gh-token"), filepath.Join(dir, "gh-token")); err != nil {
		t.Fatal(err)
	}

	f := openFiles(t, dir)
	got, err := f.Read("gh-token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ghp_fake" {
		t.Fatalf("Read through the kubelet links = %q", got)
	}
	if names := listNames(t, f); !slices.Equal(names, []string{"gh-token"}) {
		t.Fatalf("List = %q, want [gh-token]", names)
	}
}

func TestFilesRefusesADanglingSecretLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "gh-token")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenFiles(dir)
	if err == nil {
		t.Fatal("a dangling link to a secret opened")
	}
	if !strings.Contains(err.Error(), "gh-token") {
		t.Fatalf("the error does not name the entry: %v", err)
	}

	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(outside, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	f := openFiles(t, outside)
	if got := f.Skipped(); !slices.Equal(got, []string{"ca.crt"}) {
		t.Fatalf("Skipped() = %q, want [ca.crt]", got)
	}
}
