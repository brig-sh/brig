package hostsrc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// Absence is ordinary, and it is the idiom this whole design rests on: a
// source that holds nothing is how keychain-then-file falls through to the
// file without an OS predicate, and how a host that has never run the agent
// reports "no credential" rather than an error.
func TestAMissingFileIsOrdinary(t *testing.T) {
	r := NewReader()
	_, ok, err := r.Read(profile.Source{From: profile.SourceFile, Path: filepath.Join(t.TempDir(), "nope")})
	if err != nil {
		t.Fatalf("a missing file was an error: %v", err)
	}
	if ok {
		t.Error("a missing file reported a value")
	}
}

func TestFileSourceIsVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred.json")
	blob := []byte("{\"claudeAiOauth\":{\"accessToken\":\"tok\"}}\n")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	v, ok, err := NewReader().Read(profile.Source{From: profile.SourceFile, Path: path})
	if err != nil || !ok {
		t.Fatalf("Read = %v, %v", ok, err)
	}
	// Verbatim: the bytes ARE the format the agent's own file takes, and a
	// trailing newline the user's file carries belongs to the user's file.
	if string(v.Bytes) != string(blob) {
		t.Errorf("bytes = %q, want %q", v.Bytes, blob)
	}
	if v.From != "file:"+path {
		t.Errorf("From = %q", v.From)
	}
}

// The dialog economy: two secrets genuinely sharing one keychain item must
// raise one approval dialog, so one locator is read once per import.
func TestOneLocatorIsReadOnce(t *testing.T) {
	r := NewReader()
	reads := 0
	r.readFile = func(path string) ([]byte, error) { reads++; return []byte("v"), nil }
	src := profile.Source{From: profile.SourceFile, Path: "/same"}
	for i := 0; i < 3; i++ {
		if _, _, err := r.Read(src); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Errorf("read the same locator %d times", reads)
	}
}

// An empty environment variable is absent, not a value: exporting GH_TOKEN=
// is how a shell says "unset" often enough that storing an empty secret from
// it would be a bug with no symptom until the guest failed to authenticate.
func TestEmptyEnvIsAbsent(t *testing.T) {
	t.Setenv("BRIG_TEST_TOKEN", "")
	if _, ok, _ := NewReader().Read(profile.Source{From: profile.SourceEnv, Var: "BRIG_TEST_TOKEN"}); ok {
		t.Error("an empty variable reported a value")
	}
}

// No field: means store the document as it stands -- which is what makes a
// files: delivery lossless, and why there is no json: template.
func TestExtractWithoutAFieldIsVerbatim(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":1755436980000}}`)
	got, expiry, err := Extract(Value{Bytes: blob}, profile.SecretDecl{ExpiryField: "expiresAt"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(blob) {
		t.Errorf("value = %q; want the document unchanged", got)
	}
	if expiry != 1755436980000 {
		t.Errorf("expiry = %d", expiry)
	}
}

// field: extracts, because a secret bound to a VARIABLE must be the bare
// value: ref: secrets.<name> yields the whole stored value, so a document
// bound to a variable would forward an entire JSON blob as a token.
func TestExtractWithAFieldTakesTheBareValue(t *testing.T) {
	got, _, err := Extract(
		Value{Bytes: []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)},
		profile.SecretDecl{Field: "accessToken"})
	if err != nil || string(got) != "tok" {
		t.Errorf("value = %q, err = %v", got, err)
	}
}

// A field: that is not there is an error, not an empty secret: storing "" is
// how you get a secret that resolves and never authenticates.
func TestAMissingFieldIsAnError(t *testing.T) {
	if _, _, err := Extract(Value{Bytes: []byte(`{}`)}, profile.SecretDecl{Field: "accessToken"}); err == nil {
		t.Error("a missing field stored an empty value")
	}
}

// Absence and refusal are different answers. A fake readKeychain reporting
// errNoSuchItem is ordinary absence and falls through to the next source; one
// reporting a refusal (a denied dialog, a locked keychain) must fail the
// import naming the reason, and must NOT fall through -- falling through on a
// refusal is how the wrong hint ("run claude on the host once to log in")
// gets printed at somebody who already has a credential, just not one brig
// could read right now.
func TestKeychainAbsenceFallsThroughButRefusalFails(t *testing.T) {
	r := NewReader()
	r.readKeychain = func(service string) ([]byte, error) { return nil, errNoSuchItem }
	_, ok, err := r.Read(profile.Source{From: profile.SourceKeychain, Service: "svc"})
	if err != nil {
		t.Fatalf("absence was reported as an error: %v", err)
	}
	if ok {
		t.Error("absence reported a value")
	}

	r2 := NewReader()
	r2.readKeychain = func(service string) ([]byte, error) {
		return nil, fmt.Errorf("the user clicked Deny")
	}
	_, ok, err = r2.Read(profile.Source{From: profile.SourceKeychain, Service: "svc"})
	if err == nil {
		t.Fatal("a denied dialog was reported as absence")
	}
	if ok {
		t.Error("a refusal reported a value")
	}
}

// An empty keychain item -- security -w printing nothing but its own
// newline -- is absence too: there is no way to store an empty credential on
// purpose, so an empty read is indistinguishable from "not there".
func TestEmptyKeychainValueIsAbsent(t *testing.T) {
	r := NewReader()
	r.readKeychain = func(service string) ([]byte, error) { return []byte("\n"), nil }
	_, ok, err := r.Read(profile.Source{From: profile.SourceKeychain, Service: "svc"})
	if err != nil {
		t.Fatalf("an empty item was an error: %v", err)
	}
	if ok {
		t.Error("an empty item reported a value")
	}
}
