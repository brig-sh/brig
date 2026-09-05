package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reservation has to survive a file round trip, or exporting a profile and
// importing it back quietly drops it -- which was the behaviour before
// reserved was an exported field.
func TestReservedSurvivesARoundTrip(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	original, ok := Lookup("claude-desktop")
	if !ok {
		t.Fatal("claude-desktop is missing")
	}
	if !original.Reserved {
		t.Fatal("claude-desktop is not reserved, so this test proves nothing")
	}
	blob, err := Export(original)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := Import(blob, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Reserved {
		t.Error("the reservation did not survive export and import")
	}
	// The struct field surviving is not enough on its own: Go's JSON decoder
	// matches field names case-insensitively, so this would still pass with
	// the json:"reserved,omitempty" tag removed. Pin the lowercase key that
	// actually reaches the exported file.
	if !strings.Contains(string(blob), "reserved: true") {
		t.Errorf("exported profile does not contain %q:\n%s", "reserved: true", blob)
	}
}

// Reserved reads the merged set, so a profile of the user's own can declare
// itself reserved and have session names respect it.
func TestReservedReadsTheMergedSet(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\n" +
		"mem: 1\ncpus: 1\nreserved: true\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if owner, ok := Reserved("mine"); !ok || owner != "mine" {
		t.Errorf(`Reserved("mine") = %q, %v; want "mine", true`, owner, ok)
	}
	// And a built-in reservation still holds, including the trailing word a
	// slug of the name reads as.
	for _, slug := range []string{"claude-desktop", "desktop"} {
		if owner, ok := Reserved(slug); !ok || owner != "claude-desktop" {
			t.Errorf("Reserved(%q) = %q, %v; want claude-desktop, true", slug, owner, ok)
		}
	}
}

// Import refuses a name that collides with a *different* reserved profile.
// Taking a reserved profile's own name is the legitimate "pin my own image for
// a profile brig already knows" case and must still work -- once Reserved
// reads the merged set, the profile being imported is its own owner.
func TestImportAllowsOverridingAReservedProfile(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	blob := []byte("name: claude-desktop\nkind: gui\nimage: docker.io/me/d:latest\n" +
		"guestHome: /home/claude\nmem: 1\ncpus: 1\n")
	if _, _, err := Import(blob, dir); err != nil {
		t.Errorf("could not override a reserved built-in: %v", err)
	}
	// A name that is not claude-desktop's own but still reads as its
	// reserved slug -- "desktop" is the trailing word Reserved matches -- must
	// still be refused.
	collision := []byte("name: desktop\nimage: i\nguestHome: /home/desktop\n" +
		"binary: d\nmem: 1\ncpus: 1\n")
	_, _, err := Import(collision, dir)
	if err == nil {
		t.Fatal("expected an error importing a profile named desktop")
	}
	// "collides", not "profile": Validate's own errors mention a profile too,
	// so the looser word would keep this green if the blob ever started
	// failing validation instead of colliding.
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error %q does not report a collision", err.Error())
	}
}
