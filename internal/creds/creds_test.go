package creds

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

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// A secret manager expresses a reference as scheme://..., and direnv and
// friends readily leave one in the ambient environment unresolved. Forwarded
// verbatim it yields "Invalid username or token" in the guest, which is
// indistinguishable from a wrong username or a broken helper.
//
// What counts as one is the part bind_test.go's single case does not reach: an
// ordinary URL is a value somebody may legitimately be forwarding, so the guard
// has to tell the two apart rather than refusing anything with a scheme.
func TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot(t *testing.T) {
	tmpl, _ := profile.Lookup("claude-code")
	bindings := []profile.EnvBinding{{Name: "GH_TOKEN", Ref: "env.GH_TOKEN"}}
	cases := []struct {
		value    string
		forwards bool
	}{
		{"op://vault/item/field", false},
		{"vault://secret/token", false},
		{"https://example.com/callback", true}, // an ordinary URL is not a reference
		{"http://example.com", true},
		{"ghp_realtokenvalue", true},
	}
	for _, c := range cases {
		set := Bind(tmpl, bindings, nil,
			lookupFrom(map[string]string{"GH_TOKEN": c.value}), Options{})
		if got := len(set.Vars) == 1; got != c.forwards {
			t.Errorf("value %q forwarded = %v, want %v", c.value, got, c.forwards)
		}
	}
	// The escape hatch forwards anything.
	set := Bind(tmpl, bindings, nil,
		lookupFrom(map[string]string{"GH_TOKEN": "op://vault/item"}), Options{AllowRefs: true})
	if len(set.Vars) != 1 {
		t.Error("BRIG_ALLOW_REFS did not forward the reference")
	}
}

// The warning about an unresolved reference quotes its scheme, and everything
// before :// is part of the shell value. A token with :// after it would be
// echoed into the warning, and into `brig plan --json`, which carries the
// warning as a reason. Only a scheme spelled like one is quoted.
func TestAnUnresolvedReferenceQuotesOnlyAPlainScheme(t *testing.T) {
	tmpl, _ := profile.Lookup("claude-code")
	bindings := []profile.EnvBinding{{Name: "TOK", Ref: "env.TOK"}}
	for _, c := range []struct {
		value, quoted, hidden string
	}{
		{"op://vault/item/field", "(op://...)", ""},
		{"vault+kv2.v1://secret", "(vault+kv2.v1://...)", ""},
		{"ghp_REALTOKENPREFIX://x", "", "ghp_REALTOKENPREFIX"},
		{"Op://vault/item", "", "Op"},
		{"9op://vault/item", "", "9op"},
	} {
		lookup := lookupFrom(map[string]string{"TOK": c.value})
		set := Bind(tmpl, bindings, nil, lookup, Options{})
		if len(set.Vars) != 0 || len(set.Warnings) != 1 {
			t.Errorf("%q: forwarded %d, warned %d times, want it withheld with one warning",
				c.value, len(set.Vars), len(set.Warnings))
			continue
		}
		reason := Preview(tmpl, bindings, func(string) (bool, bool) { return false, true },
			lookup, Options{})[0].Reason
		for _, said := range []string{set.Warnings[0], reason} {
			if !strings.Contains(said, "unresolved secret reference") {
				t.Errorf("%q: the warning does not say what it is: %s", c.value, said)
			}
			if c.quoted != "" && !strings.Contains(said, c.quoted) {
				t.Errorf("%q: the warning does not quote %s: %s", c.value, c.quoted, said)
			}
			if c.hidden != "" && strings.Contains(said, c.hidden) {
				t.Errorf("%q: the warning echoes part of the value: %s", c.value, said)
			}
		}
	}
}

func TestSetReportsNamesNotPlumbing(t *testing.T) {
	var s Set
	s.Add("GH_TOKEN", "secret", "")
	s.AddPlumbing("GIT_TERMINAL_PROMPT", "0")
	if len(s.Names) != 1 || s.Names[0] != "GH_TOKEN" {
		t.Errorf("names = %v, want just GH_TOKEN", s.Names)
	}
	if !s.Has("GIT_TERMINAL_PROMPT") {
		t.Error("plumbing variable is not being forwarded")
	}
}
