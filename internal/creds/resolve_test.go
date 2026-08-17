package creds

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveSecretsReadsEveryDeclaredName(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n  - b\n")
	got, err := ResolveSecrets(p, "brig-x", fakeStore{"a": "one", "b": "two"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "one" || got["b"] != "two" {
		t.Errorf("resolved %v", got)
	}
}

// The message a user actually reads when a run cannot get its credential. It
// has to answer three questions without them having to ask: what is missing,
// what was it needed for, and what do I type to fix it.
func TestOneMissingSecretSaysWhatToDo(t *testing.T) {
	p := profileWith(t, "secrets:\n  - gh_token\n")
	_, err := ResolveSecrets(p, "brig-claude-code", fakeStore{})
	if err == nil {
		t.Fatal("a missing secret resolved")
	}
	got := err.Error()
	want := `missing secret "gh_token" needed by the brig-claude-code sandbox -- ` +
		"create it first with: brig secret create gh_token"
	if got != want {
		t.Errorf("error message is\n  %s\nwant\n  %s", got, want)
	}
}

// Collected rather than short-circuited: a fresh host is fixed in one pass
// instead of one failed run per secret.
func TestEveryMissingSecretIsNamedAtOnce(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n  - b\n  - c\n")
	_, err := ResolveSecrets(p, "brig-x", fakeStore{"b": "two"})
	if err == nil {
		t.Fatal("missing secrets resolved")
	}
	var missing *MissingSecretsError
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want *MissingSecretsError", err)
	}
	if len(missing.Names) != 2 || missing.Names[0] != "a" || missing.Names[1] != "c" {
		t.Fatalf("Names = %v, want [a c]", missing.Names)
	}
	got := err.Error()
	want := "missing 2 secrets needed by the brig-x sandbox -- create them first:" +
		"\n  brig secret create a" +
		"\n  brig secret create c"
	if got != want {
		t.Errorf("error message is\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "two") {
		t.Errorf("the error carries a secret value:\n%s", got)
	}
}

// cmd/brig prints "brig: " itself, so an error carrying its own would print it
// twice.
func TestTheErrorDoesNotCarryTheBrigPrefix(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n")
	_, err := ResolveSecrets(p, "brig-x", fakeStore{})
	if strings.HasPrefix(err.Error(), "brig: ") {
		t.Errorf("the error carries the prefix cmd/brig adds: %v", err)
	}
}

// A profile that needs nothing never touches the store, so a run with no
// secrets raises no keychain prompt.
func TestResolveSecretsWithNoRequirementsNeverReads(t *testing.T) {
	p := profileWith(t, "forward:\n  - GH_TOKEN\n")
	got, err := ResolveSecrets(p, "brig-x", panicStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("resolved %v from a profile with no secrets", got)
	}
}

// A backend that fails for a reason other than absence is not a missing secret,
// and "create it first" would send the user the wrong way entirely.
func TestStoreFailureIsNotReportedAsMissing(t *testing.T) {
	p := profileWith(t, "secrets:\n  - a\n")
	_, err := ResolveSecrets(p, "brig-x", brokenStore{})
	if err == nil {
		t.Fatal("a broken store resolved")
	}
	var missing *MissingSecretsError
	if errors.As(err, &missing) {
		t.Errorf("a store failure was reported as a missing secret: %v", err)
	}
	if !strings.Contains(err.Error(), "keyring is locked") {
		t.Errorf("the backend's reason was lost: %v", err)
	}
}
