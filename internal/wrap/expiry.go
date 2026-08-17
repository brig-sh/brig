package wrap

import (
	"fmt"
	"time"

	"github.com/brig-sh/brig/internal/secret"
)

// lister is what warnExpiredSecrets and the status report need beyond
// creds.SecretReader: the ability to list every stored secret's provenance.
// SecretReader stays read-only on purpose (see internal/creds/reader.go) --
// it is the seam the run path is allowed to touch -- so a backend that
// cannot list this way simply produces no warning, the same
// zero-value-means-absent contract secret.Secret.Provenance already follows.
type lister interface {
	List() ([]secret.Secret, error)
}

// warnExpiredSecrets says, before boot, that an imported credential has
// expired. It replaces the warning at the old run.go:62, which read
// Profile.HostCredential and goes dead the moment a later PR drops that key
// from the built-in profiles.
//
// Rebuilt from provenance rather than from the value: List reads it with no
// decrypt and raises no keychain dialog, which is the property that lets
// this run before every boot rather than only on demand.
func (c *Config) warnExpiredSecrets() {
	secrets, ok := c.listSecrets()
	if !ok {
		return
	}
	now := nowMilli()
	for _, decl := range c.Profile.Secrets {
		s, found := secrets[decl.Name]
		if !found || s.Provenance.ExpiresAt == 0 || s.Provenance.ExpiresAt >= now {
			// No expiry is not expired: absence is not evidence, the rule
			// HostCredential.Expired already followed. Losing it would warn
			// about every hand-created secret on every run.
			continue
		}
		c.warnf("the imported credential for %s expired %s.", c.Profile.Name, ago(now-s.Provenance.ExpiresAt))
		c.warnf("Renew it on the host, then: brig secret import %s", c.Profile.Name)
	}
}

// listSecrets opens the store and lists it, or reports ok=false for every
// reason this stays quiet about: a profile declaring no secrets (opening the
// store at all would be exactly the keychain prompt this design removes), no
// store on this platform, a store that cannot list, or a listing failure.
// None of those are this call's to report -- the run path already reports
// what it needs to, and this is a courtesy read before or alongside it.
func (c *Config) listSecrets() (map[string]secret.Secret, bool) {
	if len(c.Profile.Secrets) == 0 || c.OpenStore == nil {
		return nil, false
	}
	store, err := c.OpenStore()
	if err != nil {
		// Silent for the same reason resolution is: ErrUnsupported means
		// there is no store on this platform and nothing the user does on
		// this run changes that. Any other failure -- a locked keychain, a
		// denied dialog -- is a courtesy check either way: BuildEnv's own
		// resolution already reports it when a run actually needs the value.
		return nil, false
	}
	l, ok := store.(lister)
	if !ok {
		return nil, false
	}
	list, err := l.List()
	if err != nil {
		return nil, false
	}
	byName := make(map[string]secret.Secret, len(list))
	for _, s := range list {
		byName[s.Name] = s
	}
	return byName, true
}

// ago renders an elapsed duration the coarse way a user reads it -- "3h ago",
// not "10800000ms ago". The warning only needs to say roughly how stale the
// credential is, not to the second.
func ago(elapsedMillis int64) string {
	d := time.Duration(elapsedMillis) * time.Millisecond
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
