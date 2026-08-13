package config

import (
	"strings"
	"testing"
)

// env builds a lookup over a fixed map. A key present with an empty value is
// set-but-empty, which ${VAR:-d} and ${VAR-d} treat differently.
func env(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := pairs[name]
		return v, ok
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{
		"SET":   "value",
		"EMPTY": "",
	}
	tests := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{name: "no expression", in: "plain text", want: "plain text"},
		{name: "whole value", in: "${SET}", want: "value"},
		{name: "embedded", in: "pre-${SET}-post", want: "pre-value-post"},
		{name: "twice", in: "${SET}/${SET}", want: "value/value"},

		// ${VAR} on a set-but-empty variable is an empty value, not an error.
		{name: "set but empty", in: "${EMPTY}", want: ""},
		// Unset with no default is an error: a security control silently
		// resolving to empty is a fail-open.
		{name: "unset no default", in: "${MISSING}", bad: true},
		{name: "unset no default embedded", in: "a${MISSING}b", bad: true},

		{name: "default unused", in: "${SET:-fallback}", want: "value"},
		{name: "default used when unset", in: "${MISSING:-fallback}", want: "fallback"},
		// :- treats empty as absent; - does not.
		{name: "default used when empty", in: "${EMPTY:-fallback}", want: "fallback"},
		{name: "dash default keeps empty", in: "${EMPTY-fallback}", want: ""},
		{name: "dash default when unset", in: "${MISSING-fallback}", want: "fallback"},
		{name: "explicit empty default", in: "${MISSING:-}", want: ""},
		{name: "default containing dash", in: "${MISSING:-a-b}", want: "a-b"},
		{name: "default containing colon", in: "${MISSING:-http://x}", want: "http://x"},

		{name: "escaped dollar", in: "$$", want: "$"},
		{name: "escaped then expression", in: "$${SET}", want: "${SET}"},
		{name: "lone dollar is literal", in: "cost $5", want: "cost $5"},
		{name: "trailing dollar", in: "x$", want: "x$"},

		// A dash is the ${VAR-default} separator, as in the shell, so a dashed
		// expression is a variable plus a default rather than a bad name. Pinned
		// because it is surprising until you see it written down.
		{name: "dash reads as a default, shell-style", in: "${SET-a-b}", want: "value"},
		{name: "dash default on unset var", in: "${not-a-name}", want: "a-name"},

		{name: "unterminated", in: "${SET", bad: true},
		{name: "bad name with a dot", in: "${not.a.name}", bad: true},
		{name: "bad name with a space", in: "${bad name}", bad: true},
		{name: "empty name", in: "${}", bad: true},
		{name: "leading digit name", in: "${1X}", bad: true},
		// One level only: a default may not itself expand.
		{name: "nested default", in: "${MISSING:-${SET}}", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.in, env(vars))
			if tc.bad {
				if err == nil {
					t.Fatalf("Expand(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Expand(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandErrorNamesTheVariable(t *testing.T) {
	// The error is the only place a user learns which variable to export, so
	// the name has to be in it.
	_, err := Expand("${ACME_TOKEN}", env(nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ACME_TOKEN") {
		t.Errorf("error %q does not name the variable", err)
	}
}
