package creds

import (
	"testing"

	"github.com/brig-sh/brig/internal/agent"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestForwardSkipsUnsetAndEmpty(t *testing.T) {
	tmpl, _ := agent.Lookup("claude-code")
	set := Forward(tmpl, []string{"A", "B", "C"}, lookupFrom(map[string]string{
		"A": "value",
		"B": "", // set but empty: forwarding it would shadow a value in the image
	}), Options{})
	if len(set.Vars) != 1 || set.Vars[0].Name != "A" {
		t.Fatalf("forwarded %+v, want only A", set.Vars)
	}
}

func TestForwardRefusesDeniedVars(t *testing.T) {
	tmpl, _ := agent.Lookup("claude-code")
	env := lookupFrom(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-xxx"})

	set := Forward(tmpl, []string{"ANTHROPIC_API_KEY"}, env, Options{})
	if len(set.Vars) != 0 {
		t.Errorf("forwarded a denied variable: %+v", set.Vars)
	}
	if len(set.Warnings) != 1 {
		t.Errorf("dropped a denied variable silently: %v", set.Warnings)
	}

	// Overriding the denylist is deliberate, and possible.
	set = Forward(tmpl, []string{"ANTHROPIC_API_KEY"}, env, Options{AllowDenied: true})
	if len(set.Vars) != 1 {
		t.Errorf("BRIG_ALLOW_DENIED did not forward it: %+v", set.Vars)
	}
}

// A secret manager expresses a reference as scheme://..., and direnv and
// friends readily leave one in the ambient environment unresolved. Forwarded
// verbatim it yields "Invalid username or token" in the guest, which is
// indistinguishable from a wrong username or a broken helper.
func TestForwardRejectsUnresolvedReferences(t *testing.T) {
	tmpl, _ := agent.Lookup("claude-code")
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
		set := Forward(tmpl, []string{"GH_TOKEN"},
			lookupFrom(map[string]string{"GH_TOKEN": c.value}), Options{})
		if got := len(set.Vars) == 1; got != c.forwards {
			t.Errorf("value %q forwarded = %v, want %v", c.value, got, c.forwards)
		}
	}
	// The escape hatch forwards anything.
	set := Forward(tmpl, []string{"GH_TOKEN"},
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
