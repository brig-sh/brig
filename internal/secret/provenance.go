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
//
// The prefix and the base64 decode only prove the comment is *shaped* like
// brig's own -- they are not proof of authorship. keychain_darwin.go's own
// comment on service says why: any process running as this user can add an
// item under brig's namespace, so a decoded From is attacker-controlled
// input, not brig's. It is about to be printed into `brig secret ls` and
// into a warning that tells the user to run a command, so a From that fails
// validFrom is dropped to the zero value here, once, rather than trusted by
// every place that later prints it.
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
	if !validFrom(p.From) {
		// The zero value is what every caller already renders as absent --
		// see Secret.Modified's contract, which Provenance follows too --
		// so a rejected From needs no new handling anywhere else.
		p.From = ""
	}
	return p, true
}

// maxFromLen bounds From well past any real keychain service name or file
// path, and short enough that a decoded comment cannot hand a caller an
// arbitrarily large string to print.
const maxFromLen = 256

// validFrom reports whether a decoded From is safe to print as-is: short
// enough, and holding nothing but letters, digits, and the handful of
// punctuation a keychain service name or a file path legitimately carries.
// Never a control character, never an escape sequence -- From reaches a
// terminal through `brig secret ls` and through a warning that tells the
// user to run a command, which is exactly where a hidden escape sequence
// would do its work.
// SafeFrom renders a locator so that DecodeProvenance will hand it back
// rather than drop it.
//
// The write side has to do this because the read side is deliberately strict
// and silent: a From outside validFrom decodes to "", which reads downstream
// as "brig did not write this". An ordinary locator reaches that state --
// `file:~/Library/Application Support/My,Tool/creds.json` has a comma -- and
// the consequence is not cosmetic: `brig secret ls` shows a dash for a secret
// brig imported, and the next import refuses it as hand-created and demands
// -y, for good. Replacing the few bytes validFrom does not take keeps the
// locator legible and keeps the round trip total.
func SafeFrom(from string) string {
	if len(from) > maxFromLen {
		// The scheme and the leading path components identify the source; the
		// tail is what a person can still recognise once it is this long.
		from = from[:maxFromLen]
	}
	b := []byte(from)
	for i, c := range b {
		if !validFromByte(c) {
			b[i] = '_'
		}
	}
	return string(b)
}

func validFromByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case strings.IndexByte(" -_.:/~", c) >= 0:
		return true
	}
	return false
}

func validFrom(from string) bool {
	if len(from) > maxFromLen {
		return false
	}
	for i := 0; i < len(from); i++ {
		if !validFromByte(from[i]) {
			return false
		}
	}
	return true
}
