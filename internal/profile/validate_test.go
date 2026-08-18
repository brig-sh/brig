package profile

import (
	"strings"
	"testing"
)

// Exactly one of value: or ref:. Both is a contradiction with no safe
// precedence rule; neither is a binding that binds nothing.
func TestEnvBindingNeedsExactlyOneSource(t *testing.T) {
	both := bindingBase + "env:\n  - name: V\n    value: literal\n    ref: env.V\n"
	if _, err := Parse([]byte(both)); err == nil {
		t.Error("a binding with both value and ref was accepted")
	}
	neither := bindingBase + "env:\n  - name: V\n"
	if _, err := Parse([]byte(neither)); err == nil {
		t.Error("a binding with neither value nor ref was accepted")
	}
}

// The requirement list stays complete: a secrets. ref outside secrets: would
// never be counted by the up-front check, so the missing-secret error could not
// name it and the run would fail later and far less legibly.
func TestSecretRefMustBeDeclared(t *testing.T) {
	_, err := Parse([]byte(bindingBase + "env:\n  - name: V\n    ref: secrets.undeclared\n"))
	if err == nil {
		t.Fatal("a ref to an undeclared secret was accepted")
	}
	if !strings.Contains(err.Error(), "undeclared") || !strings.Contains(err.Error(), "secrets:") {
		t.Errorf("the error does not say what to add where: %v", err)
	}
}

// An env. ref needs no declaration: brig's own environment is not a list brig
// maintains.
func TestEnvRefNeedsNoDeclaration(t *testing.T) {
	if _, err := Parse([]byte(bindingBase + "env:\n  - name: V\n    ref: env.SOMETHING\n")); err != nil {
		t.Errorf("an env. ref was rejected: %v", err)
	}
}

// A malformed ref is caught at parse time, so nothing downstream has to decide
// what to do with one.
func TestMalformedRefIsRejectedAtParse(t *testing.T) {
	_, err := Parse([]byte(bindingBase + "env:\n  - name: V\n    ref: vault.token\n"))
	if err == nil {
		t.Fatal("a malformed ref was accepted")
	}
	if !strings.Contains(err.Error(), "V") {
		t.Errorf("the error does not name the binding: %v", err)
	}
}

// A secret name is the keychain account and the tail of a ref at once, so the
// profile is held to the grammar the store already enforces.
func TestDeclaredSecretNamesAreValidated(t *testing.T) {
	if _, err := Parse([]byte(bindingBase + "secrets:\n  - 'not a name'\n")); err == nil {
		t.Error("an invalid secret name was accepted")
	}
}

func TestDuplicateBindingsAreRejected(t *testing.T) {
	dupSecret := bindingBase + "secrets:\n  - a\n  - a\n"
	if _, err := Parse([]byte(dupSecret)); err == nil {
		t.Error("a duplicated secret requirement was accepted")
	}
	dupEnv := bindingBase + "env:\n  - name: V\n    ref: env.A\n  - name: V\n    ref: env.B\n"
	if _, err := Parse([]byte(dupEnv)); err == nil {
		t.Error("two bindings for one variable were accepted")
	}
}

// The same contradiction forward+deny was already an error for: a profile that
// binds a variable it also refuses to forward. This is the billing guard.
func TestBindingOnTheDenylistIsRejected(t *testing.T) {
	_, err := Parse([]byte(bindingBase +
		"deny:\n  - ANTHROPIC_API_KEY\nenv:\n  - name: ANTHROPIC_API_KEY\n    ref: env.ANTHROPIC_API_KEY\n"))
	if err == nil {
		t.Fatal("a binding for a denied variable was accepted")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

// A name that would not survive an environment block at all.
func TestEnvNamesAreChecked(t *testing.T) {
	for _, bad := range []string{"", "A=B", "A B"} {
		blob := bindingBase + "env:\n  - name: '" + bad + "'\n    value: x\n"
		if _, err := Parse([]byte(blob)); err == nil {
			t.Errorf("env name %q was accepted", bad)
		}
	}
}

// Each of these is a mistake a profile author makes
// once; the point of checking at parse time is that they find out at import
// rather than at the run that needed the credential.
func TestSecretSourceRules(t *testing.T) {
	no := false
	cases := []struct {
		name string
		p    Profile
		want string
	}{
		{"unknown from", Profile{Secrets: []SecretDecl{{Name: "s", From: "vault", Var: "X"}}}, "vault"},
		{"both spellings", Profile{Secrets: []SecretDecl{{
			Name: "s", From: SourceEnv, Var: "X",
			Sources: []Source{{From: SourceEnv, Var: "X"}}}}}, "sources"},
		{"empty sources", Profile{Secrets: []SecretDecl{{Name: "s", Sources: []Source{}}}}, "empty"},
		{"wrong locator", Profile{Secrets: []SecretDecl{{
			Name: "s", Sources: []Source{{From: SourceKeychain, Path: "/x"}}}}}, "path"},
		{"missing locator", Profile{Secrets: []SecretDecl{{
			Name: "s", Sources: []Source{{From: SourceKeychain}}}}}, "service"},
		{"optional is fine", Profile{Secrets: []SecretDecl{{Name: "s", Required: &no}}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.validateBindings()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("valid profile rejected: %v", err)
			case c.want == "":
			case err == nil:
				t.Fatalf("accepted, want an error naming %q", c.want)
			case !strings.Contains(err.Error(), c.want):
				t.Errorf("error %v does not name %q", err, c.want)
			}
		})
	}
}

// Binding one secret through both channels is legal, and sometimes correct.
// TestOneSecretMayTakeBothChannels in volumes_test.go pins it, now that files:
// exists to bind through.

func TestChainRules(t *testing.T) {
	p := Profile{
		Secrets: []SecretDecl{{Name: "gh-token"}},
		Env:     []EnvBinding{{Name: "GH_TOKEN", Ref: "env.GH_TOKEN", Refs: []string{"secrets.gh-token"}}},
	}
	if err := p.validateBindings(); err == nil || !strings.Contains(err.Error(), "refs") {
		t.Errorf("err = %v; want one naming refs", err)
	}
}
