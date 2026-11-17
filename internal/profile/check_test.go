package profile

import (
	"os"
	"strings"
	"testing"
)

// Check refuses each file Import refuses, with the same words, and writes
// nothing: it is the answer an author wants before the file travels, so it has
// to be the answer Import would give, not a weaker one.
func TestCheckRefusesWhatImportRefusesAndWritesNothing(t *testing.T) {
	reset(t)
	for _, tc := range []struct {
		name string
		blob string
		want string
	}{
		{"a name that collides with a reserved profile", "name: desktop\n" + versionFields, "collides"},
		{"a name brig's own config file would shadow", "name: config\n" + versionFields, "is reserved"},
		{"a file that does not parse", "name: x\nbogus: true\n" + versionFields, "unknown field"},
		{"a file that is not valid", "name: Bad Name\n" + versionFields, "may use only lowercase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Check([]byte(tc.blob))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v, want an error containing %q", err, tc.want)
			}
			_, _, importErr := Import([]byte(tc.blob), dir)
			if importErr == nil || importErr.Error() != err.Error() {
				t.Errorf("Import = %v, but Check said %v: the two must refuse the same file in the same words", importErr, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a refused file left %d entries in the profile directory", len(entries))
			}
		})
	}
}

// A file Check accepts is one Import takes, and Check puts nothing on disk:
// the profile directory Import would create is still not there afterwards.
func TestCheckAcceptsWhatImportAcceptsWithoutTouchingTheDisk(t *testing.T) {
	reset(t)
	dir := t.TempDir() + "/not-yet"
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: mine\n" + versionFields)

	p, err := Check(blob)
	if err != nil {
		t.Fatalf("Check = %v", err)
	}
	if p.Name != "mine" {
		t.Errorf("Check read the name %q, want mine", p.Name)
	}
	if _, err := os.Stat(Dir()); !os.IsNotExist(err) {
		t.Fatalf("Check touched the profile directory %s: %v", Dir(), err)
	}
	if _, _, err := Import(blob, Dir()); err != nil {
		t.Fatalf("Import refused what Check accepted: %v", err)
	}
}
