package secret

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// ProvenanceVersion is the schema version of the comment document. It is
// written so that a later brig reading an older item knows what it is holding
// rather than inferring it from which fields happen to be present.
const ProvenanceVersion = 1

// provenancePrefix marks a comment attribute as brig's own.
//
// The comment attribute is not brig's private space: it is one string on an
// item in a namespace any process running as this user can write to. The
// prefix is what lets DecodeProvenance answer "not mine" instead of guessing,
// which matters because the expiry it carries drives a warning the user acts
// on.
const provenancePrefix = "brig1:"

// Provenance is what brig knows about a stored secret without decrypting it:
// where it came from and when it expires.
//
// It exists because sources are read only on demand, so the stored copy drifts
// from both directions -- the host renews and brig never re-reads it, while
// the guest refreshes onto tmpfs and loses it at shutdown. Nothing else in the
// system would notice.
//
// The zero value means absent, which is the contract Secret.Modified already
// documents: a backend that cannot supply it returns the zero value and
// callers render it as missing rather than inventing one.
type Provenance struct {
	V int `json:"v"`
	// From is the source locator, e.g. "keychain:Claude Code-credentials" or
	// "file:~/.claude/.credentials.json". Empty for a hand-created secret,
	// which is how `import` tells one it wrote from one it must not replace
	// without -y.
	From string `json:"from,omitempty"`
	// ExpiresAt is epoch milliseconds, or zero when the source carried none.
	// Absence is not evidence of expiry.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

func (p Provenance) IsZero() bool { return p == Provenance{} }

// Encode renders provenance for the keychain comment attribute.
//
// Base64url of the JSON rather than the JSON itself, and this is a deliberate
// departure from the design document. The comment rides on the same
// security(1) interactive line as the write, and that line is tokenized with
// quote handling -- so a raw {"v":1,...} would need its quotes escaped, on a
// path whose failure mode (a silently truncated or mis-parsed line) is exactly
// the one the size pre-check exists to prevent elsewhere. Base64url spells
// everything in letters, digits, - and _, so nothing on the line needs
// quoting. The cost is that Keychain Access shows an opaque string; the gain
// is that the write cannot be broken by a service name with a quote in it.
func (p Provenance) Encode() (string, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return provenancePrefix + base64.RawURLEncoding.EncodeToString(blob), nil
}

// DecodeProvenance reads a comment attribute back, reporting false for
// anything brig did not write.
func DecodeProvenance(comment string) (Provenance, bool) {
	rest, ok := strings.CutPrefix(comment, provenancePrefix)
	if !ok {
		return Provenance{}, false
	}
	blob, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return Provenance{}, false
	}
	var p Provenance
	if err := json.Unmarshal(blob, &p); err != nil {
		return Provenance{}, false
	}
	return p, true
}
