package session

import "testing"

// The cases are the ones the bash wrapper's own test suite ran
// (script/wrapper-test.sh in nofireai/homebrew-nofire), so a behaviour
// difference between the two implementations shows up here.
func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo", "foo"},
		{"Foo", "foo"},                     // lowercased
		{"My Big Refactor", "my-big-ref"},  // spaces, truncated to 10
		{"feature/JIRA-123", "feature-ji"}, // slash is not a path separator here
		{"-foo", "foo"},                    // leading dash would read as a flag
		{"../../etc", "etc"},               // no traversal survives
		{"...", ""},                        // nothing usable
		{"", ""},                           //
		{"ünïcode", "n-code"},              // non-ASCII becomes dashes
		{"exactlyten", "exactlyten"},       // exactly at the budget
		{"abcdefghi-jkl", "abcdefghi"},     // truncation leaves no trailing dash
		{"a---b", "a-b"},                   // dashes collapse
		{"a___b", "a___b"},                 // underscore survives, so my_project
		{"Desktop!", "desktop"},            // and my-project stay separate
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
	// "claude-desktop" is not in this list: it slugs to "claude-des", which
	// lands on a workspace of its own rather than on the Desktop template's.
	for _, bad := range []string{"...", "", "desktop", "Desktop!"} {
		if _, err := Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) succeeded; want an error", bad)
		}
	}
}
