// Package gcpcred fetches Google Cloud access tokens for the credential
// broker.
//
// It runs the gcloud CLI as a subprocess, the way Brig runs cosign and oras,
// so the user's login, configurations and service account impersonation stay
// gcloud's to resolve and Brig links no Google SDK. Only brig-broker and the
// import command link this package. The run path (internal/wrap,
// internal/creds, internal/injector, cmd/brigd) never does: a run delivers
// what the broker holds and does not ask Google for anything.
//
// The tokens it returns live in the caller's memory. No error, log line or
// argv in this package carries a token.
package gcpcred

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/brig-sh/brig/internal/pointerkind"
)

// Mode is how a pointer obtains a token.
type Mode string

const (
	// ModeImpersonate mints a token for a service account, so the sandbox
	// holds that account's access and not the user's.
	ModeImpersonate Mode = "impersonate"
	// ModeSelf prints the user's own token. It is never a default: that
	// token is the user's, not an identity scoped to a sandbox.
	ModeSelf Mode = "self"
)

// Pointer is what a stored secret holds for a gcp route: where to get a
// token, never the token itself.
type Pointer struct {
	Kind           string `json:"kind"` // always "gcloud"
	Configuration  string `json:"configuration"`
	Mode           Mode   `json:"mode"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Account is the gcloud login a self pointer mints as, recorded at
	// import. Without it a later change of the configuration's account would
	// hand the sandbox another identity than the one the import named.
	Account string `json:"account,omitempty"`
}

// A configuration name, a service account and an account are passed to
// gcloud as arguments, so a value that starts with "-" would be read as a
// flag. The pointer comes from a stored secret, which is data and not trusted
// to be well formed. The configuration rule is gcloud's own: a lowercase
// letter first. The account rule admits any address gcloud logs in as and
// nothing that starts with "-" or holds a space.
var (
	configurationRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	serviceAccountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*@[a-z0-9][a-z0-9.-]*\.gserviceaccount\.com$`)
	accountRE        = regexp.MustCompile(`^[A-Za-z0-9._%+][A-Za-z0-9._%+-]*@[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

// IsPointer reports whether doc is a JSON object whose kind is "gcloud",
// whether or not it is otherwise valid, cut short or followed by other data.
// A malformed gcloud document must reach ParsePointer and be refused, never
// be taken for a raw token and sent to the guest.
func IsPointer(doc []byte) bool {
	return pointerkind.Is(doc, "gcloud")
}

// ParsePointer decodes a stored pointer strictly and validates it.
func ParsePointer(doc []byte) (Pointer, error) {
	var p Pointer
	dec := json.NewDecoder(bytes.NewReader(pointerkind.Trim(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Pointer{}, fmt.Errorf("the gcloud pointer is not valid JSON of the expected shape: %w", err)
	}
	if dec.More() {
		return Pointer{}, errors.New("the gcloud pointer has data after the JSON document")
	}
	if err := p.Validate(); err != nil {
		return Pointer{}, err
	}
	return p, nil
}

// Validate reports why p cannot be used, if it cannot.
func (p Pointer) Validate() error {
	if p.Kind != "gcloud" {
		return fmt.Errorf("the pointer kind is %q, not \"gcloud\"", p.Kind)
	}
	if !configurationRE.MatchString(p.Configuration) {
		return fmt.Errorf("the gcloud configuration name %q is not usable: use a lowercase letter first, then lowercase letters, digits and -", p.Configuration)
	}
	switch p.Mode {
	case ModeImpersonate:
		if !serviceAccountRE.MatchString(p.ServiceAccount) {
			return fmt.Errorf("mode impersonate needs a serviceAccount that is a *.gserviceaccount.com address, got %q", p.ServiceAccount)
		}
		// The service account is already on argv and checked against
		// tokeninfo; an account here would be a second, unused answer.
		if p.Account != "" {
			return errors.New("mode impersonate takes no account")
		}
	case ModeSelf:
		if p.ServiceAccount != "" {
			return errors.New("mode self takes no serviceAccount")
		}
		if !accountRE.MatchString(p.Account) {
			return fmt.Errorf("mode self needs the account of the gcloud login, an email address, got %q", p.Account)
		}
	default:
		return fmt.Errorf("the pointer mode %q is not impersonate or self", p.Mode)
	}
	return nil
}
