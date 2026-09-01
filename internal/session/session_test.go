package session

import (
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// The registry is built at run time now rather than being a package-level
// literal, so a test that looks a profile up has to load the built-ins the way
// main does. No test here writes to it, so once for the package is enough.
func TestMain(m *testing.M) {
	if err := profile.Load(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// The cases are the ones the bash wrapper's own test suite ran
// (script/wrapper-test.sh in nofireai/homebrew-nofire), so a behaviour
// difference between the two implementations shows up here.
func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo", "foo"},
		{"Foo", "foo"},                           // lowercased
		{"My Big Refactor", "my-big-refactor"},   // spaces become dashes
		{"feature/JIRA-123", "feature-jira-123"}, // slash is not a path separator here
		{"-foo", "foo"},                          // leading dash would read as a flag
		{"../../etc", "etc"},                     // no traversal survives
		{"...", ""},                              // nothing usable
		{"", ""},                                 //
		{"ünïcode", "n-code"},                    // non-ASCII becomes dashes
		{"abcdefghi-jkl", "abcdefghi-jkl"},       // nothing is cut, so nothing to re-trim
		{"a---b", "a-b"},                         // dashes collapse
		{"a___b", "a___b"},                       // underscore survives, so my_project
		{"Desktop!", "desktop"},                  // and my-project stay separate
	}
	for _, c := range cases {
		if got := Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	if slug, err := Resolve("foo"); err != nil || slug != "foo" {
		t.Errorf(`Resolve("foo") = %q, %v; want "foo", nil`, slug, err)
	}
	// "claude-desktop" is in this list now. It used to be cut to "claude-des",
	// a workspace of its own; with nothing cut it slugs to the Desktop
	// profile's own workspace, so it is refused like every other spelling of
	// it.
	for _, bad := range []string{"...", "", "desktop", "Desktop!", "claude-desktop"} {
		if _, err := Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) succeeded; want an error", bad)
		}
	}
}

// Slug does not truncate. The ten-character budget it used to spend bought a
// short directory name and paid for it by mapping two long names onto one
// directory -- which is the collision the budget was then blamed for. The only
// ceiling left is the filesystem's own limit on a path component.
func TestSlugDoesNotTruncate(t *testing.T) {
	for _, long := range []string{
		"a-really-rather-long-session-name-for-one-refactor",
		// Length alone, with nothing in it to sanitise: what is asserted is
		// that no budget is spent, not that some character survived.
		strings.Repeat("a", 200),
	} {
		if got := Slug(long); got != long {
			t.Errorf("Slug of %d characters came back as %q (%d)", len(long), got, len(got))
		}
	}
}

// LegacySlug is what an older release derived, and it has exactly one caller:
// the migration notice, which has to name the directory that release left
// behind. So it cuts where Slug no longer does, and agrees with Slug on every
// name short enough never to have been cut -- which is how a caller tells a
// session whose home has moved from one whose has not.
func TestLegacySlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo", "foo"},
		{"exactlyten", "exactlyten"},      // at the old budget, so never cut
		{"refactoring", "refactorin"},     // one character past it
		{"My Big Refactor", "my-big-ref"}, //
		{"abcdefghi-jkl", "abcdefghi"},    // the cut left a trailing dash, trimmed
		{"...", ""},                       // nothing usable, cut or not
	}
	for _, c := range cases {
		if got := LegacySlug(c.in); got != c.want {
			t.Errorf("LegacySlug(%q) = %q, want %q", c.in, got, c.want)
		}
		// The pair the notice is decided on: a name the old budget left alone
		// slugs the same under both, and nothing is said about it.
		if len(Slug(c.in)) <= 10 && LegacySlug(c.in) != Slug(c.in) {
			t.Errorf("LegacySlug(%q) disagrees with Slug on a name that was never cut", c.in)
		}
	}
}
