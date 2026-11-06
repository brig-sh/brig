// Package redact masks credentials in text by their shape.
//
// It exists for text that leaves the host: a diagnostic report a user
// attaches to a public issue. It knows no stored value and reads no secret
// store. Masking the exact values Brig holds would also catch a token of a
// shape listed nowhere here, but it would give the caller a path to every
// credential on the host, and a diagnostic command is the last place that
// path belongs. A token of an unknown shape therefore passes through, and the
// caller tells the reader to check the text before sharing it.
package redact

import (
	"regexp"
	"strings"
)

// mask is the text that replaces a value of the given kind.
func mask(kind string) string { return "[masked: " + kind + "]" }

// A value already masked starts with "[", so the name=value and JSON rules,
// which run last and accept almost any value, leave it alone.
type rule struct {
	kind string
	re   *regexp.Regexp
	// keep is how many leading submatches stay as they were: the variable
	// name or JSON key, so a reader can tell which one held the value.
	keep int
}

// The order matters. The whole-token shapes run first, so a JWT in an
// Authorization header is reported as a JWT, and the rules that keep a name
// run last, after every value they could swallow is already masked.
var rules = []rule{
	{kind: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// Brig's broker token: base64url(JSON claims) "." base64url(HMAC-SHA256),
	// and the signature is always 43 characters. The trailing class stops the
	// match from ending inside a longer dotted word.
	{kind: "brig-token", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{43}(?:[^A-Za-z0-9_.-]|$)`)},
	{kind: "anthropic-key", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`)},
	{kind: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{kind: "aws-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	// A credential is 16 characters or more, so prose such as "the Basic
	// challenge" and a header that says "Authorization: none" survive.
	{kind: "authorization", re: regexp.MustCompile(`(?i)(authorization:\s*(?:(?:bearer|basic|token)\s+)?)([A-Za-z0-9._~+/=-]{16,})`), keep: 1},
	{kind: "authorization", re: regexp.MustCompile(`(?i)(\b(?:bearer|basic)\s+)([A-Za-z0-9._~+/=-]{16,})`), keep: 1},
	{kind: "json-secret", re: regexp.MustCompile(`("(?:accessToken|refreshToken|idToken|access_token|refresh_token|id_token|client_secret|apiKey|api_key|password|secret|token)"\s*:\s*)"[^"\[][^"]*"`), keep: 1},
	{kind: "env-secret", re: regexp.MustCompile(`\b([A-Z][A-Z0-9_]*(?:TOKEN|KEY|SECRET|PASSWORD|PASSWD)=)[^\s'"\[][^\s'"]*`), keep: 1},
}

// String returns s with each credential-shaped value replaced by
// "[masked: <kind>]".
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			return r.replace(m)
		})
	}
	return s
}

func (r rule) replace(m string) string {
	sub := r.re.FindStringSubmatch(m)
	var b strings.Builder
	for i := 1; i <= r.keep && i < len(sub); i++ {
		b.WriteString(sub[i])
	}
	masked := mask(r.kind)
	if r.kind == "json-secret" {
		masked = `"` + masked + `"`
	}
	b.WriteString(masked)
	// brig-token matches one character past the token to find its end. Give
	// that character back.
	if r.kind == "brig-token" {
		if last := m[len(m)-1]; !isTokenByte(last) {
			b.WriteByte(last)
		}
	}
	return b.String()
}

func isTokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}
