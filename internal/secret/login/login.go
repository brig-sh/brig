// Package login stores the access token that `brig login` obtains from a
// NOFire AI deployment.
//
// The token lives in the OS keychain under its own service, sh.brig.login,
// apart from the secrets brig hands to a sandbox. This is the only package
// besides internal/secret that can open a keychain backend, and no run-path
// package of this module imports it (see arch_test.go), so the core's run
// cannot read the token and a profile's `from: keychain` cannot name its
// service. Brig Enterprise reads it through one accessor, and never hands it
// to a sandbox.
package login

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/brig-sh/brig/internal/secret/internal/keyring"
)

// Service is the keychain service of the login tokens.
const Service = "sh.brig.login"

// label is shown for every item in the keychain UI. It is fixed so the label
// never carries the deployment URL or anything derived from the token.
const label = "brig login"

// The errors callers distinguish, the keyring's own.
var (
	ErrNotFound    = keyring.ErrNotFound
	ErrUnsupported = keyring.ErrUnsupported
)

// Token is one deployment's access token. URL is the normalised deployment
// URL; the store does not normalise it.
type Token struct {
	URL         string    `json:"url"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Trust is the trust store the person accepted at brig login: each
	// environment variable that gave Brig one, with its value as an absolute,
	// cleaned path, never one with a .. element. It lives and dies with this
	// item. A record written without it accepts nothing.
	Trust map[string]string `json:"trust,omitempty"`
}

// Store keeps tokens by deployment URL.
type Store interface {
	// Save creates the item, or replaces an existing one.
	Save(t Token) error
	// Load returns the token of url, or ErrNotFound. An item that names
	// another URL is an error.
	Load(url string) (Token, error)
	// Delete removes the token of url, or returns ErrNotFound.
	Delete(url string) error
}

// openKeyring is a variable so tests can supply a fake backend.
var openKeyring = keyring.Open

// Open returns the login store for this host. It returns ErrUnsupported,
// wrapping the cause, when the host has no keychain.
func Open() (Store, error) {
	k, err := openKeyring(keyring.Config{Service: Service, Label: label})
	if err != nil {
		if errors.Is(err, keyring.ErrUnsupported) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", keyring.ErrUnsupported, err)
	}
	return store{k}, nil
}

// Account is the keychain account of a deployment: a hash, so the account
// never holds the URL and is always a name the keyring accepts, whatever the
// URL's length or characters.
func Account(url string) string {
	sum := sha256.Sum256([]byte(url))
	return "r" + hex.EncodeToString(sum[:])
}

type store struct{ k keyring.Store }

func (s store) Save(t Token) error {
	v, err := json.Marshal(t)
	if err != nil {
		return err
	}
	a := Account(t.URL)
	err = s.k.Create(a, v)
	if errors.Is(err, keyring.ErrExists) {
		err = s.k.Update(a, v)
	}
	return err
}

func (s store) Load(url string) (Token, error) {
	v, err := s.k.Read(Account(url))
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(v, &t); err != nil {
		return Token{}, fmt.Errorf("the stored login for %s is unreadable: %w", url, err)
	}
	// The account is a hash of the URL, so an item whose own URL differs was
	// not saved by Save for this deployment: it was planted or corrupted.
	// Returning it would let a later caller that trusts t.URL send the token
	// elsewhere. The error is not ErrNotFound, so logout removes the item.
	if t.URL != url {
		return Token{}, fmt.Errorf("the stored login for %s names another deployment", url)
	}
	return t, nil
}

func (s store) Delete(url string) error { return s.k.Delete(Account(url)) }
