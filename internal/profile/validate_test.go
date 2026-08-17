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
