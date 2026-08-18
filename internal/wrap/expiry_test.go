package wrap

import (
	"bytes"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/secret"
)

// Rebuilt from provenance rather than from the value: the expiry is an
// attribute, so List reads it without a decrypt and raises no keychain dialog
// -- which is the property that lets this run before every boot.
func TestExpiredImportedCredentialWarnsBeforeBoot(t *testing.T) {
	const now = 1755436980000
	old := nowMilli
	nowMilli = func() int64 { return now }
	t.Cleanup(func() { nowMilli = old })

	var errb bytes.Buffer
	c := &Config{
		Err: &errb,
		Profile: profile.Profile{
			Name:    "claude-code",
			Secrets: []profile.SecretDecl{{Name: "claude-credentials", Required: ptr(false)}},
		},
		OpenStore: func() (creds.SecretReader, error) {
			return listingStore{{
				Name:       "claude-credentials",
				Provenance: secret.Provenance{V: secret.ProvenanceVersion, ExpiresAt: now - 3*60*60*1000},
			}}, nil
		},
	}
	c.warnExpiredSecrets()

	want := "brig: the imported credential claude-credentials (claude-code) expired 3h ago.\n" +
		"brig: Renew it on the host, then: brig secret import claude-code\n"
	if errb.String() != want {
		t.Errorf("warning was:\n%s\nwant:\n%s", errb.String(), want)
	}
}

// A secret carrying no expiry is not expired: absence is not evidence. That is
// the rule HostCredential.Expired already followed, and losing it would warn
// about every hand-created secret on every run.
func TestNoExpiryIsNotExpired(t *testing.T) {
	var errb bytes.Buffer
	c := &Config{
		Err: &errb,
		Profile: profile.Profile{Name: "claude-code",
			Secrets: []profile.SecretDecl{{Name: "claude-credentials", Required: ptr(false)}}},
		OpenStore: func() (creds.SecretReader, error) {
			return listingStore{{Name: "claude-credentials"}}, nil
		},
	}
	c.warnExpiredSecrets()
	if errb.Len() != 0 {
		t.Errorf("warned about a secret with no expiry: %s", errb.String())
	}
}

// A profile with no secrets must not open the store at all: opening it
// unconditionally is exactly the keychain prompt this design removes.
func TestNoSecretsOpensNoStore(t *testing.T) {
	opened := false
	c := &Config{
		Err:     &bytes.Buffer{},
		Profile: profile.Profile{Name: "ubuntu"},
		OpenStore: func() (creds.SecretReader, error) {
			opened = true
			return listingStore{}, nil
		},
	}
	c.warnExpiredSecrets()
	if opened {
		t.Error("the store was opened for a profile that declares no secrets")
	}
}

// Silent on a platform with no store, for the same reason resolution is: there
// is nothing the user can do about it on this run.
func TestNoStoreIsSilent(t *testing.T) {
	var errb bytes.Buffer
	c := &Config{
		Err: &errb,
		Profile: profile.Profile{Name: "claude-code",
			Secrets: []profile.SecretDecl{{Name: "claude-credentials", Required: ptr(false)}}},
		OpenStore: func() (creds.SecretReader, error) { return nil, secret.ErrUnsupported },
	}
	c.warnExpiredSecrets()
	if errb.Len() != 0 {
		t.Errorf("warned on a platform with no store: %s", errb.String())
	}
}
