package creds

import (
	"os"
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

func TestHostCredentialParsing(t *testing.T) {
	// The real keychain blob wraps the credential in an envelope, so the
	// fields are found by name at any depth rather than by a configured path.
	blob := []byte(`{"claudeAiOauth":{"accessToken":"tok-123","expiresAt":1700000000000,
	  "refreshToken":"r","scopes":["a"]}}`)
	tok, ok := findString(blob, "accessToken")
	if !ok || tok != "tok-123" {
		t.Errorf("token = %q, %v", tok, ok)
	}
	exp, ok := findNumber(blob, "expiresAt")
	if !ok || exp != 1700000000000 {
		t.Errorf("expiry = %d, %v", exp, ok)
	}

	c := &HostCredential{ExpiresAt: 1700000000000}
	if !c.Expired(1700000000001) {
		t.Error("a past expiry did not read as expired")
	}
	if c.Expired(1699999999999) {
		t.Error("a future expiry read as expired")
	}
	// Absence of an expiry is not evidence of expiry.
	if (&HostCredential{}).Expired(1700000000000) {
		t.Error("a blob with no expiry read as expired")
	}
}
