// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	d, err := ParseDeployment("https://h")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Clean(d, "bad\r\nLogged in to x\x1b[31m"), "https://h said: badLogged in to x[31m"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := Clean(d, strings.Repeat("a", 500))
	if want := "https://h said: " + strings.Repeat("a", 200); got != want {
		t.Errorf("long input: got %d bytes", len(got))
	}
}

func TestCleanLine(t *testing.T) {
	if got := CleanLine("a\x00b\x07c\x7fd\te\nf"); got != "abcdef" {
		t.Errorf("controls: %q", got)
	}
	if got := CleanLine("héllo wörld"); got != "héllo wörld" {
		t.Errorf("printable non-ASCII changed: %q", got)
	}
	// Two-byte runes: a byte cut at 200 would split one in half.
	got := CleanLine(strings.Repeat("é", 300))
	if n := utf8.RuneCountInString(got); n != 200 || !utf8.ValidString(got) {
		t.Errorf("runes = %d, valid = %v", n, utf8.ValidString(got))
	}
	if got := CleanLine(strings.Repeat("a\n", 300)); len(got) != 200 {
		t.Errorf("controls count toward the cap: %d", len(got))
	}
	for in, want := range map[string]string{
		"\u009b31m":      "31m",
		"\u202eevil":     "evil",
		"a\u2066b\u2069": "ab",
		"a\u2028b\u2029": "ab",
		"a\u0085b":       "ab",
		"a b":            "a b",
		"a\xffb":         "ab",
		"\xff":           "",
	} {
		if got := CleanLine(in); got != want {
			t.Errorf("CleanLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanLine(""); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestCleanMessage(t *testing.T) {
	if got := CleanMessage("a\x1b[2K\r\nb"); got != "a[2Kb" {
		t.Errorf("controls: %q", got)
	}
	// Wider than CleanLine, so advice after a cut server string survives,
	// but still bounded.
	long := strings.Repeat("a", 300)
	if got := CleanMessage(long); got != long {
		t.Errorf("cut a 300-character message to %d", len(got))
	}
	if got := CleanMessage(strings.Repeat("a", 5000)); len(got) != 1000 {
		t.Errorf("unbounded: %d characters", len(got))
	}
}
